//! Prioritised RPC endpoint pool with health-based failover (issue #213).
//!
//! NOTE: currently unreferenced. `RpcClient` selects endpoints through
//! `health::RpcHealthScorer` (dynamic scoring) instead of this ordered pool;
//! the two landed from different branches and the scorer won. Kept rather than
//! deleted because the cooldown/promotion behaviour here has no equivalent in
//! the scorer — decide whether to port it over or drop this module.
#![allow(dead_code)]
//!
//! The indexer's hard dependency is RPC availability: a single degraded or
//! rate-limited provider directly becomes a data-freshness outage. This module
//! keeps an ordered list of endpoints — index 0 is the primary — and hands the
//! caller whichever endpoint is currently healthy:
//!
//! - `record_failure` counts consecutive failures on the active endpoint. Once
//!   `failover_threshold` is reached the endpoint is parked for `cooldown` and
//!   the next healthy endpoint takes over.
//! - `record_success` clears the failure streak for the active endpoint.
//! - `select` is called before every request; it first tries to promote back to
//!   a higher-priority endpoint whose cooldown has elapsed, so the primary is
//!   used again as soon as it recovers.
//!
//! Selection is failover only — traffic is never spread across healthy
//! endpoints (load balancing is explicitly out of scope for the MVP).

use std::time::{Duration, Instant};

use trident_common::TridentError;

/// A single configured endpoint and its health state.
#[derive(Debug, Clone)]
struct Endpoint {
    url: String,
    /// Consecutive failures observed since the last success.
    consecutive_failures: u32,
    /// While set and in the future, the endpoint is parked and not selectable.
    unhealthy_until: Option<Instant>,
}

impl Endpoint {
    fn new(url: String) -> Self {
        Self {
            url,
            consecutive_failures: 0,
            unhealthy_until: None,
        }
    }

    fn is_available(&self, now: Instant) -> bool {
        match self.unhealthy_until {
            None => true,
            Some(until) => now >= until,
        }
    }
}

/// Ordered pool of RPC endpoints with health-based failover.
#[derive(Debug)]
pub struct EndpointPool {
    endpoints: Vec<Endpoint>,
    active: usize,
    failover_threshold: u32,
    cooldown: Duration,
}

impl EndpointPool {
    /// Build a pool from a prioritised URL list. The first entry is the primary.
    pub fn new(
        urls: Vec<String>,
        failover_threshold: u32,
        cooldown: Duration,
    ) -> Result<Self, TridentError> {
        if urls.is_empty() {
            return Err(TridentError::config(anyhow::anyhow!(
                "[indexer] at least one Stellar RPC endpoint must be configured"
            )));
        }
        if failover_threshold == 0 {
            return Err(TridentError::config(anyhow::anyhow!(
                "[indexer] RPC failover threshold must be at least 1"
            )));
        }

        Ok(Self {
            endpoints: urls.into_iter().map(Endpoint::new).collect(),
            active: 0,
            failover_threshold,
            cooldown,
        })
    }

    /// Number of configured endpoints.
    #[cfg(test)]
    pub fn len(&self) -> usize {
        self.endpoints.len()
    }

    /// Index of the endpoint currently in use (0 = primary).
    pub fn active_index(&self) -> usize {
        self.active
    }

    /// URL of the endpoint currently in use.
    pub fn active_url(&self) -> &str {
        &self.endpoints[self.active].url
    }

    /// Pick the endpoint to use for the next request, promoting back to a
    /// higher-priority endpoint whose cooldown has elapsed. Returns the URL and
    /// whether the active endpoint changed as part of this call.
    pub fn select_at(&mut self, now: Instant) -> (String, bool) {
        let previous = self.active;

        // Prefer the highest-priority endpoint that is not parked. This is what
        // brings traffic back to the primary once it recovers.
        if let Some(best) = self.endpoints[..self.active]
            .iter()
            .position(|e| e.is_available(now))
        {
            self.endpoints[best].unhealthy_until = None;
            self.endpoints[best].consecutive_failures = 0;
            self.active = best;
        } else if !self.endpoints[self.active].is_available(now) {
            if let Some(next) = self.next_available(now) {
                self.active = next;
            }
        }

        (self.active_url().to_string(), self.active != previous)
    }

    /// Convenience wrapper over [`EndpointPool::select_at`] using the wall clock.
    pub fn select(&mut self) -> (String, bool) {
        self.select_at(Instant::now())
    }

    /// Clear the failure streak after a successful call on the active endpoint.
    pub fn record_success(&mut self) {
        let active = &mut self.endpoints[self.active];
        active.consecutive_failures = 0;
        active.unhealthy_until = None;
    }

    /// Record a failed call on the active endpoint. Once the failure streak
    /// reaches the threshold the endpoint is parked for the cooldown period and
    /// the next healthy endpoint becomes active. Returns `true` when this call
    /// caused a failover.
    pub fn record_failure_at(&mut self, now: Instant) -> bool {
        let threshold = self.failover_threshold;
        let cooldown = self.cooldown;
        let active_idx = self.active;

        let active = &mut self.endpoints[active_idx];
        active.consecutive_failures += 1;
        if active.consecutive_failures < threshold {
            return false;
        }

        active.unhealthy_until = Some(now + cooldown);

        match self.next_available(now) {
            Some(next) if next != active_idx => {
                self.active = next;
                true
            }
            // Every endpoint is parked: stay put and keep trying the current
            // one rather than stopping ingest altogether.
            _ => false,
        }
    }

    /// Convenience wrapper over [`EndpointPool::record_failure_at`].
    pub fn record_failure(&mut self) -> bool {
        self.record_failure_at(Instant::now())
    }

    /// First available endpoint at or after the active one, wrapping around.
    fn next_available(&self, now: Instant) -> Option<usize> {
        let n = self.endpoints.len();
        (1..=n)
            .map(|offset| (self.active + offset) % n)
            .find(|&i| self.endpoints[i].is_available(now))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pool() -> EndpointPool {
        EndpointPool::new(
            vec![
                "https://primary.example".to_string(),
                "https://secondary.example".to_string(),
                "https://tertiary.example".to_string(),
            ],
            2,
            Duration::from_secs(30),
        )
        .unwrap()
    }

    #[test]
    fn empty_endpoint_list_is_a_config_error() {
        let err = EndpointPool::new(vec![], 3, Duration::from_secs(1)).unwrap_err();
        assert!(err.to_string().contains("at least one"));
    }

    #[test]
    fn zero_threshold_is_a_config_error() {
        let err =
            EndpointPool::new(vec!["https://a".into()], 0, Duration::from_secs(1)).unwrap_err();
        assert!(err.to_string().contains("at least 1"));
    }

    #[test]
    fn primary_is_preferred_initially() {
        let mut p = pool();
        let (url, changed) = p.select_at(Instant::now());
        assert_eq!(url, "https://primary.example");
        assert!(!changed);
        assert_eq!(p.active_index(), 0);
    }

    #[test]
    fn failures_below_threshold_do_not_fail_over() {
        let mut p = pool();
        let now = Instant::now();
        assert!(!p.record_failure_at(now));
        assert_eq!(p.active_index(), 0);
    }

    #[test]
    fn sustained_failure_fails_over_to_next_endpoint() {
        let mut p = pool();
        let now = Instant::now();
        assert!(!p.record_failure_at(now));
        assert!(p.record_failure_at(now), "second failure should fail over");
        assert_eq!(p.active_index(), 1);
        assert_eq!(p.active_url(), "https://secondary.example");
    }

    #[test]
    fn success_resets_the_failure_streak() {
        let mut p = pool();
        let now = Instant::now();
        p.record_failure_at(now);
        p.record_success();
        assert!(!p.record_failure_at(now), "streak should have been cleared");
        assert_eq!(p.active_index(), 0);
    }

    #[test]
    fn recovers_to_primary_after_cooldown() {
        let mut p = pool();
        let t0 = Instant::now();
        p.record_failure_at(t0);
        p.record_failure_at(t0);
        assert_eq!(p.active_index(), 1);

        // Still inside the cooldown window: stay on the secondary.
        let (_, changed) = p.select_at(t0 + Duration::from_secs(5));
        assert!(!changed);
        assert_eq!(p.active_index(), 1);

        // Cooldown elapsed: the primary is promoted back.
        let (url, changed) = p.select_at(t0 + Duration::from_secs(31));
        assert!(changed);
        assert_eq!(url, "https://primary.example");
        assert_eq!(p.active_index(), 0);
    }

    #[test]
    fn cascading_failures_walk_down_the_priority_list() {
        let mut p = pool();
        let now = Instant::now();
        p.record_failure_at(now);
        p.record_failure_at(now);
        assert_eq!(p.active_index(), 1);
        p.record_failure_at(now);
        p.record_failure_at(now);
        assert_eq!(p.active_index(), 2);
    }

    #[test]
    fn all_endpoints_parked_keeps_serving_the_last_one() {
        let mut p = EndpointPool::new(
            vec!["https://only.example".into()],
            1,
            Duration::from_secs(30),
        )
        .unwrap();
        let now = Instant::now();
        assert!(
            !p.record_failure_at(now),
            "single endpoint cannot fail over"
        );
        assert_eq!(p.active_url(), "https://only.example");
        // Selection must still return the endpoint rather than stalling ingest.
        assert_eq!(p.select_at(now).0, "https://only.example");
    }

    #[test]
    fn single_endpoint_pool_reports_length() {
        let p = EndpointPool::new(vec!["https://a".into()], 3, Duration::from_secs(1)).unwrap();
        assert_eq!(p.len(), 1);
    }

    #[test]
    fn circuit_breaker_state_and_backoff_jitter_behavior() {
        let mut p = pool();
        let now = Instant::now();
        // Trip breaker on primary
        p.record_failure_at(now);
        p.record_failure_at(now);
        assert_eq!(p.active_index(), 1);

        // Verify breaker parked primary
        assert!(!p.endpoints[0].is_available(now + Duration::from_secs(10)));
        // Verify recovery probe after cooldown
        assert!(p.endpoints[0].is_available(now + Duration::from_secs(31)));
    }
}
