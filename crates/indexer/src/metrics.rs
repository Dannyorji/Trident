//! Prometheus metrics for the indexer, served from a `GET /metrics` HTTP
//! endpoint (default port 9090, configurable via `METRICS_PORT`).
//!
//! [`install`] sets up the global recorder and starts the HTTP listener; the
//! `record_*`/`set_*` helpers below are called from the streamer at the
//! relevant points in `poll_once`.

use std::net::SocketAddr;

use metrics::{counter, describe_counter, describe_gauge, describe_histogram, gauge, histogram};
use metrics_exporter_prometheus::PrometheusBuilder;
use trident_common::TridentError;

pub const LEDGER_LAG: &str = "trident_indexer_ledger_lag";
/// Target Stellar ledger close time, used to convert ledger-count lag into an
/// estimated wall-clock staleness figure (issue #294). Not measured
/// per-deployment — the indexer does not retain per-ledger close timing once
/// a page is processed — so this uses Stellar's protocol-target close time
/// rather than a rolling average. Documented alongside the metric in
/// docs/observability/data-freshness.md; keep both in sync if this changes.
pub const AVG_LEDGER_CLOSE_SECONDS: f64 = 5.0;
/// Estimated wall-clock staleness: `trident_indexer_ledger_lag *
/// AVG_LEDGER_CLOSE_SECONDS` (issue #294). A derived convenience gauge, not
/// an independent measurement — see [`AVG_LEDGER_CLOSE_SECONDS`].
pub const LEDGER_LAG_SECONDS_ESTIMATED: &str = "trident_indexer_ledger_lag_seconds_estimated";
pub const EVENTS_TOTAL: &str = "trident_indexer_events_total";
pub const EVENTS_SKIPPED_TOTAL: &str = "trident_indexer_events_skipped_total";
pub const PARSE_ERRORS_TOTAL: &str = "trident_indexer_parse_errors_total";

/// Incremented when an event exhausts its retry budget and is written to the
/// parse-error (dead-letter) table so the poll can advance past it (issue
/// #414). Distinct from PARSE_ERRORS_TOTAL, which counts every parse failure
/// including ones that later succeed on retry: this counter only moves when an
/// event is actually abandoned, which is what an alert should fire on.
pub const DEAD_LETTERED_TOTAL: &str = "trident_indexer_dead_lettered_total";
/// Deliberately separate from DEAD_LETTERED_TOTAL above: that one counts
/// undecodable events captured in `parse_errors` (a poison message — retry
/// never helps), while this counts well-formed events whose database commit
/// failed after the retry budget and landed in `failed_events` for replay
/// (issue #508). Conflating them made one number answer two different
/// operational questions.
pub const PERSIST_DEAD_LETTERED_TOTAL: &str = "trident_indexer_persist_dead_lettered_total";
/// Current number of `failed_events` rows awaiting replay. Non-empty pages
/// via TridentIndexerPersistDeadLetterBacklog (monitoring/alerts.yml).
pub const PERSIST_DEAD_LETTER_BACKLOG: &str = "trident_indexer_persist_dead_letter_backlog";
pub const POLL_DURATION_SECONDS: &str = "trident_indexer_poll_duration_seconds";
pub const POLL_ERRORS_TOTAL: &str = "trident_indexer_poll_errors_total";
pub const RPC_RETRIES_TOTAL: &str = "trident_indexer_rpc_retries_total";
pub const EFFECTIVE_POLL_INTERVAL_MS: &str = "trident_indexer_effective_poll_interval_ms";
pub const RPC_TIMEOUTS_TOTAL: &str = "trident_indexer_rpc_timeouts_total";
pub const RPC_ACTIVE_ENDPOINT: &str = "trident_indexer_rpc_active_endpoint";
pub const RPC_FAILOVERS_TOTAL: &str = "trident_indexer_rpc_failovers_total";
/// Circuit breaker state (issue #197): 0 = Closed, 1 = Open, 2 = HalfOpen.
/// See `streamer::circuit_breaker` for the state machine.
pub const RPC_BREAKER_STATE: &str = "trident_indexer_rpc_breaker_state";
/// Consecutive RPC-layer poll failures since the last success (issue #197).
/// Resets to 0 on any successful poll; feeds the breaker's own threshold.
pub const RPC_CONSECUTIVE_FAILURES: &str = "trident_indexer_rpc_consecutive_failures";
/// Count of structurally valid ScVal variants decoded from event payloads
/// where they should never legitimately appear (`ContractInstance`,
/// `LedgerKeyContractInstance`, `LedgerKeyNonce`). Emitted by the shared
/// decoder in `trident_common::scval` (issue #506, superseding the #415
/// debug-fallback counter: the decoder no longer has a fallback — matches
/// are exhaustive, so a new XDR variant fails compilation instead).
pub const UNEXPECTED_SCVAL_VARIANT_TOTAL: &str =
    trident_common::scval::UNEXPECTED_SCVAL_VARIANT_TOTAL;
pub const OUTBOX_BACKLOG: &str = "trident_indexer_outbox_backlog";

/// Reconciliation loop (issue #511): passes that completed a full compare of
/// a settled ledger window against the RPC source.
pub const RECONCILE_PASSES_TOTAL: &str = "trident_indexer_reconcile_passes_total";
/// Passes that aborted before producing a report (RPC or DB failure). A
/// failing reconciler reports nothing — which must never read as clean.
pub const RECONCILE_PASS_FAILURES_TOTAL: &str = "trident_indexer_reconcile_pass_failures_total";
/// Events the RPC reports for reconciled windows that the database does not
/// account for — the silent-under-indexing signal this loop exists to catch.
pub const RECONCILE_MISSING_EVENTS_TOTAL: &str = "trident_indexer_reconcile_missing_events_total";
/// Events the database holds that the RPC does not report for the window —
/// over-indexing, as wrong as under-indexing.
pub const RECONCILE_EXTRA_EVENTS_TOTAL: &str = "trident_indexer_reconcile_extra_events_total";
/// Ledgers in the most recent pass whose counts disagreed. Stays non-zero on
/// every pass until the discrepancy is resolved, which is what the alert
/// fires on.
pub const RECONCILE_DISCREPANT_LEDGERS: &str = "trident_indexer_reconcile_discrepant_ledgers";
/// Highest ledger covered by the most recent completed pass.
pub const RECONCILE_WINDOW_END_LEDGER: &str = "trident_indexer_reconcile_window_end_ledger";
pub const OUTBOX_PUBLISHED_TOTAL: &str = "trident_indexer_outbox_published_total";
pub const OUTBOX_PUBLISH_FAILURES_TOTAL: &str = "trident_indexer_outbox_publish_failures_total";
/// RPC call latency in seconds, labelled by `method` (e.g. `getEvents`) and
/// `endpoint` (the pool index serving the call, `0` = primary). Covers every
/// call regardless of outcome, so `_count` doubles as a per-method,
/// per-endpoint call-volume counter (issue #294).
pub const RPC_CALL_DURATION_SECONDS: &str = "trident_indexer_rpc_call_duration_seconds";
/// RPC call failures labelled by `method` and a coarse `error_type`: one of
/// `timeout`, `rate_limited`, `http_4xx`, `http_5xx`, `invalid_cursor`,
/// `rpc_error`, `empty_result`, or `transport` (issue #294).
pub const RPC_ERRORS_TOTAL: &str = "trident_indexer_rpc_errors_total";
/// Unix timestamp (seconds) of the most recent completed poll cycle. Use
/// `time() - trident_indexer_last_poll_timestamp_seconds > N` as a
/// dead-man's-switch alert for a stalled indexer (#218).
pub const HEARTBEAT_TIMESTAMP: &str = "trident_indexer_last_poll_timestamp_seconds";
/// Bounded per-contract event counter. Labels: `contract` (allowlisted contract ID or `"other"`).
/// Cardinality: |allowlist| + 1. In index-all mode (no allowlist) all events land in `"other"`.
pub const EVENTS_BY_CONTRACT_TOTAL: &str = "trident_indexer_events_by_contract_total";
pub const EVENT_DECODE_DURATION_SECONDS: &str = "trident_indexer_event_decode_duration_seconds";
/// Health score (0-100) for each RPC endpoint. Label: `endpoint` (URL).
pub const RPC_HEALTH_SCORE: &str = "trident_rpc_health_score";
/// Indexer's own Postgres pool, documented in docs/metrics-catalog.md.
pub const DB_POOL_SIZE: &str = "trident_indexer_db_pool_size";
pub const DB_POOL_IDLE_CONNECTIONS: &str = "trident_indexer_db_pool_idle_connections";
/// Backfill rate in ledgers per second, measured over each poll cycle that
/// made forward progress while behind the chain tip (issue #420).
///
/// This is the production-observable form of the catch-up benchmark: a cold
/// start or a recovery from an outage can be watched live rather than only
/// reproduced offline. It is published only while catching up — see
/// [`set_catchup_rates`] — so a caught-up indexer polling one ledger every few
/// seconds does not drag the reported rate toward zero and make a healthy
/// indexer look slow.
pub const CATCHUP_LEDGERS_PER_SECOND: &str = "trident_indexer_catchup_ledgers_per_second";
/// Backfill rate in events per second, measured over the same window as
/// [`CATCHUP_LEDGERS_PER_SECOND`] (issue #420). Ledgers/sec alone hides the
/// binding constraint: a sparse range moves fast in ledgers and slow in events.
pub const CATCHUP_EVENTS_PER_SECOND: &str = "trident_indexer_catchup_events_per_second";

/// Distance (in ledgers) between the current ingest cursor and the upper bound
/// of the last named `soroban_events` partition (issue #525).
///
/// When this value drops to zero the indexer has reached or passed the
/// boundary of the last named partition. Rows for ledgers beyond that boundary
/// fall into `soroban_events_default` (the catch-all DEFAULT partition), which
/// is the silent failure mode this alert guards against.
///
/// Updated once per poll cycle. The alert thresholds in `monitoring/alerts.yml`
/// are:
///   - warning  (TridentPartitionExhaustionWarning): < 5_000_000 ledgers (~289 days)
///   - critical (TridentPartitionExhausted):          <= 0        ledgers (already past)
pub const PARTITION_LOOKAHEAD_LEDGERS: &str = "trident_indexer_partition_lookahead_ledgers";
/// Ledger reorganisations detected and repaired (issue #196): a divergence
/// between the RPC's current history and what was already persisted,
/// resolved by deleting the affected rows and rewinding the cursor.
pub const REORGS_TOTAL: &str = "trident_indexer_reorgs_total";
/// Gaps found in the processed ledger range by the periodic scan of
/// `ledger_metadata` (issue #216). Each gap is one contiguous run of missing
/// sequences, regardless of how many ledgers it spans. A gap still open on a
/// later scan increments this again — the counter reflects scan findings,
/// not distinct gaps, so a persistently-gappy table shows a climbing rate
/// rather than going silent after the first detection.
pub const LEDGER_GAPS_DETECTED_TOTAL: &str = "trident_indexer_ledger_gaps_detected_total";
/// Previously-enqueued backfill jobs the scan confirmed are no longer gaps
/// (issue #216): on each run, any pending/running `backfill_jobs` row whose
/// range no longer appears in the freshly-scanned gap list has been filled
/// (by the backfill worker, or by the live poll loop catching back up), and
/// is marked `done` here.
pub const LEDGER_GAPS_CLOSED_TOTAL: &str = "trident_indexer_ledger_gaps_closed_total";
/// A retained-floor recovery (issue #388) advanced the cursor past ledgers
/// the RPC no longer retains, permanently losing that span from live polling
/// (issue #598). Distinct from `LEDGER_GAPS_DETECTED_TOTAL`: this fires at
/// the moment of loss, in the live poll path, rather than during the
/// periodic `ledger_metadata` scan, so an operator can alert on it directly
/// instead of relying on the gap scan to notice later.
pub const RETAINED_FLOOR_LEDGERS_SKIPPED_TOTAL: &str =
    "trident_indexer_retained_floor_ledgers_skipped_total";

/// Install the global Prometheus recorder and start serving `/metrics` on
/// `port`. Must be called once, before the streamer starts recording.
pub fn install(port: u16) -> Result<(), TridentError> {
    let addr: SocketAddr = ([0, 0, 0, 0], port).into();

    PrometheusBuilder::new()
        .with_http_listener(addr)
        .install()
        .map_err(|e| {
            TridentError::config(anyhow::Error::new(e).context("failed to start metrics exporter"))
        })?;

    describe_gauge!(
        LEDGER_LAG,
        "Difference between chain tip and indexer cursor (ledgers)"
    );
    describe_gauge!(
        LEDGER_LAG_SECONDS_ESTIMATED,
        "Estimated wall-clock lag: ledger lag * average ledger close time (issue #294)"
    );
    describe_counter!(EVENTS_TOTAL, "Total events processed since startup");
    describe_counter!(
        EVENTS_SKIPPED_TOTAL,
        "Events skipped (diagnostic, failed call, or contract filter)"
    );
    describe_counter!(PARSE_ERRORS_TOTAL, "Total events that failed XDR decoding");
    describe_counter!(
        DEAD_LETTERED_TOTAL,
        "Undecodable events durably captured in parse_errors (issue #414)"
    );
    describe_counter!(
        PERSIST_DEAD_LETTERED_TOTAL,
        "Well-formed events captured in failed_events after exhausting the persist retry budget (issue #508)"
    );
    describe_gauge!(
        PERSIST_DEAD_LETTER_BACKLOG,
        "failed_events rows awaiting replay; non-empty pages via TridentIndexerPersistDeadLetterBacklog (issue #508)"
    );
    describe_histogram!(
        POLL_DURATION_SECONDS,
        "Time per poll_once cycle, in seconds"
    );
    describe_counter!(POLL_ERRORS_TOTAL, "Poll cycles that returned an error");
    describe_counter!(
        RPC_RETRIES_TOTAL,
        "Total RPC retries triggered by transient failures"
    );
    describe_gauge!(
        EFFECTIVE_POLL_INTERVAL_MS,
        "Current adaptive poll interval in milliseconds (issue #198)"
    );
    describe_counter!(
        RPC_TIMEOUTS_TOTAL,
        "RPC calls aborted by the connect or request timeout (issue #214)"
    );
    describe_gauge!(
        RPC_ACTIVE_ENDPOINT,
        "Index of the RPC endpoint currently in use, 0 = primary (issue #213)"
    );
    describe_counter!(
        RPC_FAILOVERS_TOTAL,
        "Times the indexer failed over to another RPC endpoint (issue #213)"
    );
    describe_counter!(
        REORGS_TOTAL,
        "Total number of ledger reorganisations / rollbacks detected and reconciled (issue #196)"
    );
    describe_gauge!(
        RPC_BREAKER_STATE,
        "RPC circuit breaker state: 0=Closed, 1=Open, 2=HalfOpen (issue #197)"
    );
    describe_gauge!(
        RPC_CONSECUTIVE_FAILURES,
        "Consecutive RPC-layer poll failures since the last success (issue #197)"
    );
    describe_gauge!(
        OUTBOX_BACKLOG,
        "Committed events not yet published to the Redis stream (issue #200)"
    );
    describe_counter!(
        OUTBOX_PUBLISHED_TOTAL,
        "Events published to the Redis stream by the outbox relay (issue #200)"
    );
    describe_counter!(
        OUTBOX_PUBLISH_FAILURES_TOTAL,
        "Outbox publish attempts that failed (issue #200)"
    );
    describe_counter!(
        UNEXPECTED_SCVAL_VARIANT_TOTAL,
        "ScVal variants decoded from event payloads where they should never appear (issue #506)"
    );
    describe_counter!(
        RECONCILE_PASSES_TOTAL,
        "Reconciliation passes that completed a full window compare (issue #511)"
    );
    describe_counter!(
        RECONCILE_PASS_FAILURES_TOTAL,
        "Reconciliation passes that aborted before producing a report (issue #511)"
    );
    describe_counter!(
        RECONCILE_MISSING_EVENTS_TOTAL,
        "Events on the RPC source that the database does not account for (issue #511)"
    );
    describe_counter!(
        RECONCILE_EXTRA_EVENTS_TOTAL,
        "Events in the database that the RPC source does not report (issue #511)"
    );
    describe_gauge!(
        RECONCILE_DISCREPANT_LEDGERS,
        "Ledgers in the most recent reconciliation pass with disagreeing counts (issue #511)"
    );
    describe_gauge!(
        RECONCILE_WINDOW_END_LEDGER,
        "Highest ledger covered by the most recent completed reconciliation pass (issue #511)"
    );
    describe_gauge!(
        HEARTBEAT_TIMESTAMP,
        "Unix timestamp (seconds) of the most recent completed poll cycle (#218)"
    );
    describe_histogram!(
        RPC_CALL_DURATION_SECONDS,
        "RPC call latency in seconds, labelled by method and endpoint index (issue #294)"
    );
    describe_counter!(
        RPC_ERRORS_TOTAL,
        "RPC call failures labelled by method and error_type (issue #294)"
    );
    describe_counter!(
        EVENTS_BY_CONTRACT_TOTAL,
        "Events processed per contract (bounded: allowlisted contract IDs + 'other' bucket)"
    );
    describe_histogram!(
        EVENT_DECODE_DURATION_SECONDS,
        "Time to XDR-decode a single event, in seconds (per-event parse latency)"
    );
    describe_gauge!(
        RPC_HEALTH_SCORE,
        "Health score (0-100) for each RPC endpoint (multi-RPC failover)"
    );
    describe_gauge!(
        CATCHUP_LEDGERS_PER_SECOND,
        "Backfill rate in ledgers/sec while behind the chain tip (issue #420)"
    );
    describe_gauge!(
        CATCHUP_EVENTS_PER_SECOND,
        "Backfill rate in events/sec while behind the chain tip (issue #420)"
    );
    describe_gauge!(
        PARTITION_LOOKAHEAD_LEDGERS,
        "Ledgers remaining before the ingest cursor reaches the last named soroban_events partition boundary (issue #525)"
    );
    describe_counter!(
        REORGS_TOTAL,
        "Ledger reorganisations detected and repaired (issue #196)"
    );
    describe_counter!(
        LEDGER_GAPS_DETECTED_TOTAL,
        "Gaps found in the processed ledger range by the periodic ledger_metadata scan (issue #216)"
    );
    describe_counter!(
        LEDGER_GAPS_CLOSED_TOTAL,
        "Previously-enqueued backfill jobs confirmed filled by a later gap scan (issue #216)"
    );

    // Counters only render in the scrape output once touched at least once;
    // seed them at zero so /metrics is complete from the very first scrape.
    counter!(EVENTS_TOTAL).increment(0);
    counter!(EVENTS_SKIPPED_TOTAL).increment(0);
    counter!(PARSE_ERRORS_TOTAL).increment(0);
    counter!(POLL_ERRORS_TOTAL).increment(0);
    counter!(RPC_RETRIES_TOTAL).increment(0);
    counter!(REORGS_TOTAL).increment(0);
    counter!(RPC_TIMEOUTS_TOTAL).increment(0);
    counter!(RPC_FAILOVERS_TOTAL).increment(0);
    counter!(OUTBOX_PUBLISHED_TOTAL).increment(0);
    counter!(OUTBOX_PUBLISH_FAILURES_TOTAL).increment(0);
    counter!(LEDGER_GAPS_DETECTED_TOTAL).increment(0);
    counter!(LEDGER_GAPS_CLOSED_TOTAL).increment(0);
    counter!(UNEXPECTED_SCVAL_VARIANT_TOTAL).increment(0);
    counter!(PERSIST_DEAD_LETTERED_TOTAL).increment(0);
    gauge!(PERSIST_DEAD_LETTER_BACKLOG).set(0.0);
    counter!(DEAD_LETTERED_TOTAL).increment(0);
    counter!(RECONCILE_PASSES_TOTAL).increment(0);
    counter!(RECONCILE_PASS_FAILURES_TOTAL).increment(0);
    counter!(RECONCILE_MISSING_EVENTS_TOTAL).increment(0);
    counter!(RECONCILE_EXTRA_EVENTS_TOTAL).increment(0);
    gauge!(RECONCILE_DISCREPANT_LEDGERS).set(0.0);
    gauge!(RECONCILE_WINDOW_END_LEDGER).set(0.0);
    gauge!(RPC_ACTIVE_ENDPOINT).set(0.0);
    gauge!(RPC_BREAKER_STATE).set(0.0);
    gauge!(RPC_CONSECUTIVE_FAILURES).set(0.0);
    gauge!(OUTBOX_BACKLOG).set(0.0);
    gauge!(LEDGER_LAG).set(0.0);
    gauge!(LEDGER_LAG_SECONDS_ESTIMATED).set(0.0);
    gauge!(EFFECTIVE_POLL_INTERVAL_MS).set(0.0);
    gauge!(HEARTBEAT_TIMESTAMP).set(0.0);
    gauge!(DB_POOL_SIZE).set(0.0);
    gauge!(DB_POOL_IDLE_CONNECTIONS).set(0.0);
    // Seed at 0 so the gauge is present from the first scrape and fails safe.
    // Seeding at i64::MAX would report infinite headroom, so an indexer that
    // crash-loops before its first successful poll would leave both partition
    // alerts resolved — silence in exactly the case that needs paging. The
    // real value is written after the first poll cycle.
    gauge!(PARTITION_LOOKAHEAD_LEDGERS).set(0.0);

    // Histograms render nothing at all until they observe a value — not even
    // a HELP/TYPE header — so an indexer that has not yet made an RPC call
    // exports no `trident_indexer_rpc_call_duration_seconds_*` series. Any
    // alert dividing by `..._count` then evaluates against an empty vector
    // and silently never fires, which is exactly the class of dead alert the
    // metric-name check exists to catch. Seeding a zero observation makes the
    // series exist from the first scrape, matching the counters above.
    //
    // The cost is one bucketed sample of 0.0 per histogram, which shifts the
    // reported minimum but not the alerting ratios these feed.
    histogram!(POLL_DURATION_SECONDS).record(0.0);
    histogram!(EVENT_DECODE_DURATION_SECONDS).record(0.0);
    // Labelled, so seed the methods the poll loop actually calls — the RPC
    // alerts `sum()` across labels, so the series just has to exist.
    for method in ["getEvents", "getLedgers"] {
        histogram!(RPC_CALL_DURATION_SECONDS, "method" => method, "endpoint" => "0").record(0.0);
    }

    tracing::info!(port, "Metrics endpoint listening");
    Ok(())
}

/// Publish ledger-count lag and its derived estimated-seconds-behind gauge
/// together, so the two figures can never drift out of sync (issue #294).
pub fn set_ledger_lag(lag: i64) {
    gauge!(LEDGER_LAG).set(lag as f64);
    gauge!(LEDGER_LAG_SECONDS_ESTIMATED).set(lag as f64 * AVG_LEDGER_CLOSE_SECONDS);
}

pub fn set_effective_poll_interval(ms: u64) {
    gauge!(EFFECTIVE_POLL_INTERVAL_MS).set(ms as f64);
}

/// Stamp the heartbeat to the current Unix time. Called at the end of every
/// poll cycle (success or failure) so a dead-man's switch alert can detect a
/// stalled-but-not-crashed indexer (#218).
pub fn set_heartbeat_timestamp(secs: f64) {
    gauge!(HEARTBEAT_TIMESTAMP).set(secs);
}

/// Stamp the heartbeat to now. Convenience wrapper over
/// [`set_heartbeat_timestamp`] for the poll loop, which has no reason to read
/// the clock itself.
pub fn record_heartbeat() {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs_f64())
        .unwrap_or(0.0);
    set_heartbeat_timestamp(secs);
}

/// Publish the indexer's own Postgres pool utilisation (docs/metrics-catalog.md).
pub fn set_db_pool_stats(size: u32, idle: u32) {
    gauge!(DB_POOL_SIZE).set(size as f64);
    gauge!(DB_POOL_IDLE_CONNECTIONS).set(idle as f64);
}

/// Minimum ledger lag before a poll cycle counts as "catching up" (issue #420).
///
/// At the chain tip the indexer polls faster than ledgers close, so cycles
/// advance 0-1 ledgers and the instantaneous rate is dominated by the poll
/// interval rather than by throughput. Only cycles with a real deficit behind
/// them describe backfill speed.
pub const CATCHUP_LAG_THRESHOLD_LEDGERS: i64 = 10;

/// Publish catch-up throughput for one poll cycle (issue #420).
///
/// Called after a cycle that advanced the cursor. `lag_before` is the ledger
/// deficit at the start of the cycle; the rates are published only when that
/// deficit exceeds [`CATCHUP_LAG_THRESHOLD_LEDGERS`], so the gauges describe
/// backfill speed rather than steady-state tip-following.
///
/// Gauges rather than counters: the question these answer is "how fast is it
/// going right now", which is what sizes a recovery window. Cumulative totals
/// are already available from `EVENTS_TOTAL`.
pub fn set_catchup_rates(
    ledgers_advanced: u64,
    events_processed: u64,
    elapsed_secs: f64,
    lag_before: i64,
) {
    if lag_before <= CATCHUP_LAG_THRESHOLD_LEDGERS || elapsed_secs <= 0.0 {
        return;
    }

    gauge!(CATCHUP_LEDGERS_PER_SECOND).set(ledgers_advanced as f64 / elapsed_secs);
    gauge!(CATCHUP_EVENTS_PER_SECOND).set(events_processed as f64 / elapsed_secs);
}

pub fn record_events_processed(count: u64) {
    if count > 0 {
        counter!(EVENTS_TOTAL).increment(count);
    }
}

pub fn record_events_skipped(count: u64) {
    if count > 0 {
        counter!(EVENTS_SKIPPED_TOTAL).increment(count);
    }
}

pub fn record_parse_error() {
    counter!(PARSE_ERRORS_TOTAL).increment(1);
}

pub fn record_persist_dead_lettered() {
    counter!(PERSIST_DEAD_LETTERED_TOTAL).increment(1);
}

pub fn set_persist_dead_letter_backlog(depth: i64) {
    gauge!(PERSIST_DEAD_LETTER_BACKLOG).set(depth as f64);
}

pub fn record_dead_lettered() {
    counter!(DEAD_LETTERED_TOTAL).increment(1);
}

pub fn record_reconcile_pass_completed() {
    counter!(RECONCILE_PASSES_TOTAL).increment(1);
}

pub fn record_reconcile_pass_failed() {
    counter!(RECONCILE_PASS_FAILURES_TOTAL).increment(1);
}

pub fn record_reconcile_missing_events(count: u64) {
    counter!(RECONCILE_MISSING_EVENTS_TOTAL).increment(count);
}

pub fn record_reconcile_extra_events(count: u64) {
    counter!(RECONCILE_EXTRA_EVENTS_TOTAL).increment(count);
}

pub fn set_reconcile_discrepant_ledgers(count: i64) {
    gauge!(RECONCILE_DISCREPANT_LEDGERS).set(count as f64);
}

pub fn set_reconcile_window_end(ledger: u64) {
    gauge!(RECONCILE_WINDOW_END_LEDGER).set(ledger as f64);
}

pub fn record_poll_duration(seconds: f64) {
    histogram!(POLL_DURATION_SECONDS).record(seconds);
}

pub fn record_poll_error() {
    counter!(POLL_ERRORS_TOTAL).increment(1);
}

pub fn record_rpc_retry() {
    counter!(RPC_RETRIES_TOTAL).increment(1);
}

/// Count a detected-and-repaired ledger reorg (issue #196).
pub fn record_reorg() {
    counter!(REORGS_TOTAL).increment(1);
}

/// Count gaps found by one gap-scan run (issue #216).
pub fn record_ledger_gaps_detected(count: u64) {
    if count > 0 {
        counter!(LEDGER_GAPS_DETECTED_TOTAL).increment(count);
    }
}

/// Count previously-enqueued jobs a scan confirmed are now filled (issue #216).
pub fn record_ledger_gaps_closed(count: u64) {
    if count > 0 {
        counter!(LEDGER_GAPS_CLOSED_TOTAL).increment(count);
    }
}

/// Count ledgers permanently skipped by a retained-floor recovery (issue #598).
pub fn record_retained_floor_ledgers_skipped(count: u64) {
    if count > 0 {
        counter!(RETAINED_FLOOR_LEDGERS_SKIPPED_TOTAL).increment(count);
    }
}

/// Count an RPC call that hit the connect or overall request timeout (issue #214).
pub fn record_rpc_timeout() {
    counter!(RPC_TIMEOUTS_TOTAL).increment(1);
}

/// Publish which endpoint of the configured pool is currently serving traffic
/// (0 = primary), so a silent, sustained failover is visible (issue #213).
pub fn set_rpc_active_endpoint(index: usize) {
    gauge!(RPC_ACTIVE_ENDPOINT).set(index as f64);
}

/// Publish the RPC circuit breaker's current state (issue #197).
pub fn set_rpc_breaker_state(state: crate::streamer::BreakerState) {
    gauge!(RPC_BREAKER_STATE).set(state.as_metric_value());
}

/// Publish the breaker's consecutive-RPC-failure count (issue #197).
pub fn set_rpc_breaker_consecutive_failures(count: u32) {
    gauge!(RPC_CONSECUTIVE_FAILURES).set(count as f64);
}

/// Count a switch to a different RPC endpoint (issue #213).
/// Called only by rpc::endpoints, which is currently unreferenced.
#[allow(dead_code)]
pub fn record_rpc_failover() {
    counter!(RPC_FAILOVERS_TOTAL).increment(1);
}

/// Publish the number of committed-but-unpublished events. A backlog that keeps
/// climbing means live subscribers are missing data (issue #200).
pub fn set_outbox_backlog(backlog: i64) {
    gauge!(OUTBOX_BACKLOG).set(backlog as f64);
}

/// Count an event delivered to the Redis stream by the relay (issue #200).
pub fn record_outbox_published() {
    counter!(OUTBOX_PUBLISHED_TOTAL).increment(1);
}

/// Count a failed relay publish attempt (issue #200).
pub fn record_outbox_publish_failure() {
    counter!(OUTBOX_PUBLISH_FAILURES_TOTAL).increment(1);
}

/// Record one RPC call's latency, labelled by method and the endpoint pool
/// index that served it (0 = primary). Recorded for every call regardless of
/// outcome, so a degraded-but-not-yet-failing provider (rising latency, no
/// errors yet) is visible before it starts timing out (issue #294).
pub fn record_rpc_call_duration(method: &'static str, endpoint_index: usize, seconds: f64) {
    histogram!(RPC_CALL_DURATION_SECONDS, "method" => method, "endpoint" => endpoint_index.to_string())
        .record(seconds);
}

/// Count an RPC failure labelled by method and a coarse error type, so ops
/// can distinguish "chain is quiet" from "RPC is degraded" and see which
/// failure mode is driving it (issue #294).
pub fn record_rpc_error(method: &'static str, error_type: &'static str) {
    counter!(RPC_ERRORS_TOTAL, "method" => method, "error_type" => error_type).increment(1);
}

/// Increment the per-contract event counter. `contract_id` must be either an
/// allowlisted contract ID or the sentinel `"other"` — never an unbounded value.
pub fn record_events_by_contract(contract_id: &str, count: u64) {
    if count > 0 {
        counter!(EVENTS_BY_CONTRACT_TOTAL, "contract" => contract_id.to_string()).increment(count);
    }
}

pub fn record_decode_duration(seconds: f64) {
    histogram!(EVENT_DECODE_DURATION_SECONDS).record(seconds);
}

/// Set the health score for a specific RPC endpoint.
///
/// Called by the health scorer after each score update so operators can see
/// which endpoints are degraded and whether failover is working.
pub fn set_rpc_health_score(endpoint: &str, score: u8) {
    gauge!(RPC_HEALTH_SCORE, "endpoint" => endpoint.to_string()).set(score as f64);
}

/// Publish how many ledgers remain before the ingest cursor reaches the upper
/// bound of the last named `soroban_events` partition (issue #525).
///
/// A negative value means the cursor has already passed the boundary and rows
/// are falling into the DEFAULT catch-all partition. Two Prometheus alerts in
/// `monitoring/alerts.yml` fire on this gauge:
///   - TridentPartitionExhaustionWarning  (< 5_000_000, warning severity)
///   - TridentPartitionExhausted          (<= 0,        critical severity + Fatal poll error)
pub fn set_partition_lookahead(lookahead: i64) {
    gauge!(PARTITION_LOOKAHEAD_LEDGERS).set(lookahead as f64);
}
