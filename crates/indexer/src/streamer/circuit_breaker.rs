//! Circuit breaker for sustained Stellar RPC outages (issue #197).
//!
//! `poll_once` already retries a single `getEvents` call up to 5 times with
//! exponential backoff. That is fine for a blip, but when the RPC endpoint is
//! genuinely down, every poll interval still burns through all 5 retries —
//! logging, allocating, and hammering a provider that has already told us
//! (via 5 consecutive failures) that it is not going to answer. The breaker
//! sits one level up, around the whole poll cycle: after enough consecutive
//! poll failures it opens and the run loop skips calling `poll_once` entirely
//! for a cooldown period, sleeping instead.
//!
//! State machine (textbook Closed → Open → HalfOpen):
//!
//! ```text
//!            failure_threshold consecutive failures
//!   Closed ────────────────────────────────────────▶ Open
//!     ▲                                                │
//!     │ success                                        │ cooldown elapses
//!     │                                                 ▼
//!     └──────────────────────────────────────────  HalfOpen
//!                         failure (back to Open,
//!                         cooldown restarts)
//! ```
//!
//! In `HalfOpen`, exactly one probe call is allowed through; a success closes
//! the breaker and resets the failure count, a failure reopens it and resets
//! the cooldown clock.

use std::time::{Duration, Instant};

/// Circuit breaker state, exported as `trident_indexer_rpc_breaker_state`
/// (0 = Closed, 1 = Open, 2 = HalfOpen).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BreakerState {
    /// Normal operation: every poll is attempted.
    Closed,
    /// Tripped: polls are skipped until `opened_at + cooldown` elapses.
    Open,
    /// Cooldown elapsed: the next poll is let through as a probe.
    HalfOpen,
}

impl BreakerState {
    pub fn as_metric_value(self) -> f64 {
        match self {
            BreakerState::Closed => 0.0,
            BreakerState::Open => 1.0,
            BreakerState::HalfOpen => 2.0,
        }
    }
}

/// Thresholds controlling when the breaker trips and how long it stays open.
#[derive(Debug, Clone, Copy)]
pub struct CircuitBreakerConfig {
    /// Consecutive RPC failures before the breaker opens.
    pub failure_threshold: u32,
    /// How long the breaker stays Open before allowing a HalfOpen probe.
    pub cooldown: Duration,
}

/// A poll cycle's outcome, as far as the breaker is concerned. Only RPC
/// failures move the breaker — a storage or parse error is a different
/// failure domain and should not trip a breaker meant to protect the RPC
/// provider from a hot retry loop.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    Success,
    RpcFailure,
}

/// Closed/Open/HalfOpen breaker gating whether the poll loop should attempt
/// an RPC call this cycle.
///
/// Pure state machine: no I/O, no async. `should_allow` and `record` are
/// called from the run loop around `poll_once`; a real clock is used in
/// production but any `Fn() -> Instant` can be injected for deterministic
/// tests.
pub struct CircuitBreaker<Clock = fn() -> Instant>
where
    Clock: Fn() -> Instant,
{
    config: CircuitBreakerConfig,
    state: BreakerState,
    consecutive_failures: u32,
    /// When the breaker last transitioned into `Open`. `None` while Closed.
    opened_at: Option<Instant>,
    clock: Clock,
}

impl CircuitBreaker<fn() -> Instant> {
    pub fn new(config: CircuitBreakerConfig) -> Self {
        Self::with_clock(config, Instant::now)
    }
}

impl<Clock: Fn() -> Instant> CircuitBreaker<Clock> {
    pub fn with_clock(config: CircuitBreakerConfig, clock: Clock) -> Self {
        Self {
            config,
            state: BreakerState::Closed,
            consecutive_failures: 0,
            opened_at: None,
            clock,
        }
    }

    pub fn state(&self) -> BreakerState {
        self.state
    }

    pub fn consecutive_failures(&self) -> u32 {
        self.consecutive_failures
    }

    /// Whether the run loop should attempt a poll this cycle. Transitions
    /// Open → HalfOpen internally once the cooldown has elapsed, since that
    /// transition only becomes observable at the moment something asks.
    pub fn should_allow(&mut self) -> bool {
        match self.state {
            BreakerState::Closed => true,
            BreakerState::HalfOpen => true,
            BreakerState::Open => {
                let opened_at = self
                    .opened_at
                    .expect("opened_at is always Some while state is Open");
                if (self.clock)().duration_since(opened_at) >= self.config.cooldown {
                    self.state = BreakerState::HalfOpen;
                    true
                } else {
                    false
                }
            }
        }
    }

    /// Record the outcome of a poll cycle that was actually attempted
    /// (`should_allow` returned true). A non-RPC outcome (storage/parse/config
    /// error, or simply "no events") should be reported as `Success` — only an
    /// RPC-layer failure is this breaker's concern.
    pub fn record(&mut self, outcome: Outcome) {
        match outcome {
            Outcome::Success => {
                self.consecutive_failures = 0;
                self.state = BreakerState::Closed;
                self.opened_at = None;
            }
            Outcome::RpcFailure => {
                self.consecutive_failures = self.consecutive_failures.saturating_add(1);
                match self.state {
                    BreakerState::Closed => {
                        if self.consecutive_failures >= self.config.failure_threshold {
                            self.trip();
                        }
                    }
                    BreakerState::HalfOpen => {
                        // Probe failed: back to Open, cooldown restarts.
                        self.trip();
                    }
                    BreakerState::Open => {
                        // Only reachable if something records while should_allow
                        // was never consulted; keep the breaker open and let the
                        // existing cooldown continue rather than extending it.
                    }
                }
            }
        }
    }

    fn trip(&mut self) {
        self.state = BreakerState::Open;
        self.opened_at = Some((self.clock)());
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;
    use std::rc::Rc;

    fn config(threshold: u32, cooldown_ms: u64) -> CircuitBreakerConfig {
        CircuitBreakerConfig {
            failure_threshold: threshold,
            cooldown: Duration::from_millis(cooldown_ms),
        }
    }

    /// A clock the test controls explicitly, rather than depending on real
    /// elapsed wall-clock time (which would make cooldown-boundary tests
    /// flaky).
    fn controllable_clock() -> (impl Fn() -> Instant, impl Fn(Duration)) {
        let start = Instant::now();
        let now = Rc::new(Cell::new(start));
        let get = {
            let now = now.clone();
            move || now.get()
        };
        let advance = move |d: Duration| now.set(now.get() + d);
        (get, advance)
    }

    #[test]
    fn starts_closed_and_allows_polls() {
        let mut breaker = CircuitBreaker::new(config(3, 1000));
        assert_eq!(breaker.state(), BreakerState::Closed);
        assert!(breaker.should_allow());
    }

    #[test]
    fn stays_closed_below_threshold() {
        let mut breaker = CircuitBreaker::new(config(3, 1000));
        breaker.record(Outcome::RpcFailure);
        breaker.record(Outcome::RpcFailure);
        assert_eq!(breaker.state(), BreakerState::Closed);
        assert_eq!(breaker.consecutive_failures(), 2);
        assert!(breaker.should_allow());
    }

    #[test]
    fn opens_at_threshold() {
        let mut breaker = CircuitBreaker::new(config(3, 1000));
        breaker.record(Outcome::RpcFailure);
        breaker.record(Outcome::RpcFailure);
        breaker.record(Outcome::RpcFailure);
        assert_eq!(breaker.state(), BreakerState::Open);
    }

    #[test]
    fn success_resets_failure_count_and_stays_closed() {
        let mut breaker = CircuitBreaker::new(config(3, 1000));
        breaker.record(Outcome::RpcFailure);
        breaker.record(Outcome::RpcFailure);
        breaker.record(Outcome::Success);
        assert_eq!(breaker.consecutive_failures(), 0);
        assert_eq!(breaker.state(), BreakerState::Closed);
    }

    #[test]
    fn open_breaker_blocks_polls_until_cooldown_elapses() {
        let (clock, advance) = controllable_clock();
        let mut breaker = CircuitBreaker::with_clock(config(1, 1000), clock);
        breaker.record(Outcome::RpcFailure);
        assert_eq!(breaker.state(), BreakerState::Open);
        assert!(
            !breaker.should_allow(),
            "must block while cooldown has not elapsed"
        );

        advance(Duration::from_millis(500));
        assert!(
            !breaker.should_allow(),
            "must still block halfway through the cooldown"
        );

        advance(Duration::from_millis(500));
        assert!(
            breaker.should_allow(),
            "must allow exactly one probe once the cooldown has elapsed"
        );
        assert_eq!(breaker.state(), BreakerState::HalfOpen);
    }

    #[test]
    fn half_open_probe_success_closes_breaker() {
        let (clock, advance) = controllable_clock();
        let mut breaker = CircuitBreaker::with_clock(config(1, 1000), clock);
        breaker.record(Outcome::RpcFailure);
        advance(Duration::from_millis(1000));
        assert!(breaker.should_allow());
        assert_eq!(breaker.state(), BreakerState::HalfOpen);

        breaker.record(Outcome::Success);
        assert_eq!(breaker.state(), BreakerState::Closed);
        assert_eq!(breaker.consecutive_failures(), 0);
        assert!(breaker.should_allow());
    }

    #[test]
    fn half_open_probe_failure_reopens_and_restarts_cooldown() {
        let (clock, advance) = controllable_clock();
        let mut breaker = CircuitBreaker::with_clock(config(1, 1000), clock);
        breaker.record(Outcome::RpcFailure);
        advance(Duration::from_millis(1000));
        assert!(breaker.should_allow()); // HalfOpen probe allowed
        breaker.record(Outcome::RpcFailure); // probe fails
        assert_eq!(breaker.state(), BreakerState::Open);

        // Cooldown must have restarted from this failure, not the original one:
        // advancing by only slightly less than the full cooldown must still block.
        advance(Duration::from_millis(999));
        assert!(!breaker.should_allow());
        advance(Duration::from_millis(1));
        assert!(breaker.should_allow());
    }

    #[test]
    fn non_rpc_success_between_failures_resets_the_streak() {
        // A poll cycle with no RPC failure (e.g. a clean cycle with zero
        // events) must reset the consecutive-failure streak, matching how
        // record(Success) is called for any cycle that did not fail at the
        // RPC layer.
        let mut breaker = CircuitBreaker::new(config(3, 1000));
        breaker.record(Outcome::RpcFailure);
        breaker.record(Outcome::RpcFailure);
        breaker.record(Outcome::Success);
        breaker.record(Outcome::RpcFailure);
        breaker.record(Outcome::RpcFailure);
        assert_eq!(
            breaker.state(),
            BreakerState::Closed,
            "two failures after a reset must not reopen a threshold-3 breaker"
        );
    }
}
