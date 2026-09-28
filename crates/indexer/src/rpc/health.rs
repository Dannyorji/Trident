//! RPC endpoint health scoring system (multi-RPC failover).
//!
//! Maintains a dynamic score (0–100) per endpoint based on observed behavior.
//! Higher scores indicate healthier endpoints. The scorer routes traffic to the
//! healthiest endpoint, with automatic failover and recovery.
//!
//! ## Score rules
//!
//! **Deductions:**
//! - Timeout on HTTP request: -20
//! - Non-200 HTTP response: -15
//! - Stale ledger (tip not advanced in 30s vs other endpoints): -10
//! - JSON-RPC error response: -10
//! - Connection refused: -30
//! - Rate limited (HTTP 429): -5, a shorter penalty than a generic non-200,
//!   since a busy endpoint recovers on its own and shouldn't be scored like a
//!   genuinely broken one (issue #656).
//!
//! **Recovery:**
//! - Successful response: +5 (capped at 100)
//!
//! ## Routing
//!
//! Always prefer the endpoint with the highest current score. If multiple
//! endpoints have the same score, prefer the one that was most recently
//! successful (least recently updated in the recovery direction).
//!
//! ## Persistence
//!
//! Scores are in-process only and reset to 100 on restart. This ensures a
//! restarted indexer gives all endpoints a fair chance rather than inheriting
//! stale bad scores.

use std::collections::HashMap;
use std::sync::RwLock;
use std::time::{Duration, Instant};

use trident_common::TridentError;

use crate::metrics;

/// Initial score for all endpoints on startup.
const INITIAL_SCORE: u8 = 100;

/// Score below which all endpoints are considered critically degraded.
const ALL_DEGRADED_THRESHOLD: u8 = 20;

/// Maximum score cap.
const MAX_SCORE: u8 = 100;

/// Minimum score floor.
const MIN_SCORE: u8 = 0;

/// Score deduction amounts.
const DEDUCT_TIMEOUT: u8 = 20;
const DEDUCT_NON_200: u8 = 15;
const DEDUCT_STALE_LEDGER: u8 = 10;
const DEDUCT_RPC_ERROR: u8 = 10;
const DEDUCT_CONNECTION_REFUSED: u8 = 30;
const DEDUCT_RATE_LIMIT: u8 = 5;

/// Score recovery amount on success.
const RECOVER_SUCCESS: u8 = 5;

/// Threshold for considering a ledger stale (30 seconds).
const STALE_LEDGER_THRESHOLD: Duration = Duration::from_secs(30);

/// Health state for a single endpoint.
#[derive(Debug, Clone)]
struct EndpointHealth {
    score: u8,
    last_success: Option<Instant>,
    last_ledger: Option<u64>,
    last_ledger_timestamp: Option<Instant>,
}

impl EndpointHealth {
    fn new() -> Self {
        Self {
            score: INITIAL_SCORE,
            last_success: None,
            last_ledger: None,
            last_ledger_timestamp: None,
        }
    }

    fn apply_deduction(&mut self, amount: u8) {
        // MIN_SCORE is 0 and `score` is a u8, so saturating_sub already floors
        // here — unlike apply_recovery, where MAX_SCORE is below u8::MAX and
        // the clamp does real work.
        self.score = self.score.saturating_sub(amount);
    }

    fn apply_recovery(&mut self) {
        self.score = self.score.saturating_add(RECOVER_SUCCESS).min(MAX_SCORE);
        self.last_success = Some(Instant::now());
    }

    fn update_ledger(&mut self, ledger: u64) {
        self.last_ledger = Some(ledger);
        self.last_ledger_timestamp = Some(Instant::now());
    }

    #[allow(dead_code)]
    fn set_score(&mut self, score: u8) {
        self.score = score;
    }
}

/// RPC endpoint health scorer.
///
/// Maintains per-endpoint scores and provides routing logic to select the
/// healthiest endpoint for each request.
#[derive(Debug)]
pub struct RpcHealthScorer {
    endpoints: RwLock<HashMap<String, EndpointHealth>>,
    scores: RwLock<HashMap<String, u8>>,
    /// Configured endpoints in priority order; index 0 is the primary. The
    /// maps above are keyed by URL and so have no stable ordering, which would
    /// otherwise make an all-equal selection depend on hash order.
    priority: Vec<String>,
}

impl RpcHealthScorer {
    /// Create a new health scorer with the given endpoint URLs.
    ///
    /// All endpoints start with a score of 100.
    pub fn new(urls: Vec<String>) -> Result<Self, TridentError> {
        if urls.is_empty() {
            return Err(TridentError::config(anyhow::anyhow!(
                "[indexer] at least one RPC endpoint must be configured for health scoring"
            )));
        }

        let mut endpoints = HashMap::new();
        let mut scores = HashMap::new();

        for url in &urls {
            endpoints.insert(url.clone(), EndpointHealth::new());
            scores.insert(url.clone(), INITIAL_SCORE);
        }

        Ok(Self {
            endpoints: RwLock::new(endpoints),
            scores: RwLock::new(scores),
            priority: urls,
        })
    }

    /// Record a successful RPC response from the given endpoint.
    ///
    /// Increases the endpoint's score by 5 (capped at 100) and records the
    /// current ledger if provided.
    pub fn record_success(&self, url: &str, ledger: Option<u64>) {
        if let Ok(mut endpoints) = self.endpoints.write() {
            if let Some(health) = endpoints.get_mut(url) {
                health.apply_recovery();
                if let Some(ledger) = ledger {
                    health.update_ledger(ledger);
                }

                // Update the read-optimized scores map and publish metric
                if let Ok(mut scores) = self.scores.write() {
                    scores.insert(url.to_string(), health.score);
                    metrics::set_rpc_health_score(url, health.score);
                }
            }
        }
    }

    /// Record a timeout error from the given endpoint.
    ///
    /// Deducts 20 points from the endpoint's score.
    pub fn record_timeout(&self, url: &str) {
        self.apply_deduction(url, DEDUCT_TIMEOUT);
    }

    /// Record a non-200 HTTP response from the given endpoint.
    ///
    /// Deducts 15 points from the endpoint's score.
    pub fn record_non_200(&self, url: &str) {
        self.apply_deduction(url, DEDUCT_NON_200);
    }

    /// Record a JSON-RPC error response from the given endpoint.
    ///
    /// Deducts 10 points from the endpoint's score.
    pub fn record_rpc_error(&self, url: &str) {
        self.apply_deduction(url, DEDUCT_RPC_ERROR);
    }

    /// Record a connection refused error from the given endpoint.
    ///
    /// Deducts 30 points from the endpoint's score.
    pub fn record_connection_refused(&self, url: &str) {
        self.apply_deduction(url, DEDUCT_CONNECTION_REFUSED);
    }

    /// Record a rate-limit (HTTP 429) response from the given endpoint.
    ///
    /// Deducts only 5 points — a busy endpoint is not a broken one, and a
    /// full non-200 deduction would fail traffic over to a backup equally
    /// likely to be rate-limited too, causing correlated thrashing (issue
    /// #656).
    pub fn record_rate_limited(&self, url: &str) {
        self.apply_deduction(url, DEDUCT_RATE_LIMIT);
    }

    /// Check if the given endpoint has a stale ledger compared to others.
    ///
    /// An endpoint is considered stale if its latest ledger hasn't advanced
    /// in 30 seconds compared to another endpoint's response.
    ///
    /// Returns true if the endpoint is stale and deducts 10 points.
    pub fn check_and_record_stale(&self, url: &str) -> bool {
        let endpoints = self.endpoints.read().expect("endpoints lock poisoned");

        let health = match endpoints.get(url) {
            Some(h) => h,
            None => return false,
        };

        let (current_ledger, current_timestamp) =
            match (health.last_ledger, health.last_ledger_timestamp) {
                (Some(l), Some(t)) => (l, t),
                _ => return false,
            };

        let now = Instant::now();

        // Check if this endpoint is stale compared to others
        for (other_url, other_health) in endpoints.iter() {
            if other_url == url {
                continue;
            }

            if let (Some(other_ledger), Some(other_timestamp)) =
                (other_health.last_ledger, other_health.last_ledger_timestamp)
            {
                // If the other endpoint has a more recent ledger and this one hasn't advanced
                if other_ledger > current_ledger {
                    let time_since_other = now.saturating_duration_since(other_timestamp);
                    let time_since_current = now.saturating_duration_since(current_timestamp);

                    // If the other endpoint's data is recent enough and this one is behind
                    if time_since_other < STALE_LEDGER_THRESHOLD
                        && time_since_current > STALE_LEDGER_THRESHOLD
                    {
                        self.apply_deduction(url, DEDUCT_STALE_LEDGER);
                        return true;
                    }
                }
            }
        }

        false
    }

    /// Select the healthiest endpoint for the next request.
    ///
    /// Returns the URL of the endpoint with the highest score. If multiple
    /// endpoints have the same score, prefers the one most recently successful.
    pub fn select_best_endpoint(&self) -> String {
        let scores = self.scores.read().expect("scores lock poisoned");
        let endpoints = self.endpoints.read().expect("endpoints lock poisoned");

        let mut best_url: Option<&String> = None;
        let mut best_score = MIN_SCORE;
        let mut best_last_success: Option<Instant> = None;

        // Walk in configured priority order so that when everything else ties
        // the earlier-listed endpoint keeps serving. Iterating the HashMap
        // instead would make the choice depend on hash order.
        for url in &self.priority {
            let Some(health) = endpoints.get(url) else {
                continue;
            };
            let score = scores.get(url).copied().unwrap_or(INITIAL_SCORE);
            let last_success = health.last_success;

            // Highest score wins outright; on a tie the more recently
            // successful endpoint wins, and an endpoint that has ever
            // succeeded beats one that never has. A pure tie leaves the
            // incumbent in place, which is what makes priority order decide.
            let better = match best_url {
                None => true,
                Some(_) if score != best_score => score > best_score,
                Some(_) => match (last_success, best_last_success) {
                    (Some(current), Some(best)) => current > best,
                    (Some(_), None) => true,
                    (None, _) => false,
                },
            };

            if better {
                best_url = Some(url);
                best_score = score;
                best_last_success = last_success;
            }
        }

        best_url.expect("at least one endpoint").clone()
    }

    /// Get the current score for a specific endpoint.
    #[allow(dead_code)]
    pub fn get_score(&self, url: &str) -> u8 {
        let scores = self.scores.read().expect("scores lock poisoned");
        scores.get(url).copied().unwrap_or(INITIAL_SCORE)
    }

    /// Get all current scores.
    ///
    /// Returns a map of endpoint URLs to their current scores.
    #[allow(dead_code)]
    pub fn get_all_scores(&self) -> HashMap<String, u8> {
        let scores = self.scores.read().expect("scores lock poisoned");
        scores.clone()
    }

    /// Check if all endpoints are critically degraded (score < 20).
    ///
    /// Returns true if every endpoint has a score below the threshold.
    pub fn all_degraded(&self) -> bool {
        let scores = self.scores.read().expect("scores lock poisoned");
        scores.values().all(|&score| score < ALL_DEGRADED_THRESHOLD)
    }

    /// Apply a score deduction to the given endpoint.
    fn apply_deduction(&self, url: &str, amount: u8) {
        if let Ok(mut endpoints) = self.endpoints.write() {
            if let Some(health) = endpoints.get_mut(url) {
                health.apply_deduction(amount);

                // Update the read-optimized scores map and publish metric
                if let Ok(mut scores) = self.scores.write() {
                    scores.insert(url.to_string(), health.score);
                    metrics::set_rpc_health_score(url, health.score);
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scorer() -> RpcHealthScorer {
        RpcHealthScorer::new(vec![
            "https://primary.example".to_string(),
            "https://backup.example".to_string(),
        ])
        .unwrap()
    }

    #[test]
    fn empty_endpoint_list_is_error() {
        let err = RpcHealthScorer::new(vec![]).unwrap_err();
        assert!(err.to_string().contains("at least one"));
    }

    #[test]
    fn initial_score_is_100() {
        let s = scorer();
        assert_eq!(s.get_score("https://primary.example"), 100);
        assert_eq!(s.get_score("https://backup.example"), 100);
    }

    #[test]
    fn success_increases_score_capped_at_100() {
        let s = scorer();
        s.record_success("https://primary.example", Some(100));
        assert_eq!(s.get_score("https://primary.example"), 100); // Already at max
    }

    #[test]
    fn timeout_deducts_20() {
        let s = scorer();
        s.record_timeout("https://primary.example");
        assert_eq!(s.get_score("https://primary.example"), 80);
    }

    #[test]
    fn non_200_deducts_15() {
        let s = scorer();
        s.record_non_200("https://primary.example");
        assert_eq!(s.get_score("https://primary.example"), 85);
    }

    #[test]
    fn rpc_error_deducts_10() {
        let s = scorer();
        s.record_rpc_error("https://primary.example");
        assert_eq!(s.get_score("https://primary.example"), 90);
    }

    #[test]
    fn connection_refused_deducts_30() {
        let s = scorer();
        s.record_connection_refused("https://primary.example");
        assert_eq!(s.get_score("https://primary.example"), 70);
    }

    #[test]
    fn rate_limited_deducts_5() {
        let s = scorer();
        s.record_rate_limited("https://primary.example");
        assert_eq!(s.get_score("https://primary.example"), 95);
    }

    #[test]
    fn rate_limit_penalty_is_smaller_than_a_hard_failure() {
        let rate_limited = scorer();
        rate_limited.record_rate_limited("https://primary.example");

        let hard_failure = scorer();
        hard_failure.record_non_200("https://primary.example");

        assert!(
            rate_limited.get_score("https://primary.example")
                > hard_failure.get_score("https://primary.example"),
            "a 429 must not be scored as severely as a generic 5xx"
        );
    }

    #[test]
    fn rate_limiting_does_not_induce_cross_endpoint_failover() {
        // A single rate-limited response must not be enough to push traffic
        // to a backup that could just as easily be rate-limited itself.
        let s = scorer();
        s.record_rate_limited("https://primary.example");
        assert_eq!(
            s.select_best_endpoint(),
            "https://primary.example",
            "one 429 should not trigger failover"
        );
    }

    #[test]
    fn recovery_adds_5() {
        let s = scorer();
        s.record_timeout("https://primary.example");
        assert_eq!(s.get_score("https://primary.example"), 80);

        s.record_success("https://primary.example", Some(100));
        assert_eq!(s.get_score("https://primary.example"), 85);
    }

    #[test]
    fn score_floor_is_0() {
        let s = scorer();
        for _ in 0..10 {
            s.record_connection_refused("https://primary.example");
        }
        assert_eq!(s.get_score("https://primary.example"), 0);
    }

    #[test]
    fn select_best_chooses_highest_score() {
        let s = scorer();
        s.record_timeout("https://primary.example");
        assert_eq!(s.select_best_endpoint(), "https://backup.example");
    }

    #[test]
    fn select_best_prefers_recently_successful_on_tie() {
        let s = scorer();
        s.record_success("https://primary.example", Some(100));
        // Both at 100, but primary was more recently successful
        assert_eq!(s.select_best_endpoint(), "https://primary.example");
    }

    #[test]
    fn all_degraded_true_when_all_below_20() {
        let s = scorer();
        for _ in 0..5 {
            s.record_connection_refused("https://primary.example");
            s.record_connection_refused("https://backup.example");
        }
        assert!(s.all_degraded());
    }

    #[test]
    fn all_degraded_false_when_any_above_20() {
        let s = scorer();
        s.record_connection_refused("https://primary.example");
        s.record_connection_refused("https://primary.example");
        // Primary at 40, backup at 100
        assert!(!s.all_degraded());
    }

    #[test]
    fn ledger_tracking_works() {
        let s = scorer();
        s.record_success("https://primary.example", Some(100));
        s.record_success("https://backup.example", Some(95));

        // Verify scores are updated
        assert_eq!(s.get_score("https://primary.example"), 100);
        assert_eq!(s.get_score("https://backup.example"), 100);
    }

    #[test]
    fn stale_detection_requires_time_difference() {
        let s = scorer();
        // Both endpoints report the same ledger - no stale detection
        s.record_success("https://primary.example", Some(100));
        s.record_success("https://backup.example", Some(100));

        assert!(!s.check_and_record_stale("https://primary.example"));
        assert_eq!(s.get_score("https://primary.example"), 100);
    }

    #[test]
    fn multiple_deductions_accumulate() {
        let s = scorer();
        s.record_timeout("https://primary.example"); // -20 -> 80
        s.record_non_200("https://primary.example"); // -15 -> 65
        s.record_rpc_error("https://primary.example"); // -10 -> 55

        assert_eq!(s.get_score("https://primary.example"), 55);
    }

    #[test]
    fn recovery_eventually_restores_to_100() {
        let s = scorer();
        s.record_connection_refused("https://primary.example"); // 70

        for _ in 0..10 {
            s.record_success("https://primary.example", Some(100));
        }

        assert_eq!(s.get_score("https://primary.example"), 100);
    }

    #[test]
    fn three_endpoints_routing() {
        let s = RpcHealthScorer::new(vec![
            "https://primary.example".to_string(),
            "https://backup1.example".to_string(),
            "https://backup2.example".to_string(),
        ])
        .unwrap();

        // Primary degraded, backup1 degraded, backup2 healthy
        s.record_connection_refused("https://primary.example"); // 70
        s.record_connection_refused("https://backup1.example"); // 70
        s.record_connection_refused("https://backup1.example"); // 40

        assert_eq!(s.select_best_endpoint(), "https://backup2.example");
    }

    #[test]
    fn unknown_endpoint_noops() {
        let s = scorer();
        // These should not panic
        s.record_timeout("https://unknown.example");
        s.record_success("https://unknown.example", Some(100));
        assert_eq!(s.get_score("https://unknown.example"), 100); // Default
    }
}
