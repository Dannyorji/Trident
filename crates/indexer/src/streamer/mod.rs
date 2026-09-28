//! # Streamer
//!
//! Owns the RPC polling loop. Responsibilities:
//!
//! - Maintaining the ledger cursor: reading the last processed sequence from
//!   `system_state` on startup, advancing it after each successful batch, and
//!   persisting it atomically with the events it covers.
//! - Calling `getEvents` on the Stellar Soroban RPC node on a configurable
//!   interval (`POLL_INTERVAL_MS`), following the `pagingToken` cursor field
//!   to paginate across large ledger ranges within a single poll cycle.
//! - Fault tolerance and retry logic: transient RPC failures are retried with
//!   exponential backoff; persistent failures are logged without crashing the
//!   process or losing cursor position so the next poll cycle can recover.
//! - Handing each raw event to the `Parser` and committing normalised
//!   `SorobanEvent` values to PostgreSQL together with an outbox row (issue
//!   #200). Redis delivery is owned by `redis_stream::relay`, so a crash
//!   between the commit and the publish cannot drop an event.

mod circuit_breaker;

use std::collections::{HashMap, HashSet};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use sqlx::PgPool;
use tokio_retry::{strategy::ExponentialBackoff, Retry};
use tokio_util::sync::CancellationToken;
use tracing::Instrument;
use trident_common::{Severity, TridentError};

use crate::{
    alerting::{AlertContext, Alerter},
    config::Config,
    db, metrics,
    parser::Parser,
    poll::{AdaptivePoll, AdaptivePollConfig},
    rpc::{filters::build_event_filters, retry_strategy, FilterPlan, RpcClient, RpcHttpSettings},
    token_metadata,
};
pub use circuit_breaker::{BreakerState, CircuitBreaker, CircuitBreakerConfig, Outcome};

/// How often (in poll loop iterations) we re-query `indexed_contracts`.
/// At the default 5 s poll interval this is ≈ 60 s — matches the env-var default.
const FILTER_REFRESH_EVERY_N_POLLS: u32 = 12;

/// How often (in poll loop iterations) the gap scan runs (issue #216). Much
/// less frequent than the filter refresh above: a gap scan reads the whole
/// `ledger_metadata` table's sequence column via a window function, and a
/// gap that has existed for one poll interval will still be there in ten
/// minutes, so there is no correctness reason to run it more often than
/// this. At the default 5s poll interval this is ~10 minutes.
const GAP_SCAN_EVERY_N_POLLS: u32 = 120;

pub struct Streamer {
    config: Config,
    db: PgPool,
    rpc: RpcClient,
    parser: Parser,
    /// `None`  → index all contracts (empty `indexed_contracts` table).
    /// `Some`  → allowlist with per-contract `index_from` boundaries (issue
    ///           #202); events from unlisted contracts, or from listed ones
    ///           below their `index_from`, are skipped.
    contract_filter: Option<HashMap<String, i64>>,
    /// Server-side `getEvents` filters derived from `contract_filter` (issue
    /// #203). Rebuilt whenever the allowlist is reloaded. Only the contract
    /// ids matter to the RPC — the per-contract `index_from` boundaries are
    /// applied client-side, since `getEvents` has one `startLedger` for the
    /// whole request.
    filter_plan: FilterPlan,
    /// Counts poll cycles so we know when to refresh the filter.
    poll_count: u32,
    /// Outbound webhook alerter (issue #75). No-op when URL is not configured.
    alerter: Alerter,
    /// Chain tip ledger from the most recent RPC response (issue #75).
    last_chain_tip: u64,
    /// Adaptive poll-interval controller driven by chain-tip lag (issue #198).
    adaptive_poll: AdaptivePoll,
    /// Parsed-spec cache keyed by WASM code hash (issue #260).
    spec_cache: crate::spec::SpecCache,
    /// Last code hash synced to `contract_specs` per contract, so a redeploy
    /// (changed hash) is what triggers a refresh, not every poll cycle
    /// (issue #260).
    known_code_hashes: std::collections::HashMap<String, String>,
    /// Tracked contracts most recently classified as SEP-41 tokens (issue
    /// #269) — what bounds storage-snapshot fetching (issue #270) to
    /// contracts we actually know how to read a balance from.
    token_contracts: HashSet<String>,
    /// Trips after sustained RPC failures so the run loop stops attempting
    /// polls during an outage instead of retrying every interval (issue #197).
    rpc_breaker: CircuitBreaker,
}

/// One contract-storage snapshot change observed during a poll cycle,
/// pending persistence (issue #270). Owns its data so it outlives the
/// borrows built from `page_events`/`page_tokens` earlier in `poll_once`.
struct OwnedStorageSnapshot {
    contract_id: String,
    storage_key: String,
    key_json: serde_json::Value,
    value_json: Option<serde_json::Value>,
    ledger_sequence: u64,
}

impl Streamer {
    /// Build the streamer. It owns no Redis connection: events are committed to
    /// Postgres with an outbox row and delivered by `redis_stream::relay`
    /// (issue #200).
    pub async fn new(config: Config, db: PgPool) -> Result<Self, TridentError> {
        let rpc = RpcClient::with_endpoints(
            config.stellar_rpc_urls.clone(),
            &RpcHttpSettings {
                connect_timeout: config.rpc_connect_timeout,
                request_timeout: config.rpc_request_timeout,
                pool_idle_timeout: config.rpc_pool_idle_timeout,
                pool_max_idle_per_host: config.rpc_pool_max_idle_per_host,
                tcp_keepalive: config.rpc_tcp_keepalive,
                max_calls_per_sec: config.rpc_max_calls_per_sec,
            },
        )?;
        tracing::info!(
            endpoints = config.stellar_rpc_urls.len(),
            primary = %config.stellar_rpc_url,
            "RPC endpoint pool configured with health scoring"
        );
        let sac_registry = crate::parser::sac::SacRegistry::build(
            &config.tracked_sac_assets,
            &config.network_passphrase,
        )?;
        tracing::info!(
            tracked_assets = config.tracked_sac_assets.len(),
            "SAC asset registry built"
        );
        let parser = Parser::new(config.index_diagnostic).with_sac_registry(sac_registry);
        let contract_filter = Self::load_filter(&db, &config.network).await?;
        let filter_plan = plan_filters(contract_filter.as_ref(), &config.topic_filters);
        let alerter = Alerter::from_config(
            config.alert_webhook_url.clone(),
            config.alert_lag_threshold,
            config.alert_cooldown_minutes,
        )?;
        let adaptive_poll = AdaptivePoll::new(AdaptivePollConfig {
            floor: config.poll_interval_floor,
            ceiling: config.poll_interval_ceiling,
            high_watermark: config.lag_high_watermark,
            hysteresis_ledgers: config.poll_hysteresis_ledgers,
        });
        let rpc_breaker = CircuitBreaker::new(CircuitBreakerConfig {
            failure_threshold: config.rpc_breaker_failure_threshold,
            cooldown: config.rpc_breaker_cooldown,
        });

        Ok(Self {
            config,
            db,
            rpc,
            parser,
            contract_filter,
            filter_plan,
            poll_count: 0,
            alerter,
            last_chain_tip: 0,
            adaptive_poll,
            spec_cache: crate::spec::SpecCache::new(),
            known_code_hashes: std::collections::HashMap::new(),
            token_contracts: HashSet::new(),
            rpc_breaker,
        })
    }

    /// Load (or reload) the contract allowlist from DB.
    /// Returns `None` if the table is empty (index-all mode).
    async fn load_filter(
        pool: &PgPool,
        network: &str,
    ) -> Result<Option<HashMap<String, i64>>, TridentError> {
        let map = db::load_indexed_contracts(pool, network).await?;
        if map.is_empty() {
            Ok(None)
        } else {
            tracing::info!(count = map.len(), "Contract allowlist loaded");
            Ok(Some(map))
        }
    }

    /// Reload the contract filter from DB. Called periodically inside `run`.
    /// Detects newly-registered contracts with a historical `index_from` and
    /// logs a backfill suggestion (issue #202).
    pub async fn refresh_contract_filter(&mut self) -> Result<(), TridentError> {
        match Self::load_filter(&self.db, &self.config.network).await {
            Ok(filter) => {
                // Detect newly added contracts whose index_from is behind the
                // current cursor, which means historical events are missing.
                if let Some(ref new_map) = filter {
                    if let Some(ref old_map) = self.contract_filter {
                        let cursor = crate::db::get_cursor(&self.db, &self.config.network)
                            .await
                            .unwrap_or(0);
                        for (id, &index_from) in new_map {
                            if !old_map.contains_key(id)
                                && index_from > 0
                                && (index_from as u64) < cursor
                            {
                                tracing::warn!(
                                    contract_id = id,
                                    index_from = index_from,
                                    cursor = cursor,
                                    "Contract registered with historical index_from; \
                                     enqueue a backfill for ledgers {}..{}",
                                    index_from,
                                    cursor - 1,
                                );
                            }
                        }
                    }
                }
                self.filter_plan = plan_filters(filter.as_ref(), &self.config.topic_filters);
                self.contract_filter = filter;
                Ok(())
            }
            Err(e) => {
                // Non-fatal: keep the existing filter, log the error.
                tracing::warn!(error = %e, "Failed to refresh contract filter; keeping existing");
                Ok(())
            }
        }
    }

    /// Fetch + persist specs and detected interfaces for every tracked
    /// contract (issues #260, #269). Best-effort per contract: an RPC or
    /// parse failure just skips that contract for this cycle. A DB write
    /// only happens when the observed code hash differs from the last one
    /// synced, so a contract whose code has not changed costs one cheap
    /// `getLedgerEntries` call, not a WASM re-fetch.
    async fn sync_contract_specs(&mut self) {
        let Some(filter) = self.contract_filter.clone() else {
            return;
        };

        // Only the ids matter here — spec sync is per contract and unrelated
        // to each contract's index_from boundary.
        for contract_id in filter.into_keys() {
            match crate::spec::fetch_contract_spec(&self.rpc, &self.spec_cache, &contract_id).await
            {
                Ok(Some(contract_spec)) => {
                    if contract_spec.interfaces.iter().any(|i| i == "sep41_token") {
                        self.token_contracts.insert(contract_id.clone());
                    } else {
                        self.token_contracts.remove(&contract_id);
                    }

                    let changed =
                        self.known_code_hashes.get(&contract_id) != Some(&contract_spec.code_hash);
                    if changed {
                        match db::upsert_contract_spec(
                            &self.db,
                            &contract_id,
                            &self.config.network,
                            &contract_spec,
                        )
                        .await
                        {
                            Ok(()) => {
                                self.known_code_hashes
                                    .insert(contract_id.clone(), contract_spec.code_hash.clone());
                            }
                            Err(e) => {
                                tracing::warn!(contract_id = %contract_id, error = %e, "Failed to persist contract spec");
                            }
                        }
                    }
                }
                Ok(None) => {}
                Err(e) => {
                    tracing::warn!(contract_id = %contract_id, error = %e, "Failed to fetch contract spec");
                }
            }
        }
    }

    /// Scan `ledger_metadata` for gaps in the processed range and enqueue a
    /// `backfill_jobs` row for each, so `crates/backfill --from-queue` can
    /// re-fetch the missing ledgers (issue #216).
    ///
    /// Also reconciles the other direction: any previously-enqueued job whose
    /// range no longer shows up as a gap has been filled (by the backfill
    /// worker, or by the poll loop itself catching back up over it) and is
    /// marked `done`.
    ///
    /// Best-effort: a DB failure here is logged and the scan is skipped this
    /// cycle rather than propagated — this is a periodic maintenance task,
    /// not part of the ingest path, and the next scheduled scan will retry
    /// (issue #216 explicitly asks that the scan not compete with or block
    /// live polling).
    async fn scan_and_enqueue_gaps(&mut self) {
        let gaps = match db::scan_ledger_gaps(&self.db, self.config.gap_scan_max_per_run).await {
            Ok(gaps) => gaps,
            Err(e) => {
                tracing::warn!(error = %e, "Gap scan failed; will retry on the next scheduled scan");
                return;
            }
        };

        match db::close_filled_backfill_jobs(&self.db, &self.config.network, &gaps).await {
            Ok(closed) if closed > 0 => {
                metrics::record_ledger_gaps_closed(closed);
                tracing::info!(
                    closed,
                    "Gap scan: previously-enqueued backfill jobs confirmed filled"
                );
            }
            Ok(_) => {}
            Err(e) => {
                tracing::warn!(error = %e, "Failed to reconcile completed backfill jobs");
            }
        }

        if gaps.is_empty() {
            return;
        }

        metrics::record_ledger_gaps_detected(gaps.len() as u64);
        tracing::warn!(
            gap_count = gaps.len(),
            first_gap = ?gaps.first(),
            "Gap scan: found holes in the processed ledger range; enqueuing backfill jobs"
        );

        for gap in &gaps {
            if let Err(e) = db::enqueue_backfill_job(&self.db, *gap, &self.config.network).await {
                tracing::warn!(
                    from_ledger = gap.from_ledger,
                    to_ledger = gap.to_ledger,
                    error = %e,
                    "Failed to enqueue backfill job for a detected gap"
                );
            }
        }
    }

    /// Recover from a `getEvents` rejection reporting that `cursor` predates
    /// the RPC's retained history, by advancing to the oldest ledger the RPC
    /// still retains (issue #388). Returns the new cursor value.
    ///
    /// The span strictly between the old `cursor` and the new `floor` is
    /// permanently lost to live polling the moment the cursor jumps — this
    /// records it durably as a `backfill_jobs` row and increments a
    /// dedicated metric before advancing, so the loss is queryable and
    /// alertable instead of visible only in a log line (issue #598).
    ///
    /// Takes `db`/`network` explicitly (rather than `&self`) so it can be
    /// exercised directly in a test without spinning up a whole `Streamer`.
    async fn recover_retained_floor(
        db: &PgPool,
        network: &str,
        cursor: u64,
        floor: u64,
        source_error: &TridentError,
    ) -> u64 {
        let skipped_from = cursor + 1;
        let skipped_to = floor.saturating_sub(1);

        if skipped_from <= skipped_to {
            let gap = db::LedgerGap {
                from_ledger: skipped_from,
                to_ledger: skipped_to,
            };
            if let Err(enqueue_err) = db::enqueue_backfill_job(db, gap, network).await {
                tracing::warn!(
                    from_ledger = skipped_from,
                    to_ledger = skipped_to,
                    error = %enqueue_err,
                    "Failed to enqueue backfill job for a retained-floor skip"
                );
            }
            metrics::record_retained_floor_ledgers_skipped(skipped_to - skipped_from + 1);
        }

        // page_request_params sends `cursor + 1`, so store floor - 1 to make
        // the next request anchor exactly at the oldest retained ledger.
        let new_cursor = floor.saturating_sub(1);
        tracing::warn!(
            error = %source_error,
            retained_floor = floor,
            cursor = new_cursor,
            skipped_from,
            skipped_to,
            "startLedger predates the RPC's retained history; advancing to the oldest retained ledger"
        );
        new_cursor
    }

    /// Start the polling loop. Runs until `shutdown` is cancelled, always
    /// finishing the current `poll_once` before stopping (never mid-batch).
    pub async fn run(&mut self, shutdown: CancellationToken) -> Result<(), TridentError> {
        tracing::info!(network = %self.config.network, "Streamer started");
        tracing::info!(
            "[indexer] poll interval: {}ms",
            self.config.poll_interval.as_millis()
        );
        tracing::info!(
            "[indexer] max events per poll: {}",
            self.config.max_events_per_poll
        );

        let mut cursor = db::get_cursor(&self.db, &self.config.network).await?;
        tracing::info!(cursor, "Resuming from ledger cursor");

        // Populate contract specs / interface tags once at startup (issues
        // #260, #269) so storage snapshotting (#270) has token classification
        // available from the first poll rather than waiting for the first
        // periodic refresh.
        self.sync_contract_specs().await;

        loop {
            // Check for shutdown before starting a new poll so we never begin
            // a batch we can't finish atomically.
            if shutdown.is_cancelled() {
                break;
            }

            // Dead-man's-switch: ticks once per loop iteration regardless of
            // poll outcome, so Prometheus can alert on a hung/crashed process
            // even when lag itself still looks fine.
            metrics::record_heartbeat();
            metrics::set_db_pool_stats(self.db.size(), self.db.num_idle() as u32);

            // Periodically refresh the contract allowlist so new contracts
            // become active without a restart (issue #47).
            self.poll_count = self.poll_count.wrapping_add(1);
            if self.poll_count.is_multiple_of(FILTER_REFRESH_EVERY_N_POLLS) {
                self.refresh_contract_filter().await?;
                self.sync_contract_specs().await;
            }

            // Periodically scan for gaps in the processed ledger range and
            // enqueue backfill jobs to close them (issue #216). Runs far less
            // often than every cycle (see GAP_SCAN_EVERY_N_POLLS) so it never
            // competes with live ingest for DB time; failures are logged and
            // skipped rather than propagated, since a missed scan is not a
            // reason to halt or retry the whole poll cycle — the next
            // scheduled scan will pick the gap up.
            if self.poll_count.is_multiple_of(GAP_SCAN_EVERY_N_POLLS) {
                self.scan_and_enqueue_gaps().await;
            }

            // Circuit breaker (issue #197): while Open, skip the poll body
            // entirely and let the loop fall through to the sleep below,
            // rather than spending an RPC round-trip (and its 5-attempt
            // retry budget) on an endpoint that has already told us it is
            // down. should_allow() flips Open -> HalfOpen once the cooldown
            // has elapsed, letting exactly one probe cycle through.
            metrics::set_rpc_breaker_state(self.rpc_breaker.state());
            metrics::set_rpc_breaker_consecutive_failures(self.rpc_breaker.consecutive_failures());
            if !self.rpc_breaker.should_allow() {
                tracing::warn!(
                    consecutive_failures = self.rpc_breaker.consecutive_failures(),
                    "RPC circuit breaker open; skipping poll cycle"
                );
            } else {
                let poll_span = tracing::info_span!("poll_cycle", cursor = cursor);
                match self.poll_once(&mut cursor).instrument(poll_span).await {
                    Ok(events_processed) => {
                        self.rpc_breaker.record(Outcome::Success);
                        if events_processed > 0 {
                            tracing::info!(events_processed, cursor, "Batch processed");
                        } else {
                            tracing::debug!(cursor, "No new events");
                        }
                    }
                    Err(e) => {
                        metrics::record_poll_error();
                        self.rpc_breaker.record(if e.is_rpc() {
                            Outcome::RpcFailure
                        } else {
                            Outcome::Success
                        });
                        // Branch on the structured classification: transient failures
                        // are retried on the next interval (the cursor is safe), poison
                        // input is skipped, and fatal errors halt the streamer.
                        match e.severity() {
                            Severity::Fatal => {
                                tracing::error!(error = %e, "Fatal error, halting streamer");
                                return Err(e);
                            }
                            Severity::Retryable => {
                                // A fresh index anchors at ledger 1, but the RPC
                                // prunes old ledgers, so on a network whose retained
                                // window has moved past 1 every poll is rejected
                                // identically and the cursor never advances —
                                // retrying alone can never clear it (issue #388).
                                // Adopt the floor the error reports so the next poll
                                // starts inside the retained window.
                                match parse_retained_floor(&e.to_string()) {
                                    Some(floor) if cursor < floor.saturating_sub(1) => {
                                        cursor = Self::recover_retained_floor(
                                            &self.db,
                                            &self.config.network,
                                            cursor,
                                            floor,
                                            &e,
                                        )
                                        .await;
                                    }
                                    _ => {
                                        tracing::warn!(error = %e, "Transient poll failure, will retry next interval");
                                    }
                                }
                            }
                            Severity::Skip => {
                                tracing::warn!(error = %e, "Non-retryable poll failure, skipping cycle");
                            }
                        }
                    }
                }
                metrics::set_rpc_breaker_state(self.rpc_breaker.state());
                metrics::set_rpc_breaker_consecutive_failures(
                    self.rpc_breaker.consecutive_failures(),
                );
            }

            // Derive the next poll interval from the current chain-tip lag:
            // poll fast while behind, back off once caught up (issue #198).
            let lag = self.last_chain_tip.saturating_sub(cursor);
            let interval = self.adaptive_poll.next_interval(lag);
            metrics::set_effective_poll_interval(interval.as_millis() as u64);

            // Stamp the heartbeat after every cycle — even failed ones — so the
            // gauge advances as long as the poll loop is alive (#218). A dead-man's
            // switch alert fires when `time() - gauge > threshold`.
            if let Ok(now) = SystemTime::now().duration_since(UNIX_EPOCH) {
                metrics::set_heartbeat_timestamp(now.as_secs_f64());
            }

            // Sleep until the next poll interval, waking immediately on shutdown.
            tokio::select! {
                _ = tokio::time::sleep(interval) => {}
                _ = shutdown.cancelled() => {
                    tracing::info!("Shutdown signal received, stopping after current poll");
                    break;
                }
            }
        }

        tracing::info!(cursor, "Streamer stopped cleanly; cursor persisted");
        Ok(())
    }

    /// Fetch and decode per-invocation fee + declared-resource metering for
    /// one transaction hash via `getTransaction` (issue #266).
    ///
    /// Best-effort: any RPC or decode failure is logged and treated as "no
    /// metrics for this transaction" rather than failing the poll cycle —
    /// metering is a value-add on top of event indexing, not a correctness
    /// requirement for it.
    async fn fetch_invocation_metrics(
        &self,
        tx_hash: &str,
    ) -> Option<crate::parser::invocation_metrics::InvocationMetrics> {
        let resp = match self.rpc.get_transaction(tx_hash).await {
            Ok(resp) => resp,
            Err(e) => {
                tracing::warn!(
                    tx_hash,
                    error = %e,
                    "getTransaction failed; skipping invocation metrics"
                );
                return None;
            }
        };

        if resp.status == "NOT_FOUND" {
            tracing::warn!(
                tx_hash,
                "getTransaction returned NOT_FOUND for a just-indexed event; skipping invocation metrics"
            );
            return None;
        }

        let (envelope_xdr, result_xdr) = match (resp.envelope_xdr, resp.result_xdr) {
            (Some(envelope), Some(result)) => (envelope, result),
            _ => {
                tracing::warn!(
                    tx_hash,
                    status = %resp.status,
                    "getTransaction response missing envelope/result XDR; skipping invocation metrics"
                );
                return None;
            }
        };

        match crate::parser::invocation_metrics::decode_invocation_metrics(
            &envelope_xdr,
            &result_xdr,
        ) {
            Ok(metrics) => Some(metrics),
            Err(e) => {
                tracing::warn!(tx_hash, error = %e, "Failed to decode invocation metrics");
                None
            }
        }
    }

    /// Resolve and cache SEP-41 token metadata for every distinct contract
    /// among `token_projections` whose `token_metadata` row is missing or
    /// older than `token_metadata_refresh_interval` (issue #263).
    ///
    /// Best-effort: an RPC/simulation failure for one contract is logged and
    /// skipped, leaving its row (if any) untouched so the next page carrying
    /// activity for that contract retries it — the poll cycle itself never
    /// fails because of this.
    async fn resolve_stale_token_metadata(&self, token_projections: &[db::TokenProjection<'_>]) {
        if token_projections.is_empty() {
            return;
        }

        let mut contract_ids: Vec<String> = token_projections
            .iter()
            .map(|p| p.event.contract_id.clone())
            .collect();
        contract_ids.sort_unstable();
        contract_ids.dedup();

        let cutoff = chrono::Utc::now()
            - chrono::Duration::from_std(self.config.token_metadata_refresh_interval)
                .unwrap_or(chrono::Duration::zero());
        let fresh = match db::fresh_token_metadata_contract_ids(
            &self.db,
            &contract_ids,
            &self.config.network,
            cutoff,
        )
        .await
        {
            Ok(fresh) => fresh,
            Err(e) => {
                tracing::warn!(error = %e, "Failed to load fresh token_metadata contract ids; skipping this cycle's resolution");
                return;
            }
        };

        for contract_id in contract_ids {
            if fresh.contains(&contract_id) {
                continue;
            }

            let resolution = match token_metadata::resolve(&self.rpc, &contract_id).await {
                Ok(resolution) => resolution,
                Err(e) => {
                    tracing::warn!(
                        contract_id = %contract_id,
                        error = %e,
                        "Failed to resolve token metadata; will retry on a future poll"
                    );
                    continue;
                }
            };

            if let Err(e) =
                db::upsert_token_metadata(&self.db, &contract_id, &self.config.network, &resolution)
                    .await
            {
                tracing::warn!(
                    contract_id = %contract_id,
                    error = %e,
                    "Failed to cache token metadata"
                );
            }
        }
    }

    /// Check for chain reorganisation / rollback before polling events (issue #196).
    ///
    /// Reorg conditions:
    /// 1. `latest_ledger < *cursor`: The chain tip reported by RPC regressed behind our current cursor.
    /// 2. Ledger hash mismatch: Compare recent stored ledger hashes from `ledger_metadata` against RPC `getLedgers`.
    ///
    /// If a reorg is detected:
    /// - Verify that the rewind depth does not exceed `config.max_reorg_depth`.
    /// - If it exceeds max depth, emit metric, log error, and return a fatal error.
    /// - If within bounds, atomically delete affected rows, rewind cursor in `system_state`, update `*cursor`,
    ///   record metric `metrics::record_reorg()`, and log structured warning.
    async fn check_and_handle_reorg(&mut self, cursor: &mut u64) -> Result<(), TridentError> {
        if *cursor == 0 {
            return Ok(());
        }

        let latest_ledger = match self.rpc.get_latest_ledger().await {
            Ok(seq) => seq,
            Err(e) => {
                tracing::debug!(error = %e, "Failed to fetch latest ledger for reorg check; will check on next poll");
                return Ok(());
            }
        };

        let mut reorg_start: Option<u64> = None;

        // Condition 1: Chain tip regressed behind current cursor
        if latest_ledger < *cursor {
            reorg_start = Some(latest_ledger + 1);
        } else {
            // Condition 2: Check stored ledger hashes against RPC for recent ledgers
            let check_count = (self.config.max_reorg_depth.min(10)) as i64;
            let recent_ledgers = db::get_recent_ledger_metadata(&self.db, check_count).await?;

            for (seq, stored_hash) in recent_ledgers {
                if let Ok(Some(rpc_hash)) = self.rpc.get_ledger(seq).await {
                    if rpc_hash != stored_hash {
                        tracing::warn!(
                            sequence = seq,
                            stored_hash = %stored_hash,
                            rpc_hash = %rpc_hash,
                            "Ledger hash mismatch detected indicating reorg"
                        );
                        reorg_start = Some(match reorg_start {
                            Some(existing) => existing.min(seq),
                            None => seq,
                        });
                    }
                }
            }
        }

        if let Some(reorg_seq) = reorg_start {
            let reorg_depth = cursor.saturating_sub(reorg_seq) + 1;

            if reorg_depth > self.config.max_reorg_depth {
                metrics::record_reorg();
                tracing::error!(
                    reorg_start = reorg_seq,
                    cursor = *cursor,
                    reorg_depth = reorg_depth,
                    max_reorg_depth = self.config.max_reorg_depth,
                    "Deep ledger reorganisation detected exceeding maximum allowed depth; halting indexer"
                );
                return Err(TridentError::config(anyhow::anyhow!(
                    "Deep reorg of depth {} exceeds max allowed depth {}",
                    reorg_depth,
                    self.config.max_reorg_depth
                )));
            }

            let new_cursor = reorg_seq.saturating_sub(1);
            tracing::warn!(
                reorg_start = reorg_seq,
                cursor_before = *cursor,
                new_cursor = new_cursor,
                reorg_depth = reorg_depth,
                "Ledger reorganisation detected; rolling back affected ledger data and rewinding cursor"
            );

            metrics::record_reorg();
            db::handle_reorg_rollback(&self.db, &self.config.network, reorg_seq, new_cursor)
                .await?;
            *cursor = new_cursor;
        }

        Ok(())
    }

    /// Execute a single poll cycle. Fetches all available pages from the RPC
    /// starting at `cursor`, persists each event, and advances the cursor.
    /// Returns the total number of events processed in this cycle.
    async fn poll_once(&mut self, cursor: &mut u64) -> Result<usize, TridentError> {
        self.check_and_handle_reorg(cursor).await?;

        let poll_start = Instant::now();
        // Cursor and lag as this cycle begins, so catch-up throughput can be
        // measured over the cycle (issue #420). `last_chain_tip` is the tip
        // observed by the previous cycle; the current cycle's first RPC page
        // refreshes it, but the deficit we were working against is this one.
        let cursor_at_start = *cursor;
        let lag_at_start = self.last_chain_tip.saturating_sub(cursor_at_start) as i64;

        // Reorg check (issue #196): before fetching anything new, confirm the
        // ledgers we already persisted still match what the RPC reports for
        // those sequences. Run before the partition-ranges query too, since a
        // reorg can change which range the cursor should resume from.
        self.check_and_handle_reorg(cursor).await?;

        // Query the named partition ranges once per poll cycle so every insert
        // in this cycle can check whether it would fall outside them and land
        // in the DEFAULT catch-all partition (issue #525).
        let partition_ranges = match db::named_partition_ranges(&self.db).await {
            Ok(ranges) if !ranges.is_empty() => {
                let highest = ranges.iter().map(|&(_, hi)| hi).max().unwrap_or(0);
                metrics::set_partition_lookahead(highest.saturating_sub(*cursor as i64));
                ranges
            }
            Ok(_) => {
                // No named partitions at all — every insert would land in
                // DEFAULT. This is a real misconfiguration, not a blip.
                return Err(TridentError::config(anyhow::anyhow!(
                    "partition exhaustion: soroban_events has no named range partitions.                      All inserts would land in soroban_events_default.                      Run `SELECT create_soroban_partition(0, 2000000);` to create the                      first partition (issue #525)."
                )));
            }
            Err(e) => {
                // A failed catalogue query is usually a transient connection
                // problem. Returning a config error here would classify it as
                // Fatal and halt ingestion permanently, so surface it as the
                // storage error it is and let the caller's retry path handle it.
                return Err(TridentError::storage(anyhow::Error::new(e).context(
                    "could not query soroban_events partition ranges (issue #525)",
                )));
            }
        };
        // Full jitter (issue #197): without it, every indexer replica computes
        // the identical backoff schedule and retries in lockstep against the
        // same RPC endpoint on a shared outage.
        let retry_strategy = retry_strategy();

        // The first page of a poll anchors by ledger (startLedger); every later
        // page in the same poll resumes via the RPC paging token. A fresh index
        // (cursor 0) starts at ledger 1 (or the minimum index_from across all
        // tracked contracts, whichever is higher); a resume starts at the ledger
        // after the last one we fully processed. startLedger and cursor are
        // mutually exclusive in the Soroban RPC, so only one is ever sent per
        // request.
        //
        // When per-contract index_from values are set, we compute an effective
        // cursor so that the first startLedger respects the earliest index_from,
        // avoiding unnecessary scanning of ledgers that will produce no events
        // for any tracked contract (issue #202).
        let effective_cursor = if *cursor == 0 {
            match &self.contract_filter {
                Some(filter) => {
                    // index_from is a BIGINT and cursors are u64; clamp at 0 so
                    // a negative or unset value cannot wrap on conversion.
                    let min_from = filter.values().copied().min().unwrap_or(0).max(0) as u64;
                    // Set effective_cursor to min_from - 1 so page_request_params
                    // returns startLedger = min_from. Saturating so 0 and 1 both
                    // yield 0 rather than wrapping.
                    min_from.saturating_sub(1)
                }
                None => 0,
            }
        } else {
            *cursor
        };
        let mut page_cursor: Option<String> = None;
        let mut total = 0;

        // Snapshot the server-side filters for the whole cycle (issue #203) so
        // the request shape is stable across pages and the borrow does not
        // conflict with the mutable state updated inside the loop.
        let filters = self.filter_plan.filters.clone();

        loop {
            let (sl, pc) = page_request_params(effective_cursor, page_cursor.as_deref());
            let mut attempt = 0u32;
            let limit = self.config.max_events_per_poll;
            // Server-side narrowing (issue #203). Empty in index-all mode; the
            // client-side allowlist check below stays as the safety net.
            let filters = filters.as_slice();
            let page = Retry::start(retry_strategy.clone(), || {
                attempt += 1;
                if attempt > 1 {
                    metrics::record_rpc_retry();
                }
                async { self.rpc.get_events(sl, pc.clone(), limit, filters).await }
                    .instrument(tracing::info_span!("rpc_get_events"))
            })
            .await?;

            tracing::debug!(
                latest_ledger = page.latest_ledger,
                cursor = *cursor,
                "RPC page received"
            );

            metrics::set_ledger_lag(page.latest_ledger.saturating_sub(*cursor) as i64);
            self.last_chain_tip = page.latest_ledger;

            if page.events.is_empty() {
                break;
            }

            let last_paging_token = page.events.last().map(|e| e.page_cursor());

            let mut events_in_page: i32 = 0;
            let mut skipped_in_page: u64 = 0;
            // Accumulate the page and commit it in one transaction (issue #199)
            // rather than paying a round-trip per row.
            let mut page_events: Vec<trident_common::SorobanEvent> =
                Vec::with_capacity(page.events.len());
            // Token projections keyed by position in `page_events` (issue #211).
            // Indices, not references, because `page_events` is still growing.
            let mut page_tokens: Vec<(usize, crate::parser::token_events::TokenEvent)> = Vec::new();
            for raw in &page.events {
                let decode_start = Instant::now();
                let parse_result = {
                    let _span = tracing::info_span!("parse_events").entered();
                    self.parser.parse_event_with_projection(raw)
                };
                metrics::record_decode_duration(decode_start.elapsed().as_secs_f64());

                match parse_result {
                    Ok(Some(parsed)) => {
                        let event = parsed.event;
                        // Contract allowlist filtering (issues #47, #202).
                        // None → index all; Some(map) → only listed contracts,
                        // and only at or above their per-contract index_from.
                        if let Some(ref filter) = self.contract_filter {
                            match filter.get(&event.contract_id) {
                                None => {
                                    tracing::trace!(
                                        contract_id = %event.contract_id,
                                        "Skipping event from unlisted contract"
                                    );
                                    // Unlisted contracts land in the "other" bucket to
                                    // bound cardinality (issue #212).
                                    metrics::record_events_by_contract("other", 1);
                                    skipped_in_page += 1;
                                    continue;
                                }
                                Some(&index_from)
                                    if (event.ledger_sequence as i64) < index_from =>
                                {
                                    tracing::trace!(
                                        contract_id = %event.contract_id,
                                        ledger = event.ledger_sequence,
                                        index_from = index_from,
                                        "Skipping event below contract index_from"
                                    );
                                    // Listed, but below its start ledger — still a
                                    // skip, so bucket it the same way.
                                    metrics::record_events_by_contract("other", 1);
                                    skipped_in_page += 1;
                                    continue;
                                }
                                _ => {}
                            }
                        }
                        // Allowlisted or index-all: record under the real contract_id.
                        // In index-all mode cardinality is unbounded — operators should
                        // configure an allowlist if per-contract metrics are needed.
                        metrics::record_events_by_contract(&event.contract_id, 1);
                        // Events are accumulated and committed as one page,
                        // together with their outbox rows; the relay owns the
                        // Redis publish (issues #199, #200). Publishing inline
                        // here would reintroduce the lost-event window: a crash
                        // after the commit but before the XADD dropped the event
                        // for live subscribers with no replay path.
                        if let Some(token) = parsed.token {
                            page_tokens.push((page_events.len(), token));
                        }
                        page_events.push(event);
                        total += 1;
                        events_in_page += 1;
                    }
                    Ok(None) => {
                        // diagnostic or failed-call event — intentionally skipped
                        skipped_in_page += 1;
                    }
                    Err(e) => {
                        tracing::warn!(
                            tx_hash = %raw.tx_hash,
                            error = %e,
                            "Skipping unparseable event"
                        );
                        metrics::record_parse_error();
                        let ledger_seq: u64 = raw.ledger.parse().unwrap_or(0);
                        // Same derivation as the happy path so a parse_errors
                        // row points at the event it actually came from.
                        let event_idx: u32 = crate::parser::raw_event_index(raw);
                        let raw_payload = serde_json::to_string(&serde_json::json!({
                            "type": &raw.event_type,
                            "ledger": &raw.ledger,
                            "ledgerClosedAt": &raw.ledger_closed_at,
                            "contractId": &raw.contract_id,
                            "id": &raw.id,
                            "topic": &raw.topic,
                            "value": &raw.value,
                        }))
                        .unwrap_or_else(|_| "{}".to_string());
                        // Retry dead-letter insert with bounded backoff so a
                        // transient DB hiccup does not lose the audit record
                        // (issue #414).
                        let db = self.db.clone();
                        let payload = raw_payload.clone();
                        let errmsg = e.to_string();
                        let dead_letter_strategy = ExponentialBackoff::from_millis(100)
                            .max_delay(Duration::from_secs(1))
                            .take(3);
                        if let Err(db_err) = Retry::start(dead_letter_strategy, || {
                            let db = db.clone();
                            let payload = payload.clone();
                            let errmsg = errmsg.clone();
                            async move {
                                db::insert_parse_error(
                                    &db, ledger_seq, event_idx, &payload, &errmsg,
                                )
                                .await
                            }
                        })
                        .await
                        {
                            tracing::error!(
                                error = %db_err,
                                "Failed to record parse error in database after retries"
                            );
                        } else {
                            // Only count a dead-letter once the row is durably
                            // recorded (issue #414). Incrementing on the failure
                            // path instead would make the alert fire for events
                            // that were never actually captured for replay.
                            metrics::record_dead_lettered();
                        }
                        skipped_in_page += 1;
                    }
                }
            }

            metrics::record_events_processed(events_in_page as u64);
            metrics::record_events_skipped(skipped_in_page);

            // Refresh the dead-letter backlog gauge every active cycle so an
            // operator replay (which shrinks the queue out-of-band) is
            // reflected without restarting the indexer. Best-effort: the
            // gauge is observability, not control flow.
            if let Ok(depth) = db::count_pending_failed_events(&self.db).await {
                metrics::set_persist_dead_letter_backlog(depth);
            }

            // Decide whether this page advances the cursor, and gather the
            // ledger provenance that must land in the same transaction.
            let mut next_cursor: Option<u64> = None;
            let mut ledger_hash = String::new();
            let mut ledger_timestamp = String::new();
            let mut ledger_sequence = 0u64;

            if let Some(last) = page.events.last() {
                let seq: u64 = last.ledger.parse().unwrap_or(*cursor);
                if seq > *cursor {
                    next_cursor = Some(seq);
                    ledger_sequence = seq;
                    ledger_timestamp = last.ledger_closed_at.clone();

                    // Fetch the real ledger hash from getLedgers RPC.
                    // Non-critical: log a warning on failure, store empty string.
                    ledger_hash = match self.rpc.get_ledger(seq).await {
                        Ok(Some(h)) => h,
                        Ok(None) => {
                            tracing::warn!(seq, "getLedgers returned no ledger for sequence");
                            String::new()
                        }
                        Err(e) => {
                            tracing::warn!(seq, error = %e, "getLedgers failed, storing empty hash");
                            String::new()
                        }
                    };
                }
            }

            // Guarantee the natural key from migration 0025 holds across this
            // batch before it reaches Postgres. Mutates event_index in place
            // only, so the positional indices in `page_tokens` stay valid
            // (issue #388).
            crate::parser::assign_unique_event_indexes(&mut page_events);

            // Partition boundary guard (issue #525): verify no event in this
            // page would overflow into soroban_events_default before we touch
            // the database. Uses the boundary queried at the top of this cycle
            // so there is no extra round-trip per page.
            //
            // This returns TridentError::ConfigError (Severity::Fatal), which
            // causes the poll loop to halt and log an ERROR rather than
            // silently retrying — the intended loud-failure behaviour for
            // partition exhaustion.
            if !page_events.is_empty() {
                let ledger_sequences: Vec<i64> = page_events
                    .iter()
                    .map(|e| e.ledger_sequence as i64)
                    .collect();
                db::assert_no_default_partition_overflow(&ledger_sequences, &partition_ranges)?;
            }

            // One transaction for the whole page: events, cursor, and ledger
            // metadata land together or not at all, so a crash can never leave
            // the cursor ahead of the events it claims to cover (issue #199).
            // Resolve the projection indices now that `page_events` is final.
            let token_projections: Vec<db::TokenProjection<'_>> = page_tokens
                .iter()
                .map(|(index, token)| db::TokenProjection {
                    event: &page_events[*index],
                    token,
                })
                .collect();

            // Per-invocation fee + declared-resource metering for tracked
            // contracts (issue #266). Every event in `page_events` already
            // passed the allowlist check above, so this never runs unbounded
            // index-all fan-out (see docs/contract-invocation-metering.md).
            //
            // Two passes over `page_events`: fetching (pass 1) mutates
            // `tx_metrics`, and building the rows (pass 2) borrows from it —
            // keeping those separate avoids holding a long-lived immutable
            // borrow into the map across a later mutable insert.
            let mut tx_metrics: std::collections::HashMap<
                &str,
                Option<crate::parser::invocation_metrics::InvocationMetrics>,
            > = std::collections::HashMap::new();
            let mut invocation_metrics: Vec<db::InvocationMetricRow<'_>> = Vec::new();
            if self.contract_filter.is_some() {
                for event in &page_events {
                    if !tx_metrics.contains_key(event.transaction_hash.as_str()) {
                        let decoded = self.fetch_invocation_metrics(&event.transaction_hash).await;
                        tx_metrics.insert(event.transaction_hash.as_str(), decoded);
                    }
                }

                let mut seen_pairs: std::collections::HashSet<(&str, &str)> =
                    std::collections::HashSet::new();
                for event in &page_events {
                    let pair = (event.contract_id.as_str(), event.transaction_hash.as_str());
                    if !seen_pairs.insert(pair) {
                        continue;
                    }
                    if let Some(Some(metrics)) = tx_metrics.get(event.transaction_hash.as_str()) {
                        invocation_metrics.push(db::InvocationMetricRow {
                            contract_id: &event.contract_id,
                            transaction_hash: &event.transaction_hash,
                            ledger_sequence: event.ledger_sequence,
                            ledger_timestamp: &event.ledger_timestamp,
                            metrics,
                        });
                    }
                }
            }

            // Contract storage snapshots (issue #270): bounded to tracked
            // contracts already classified as SEP-41 tokens (issue #269), and
            // only for holder addresses that moved funds in this page — never
            // a scan of arbitrary storage.
            let mut owned_storage_snapshots: Vec<OwnedStorageSnapshot> = Vec::new();
            if !self.token_contracts.is_empty() {
                let mut holders_by_contract: std::collections::HashMap<
                    &str,
                    std::collections::HashSet<&str>,
                > = std::collections::HashMap::new();
                for (index, token) in &page_tokens {
                    let contract_id = page_events[*index].contract_id.as_str();
                    if !self.token_contracts.contains(contract_id) {
                        continue;
                    }
                    let holders = holders_by_contract.entry(contract_id).or_default();
                    if let Some(from) = &token.from {
                        holders.insert(from.as_str());
                    }
                    if let Some(to) = &token.to {
                        holders.insert(to.as_str());
                    }
                }

                let snapshot_ledger = if ledger_sequence > 0 {
                    ledger_sequence
                } else {
                    *cursor
                };
                for (contract_id, holders) in holders_by_contract {
                    let holder_list: Vec<String> =
                        holders.into_iter().map(str::to_string).collect();
                    let observations = match crate::storage::fetch_balance_snapshots(
                        &self.rpc,
                        contract_id,
                        &holder_list,
                    )
                    .await
                    {
                        Ok(obs) => obs,
                        Err(e) => {
                            tracing::warn!(contract_id, error = %e, "Failed to fetch storage snapshot");
                            continue;
                        }
                    };

                    for obs in observations {
                        let last = db::get_latest_storage_value(
                            &self.db,
                            contract_id,
                            &self.config.network,
                            &obs.storage_key,
                        )
                        .await
                        .unwrap_or(None);

                        if last != obs.value_json {
                            owned_storage_snapshots.push(OwnedStorageSnapshot {
                                contract_id: contract_id.to_string(),
                                storage_key: obs.storage_key,
                                key_json: obs.key_json,
                                value_json: obs.value_json,
                                ledger_sequence: snapshot_ledger,
                            });
                        }
                    }
                }
            }
            let storage_snapshots: Vec<db::StorageSnapshotRow<'_>> = owned_storage_snapshots
                .iter()
                .map(|s| db::StorageSnapshotRow {
                    contract_id: &s.contract_id,
                    storage_key: &s.storage_key,
                    key_json: &s.key_json,
                    value_json: s.value_json.as_ref(),
                    ledger_sequence: s.ledger_sequence,
                })
                .collect();

            commit_page_with_fallback(
                &self.db,
                &page_events,
                &page_tokens,
                &invocation_metrics,
                &storage_snapshots,
                &self.config.network,
                next_cursor,
                next_cursor.map(|_| db::LedgerMeta {
                    sequence: ledger_sequence,
                    hash: &ledger_hash,
                    timestamp: &ledger_timestamp,
                    event_count: events_in_page,
                }),
                self.config.db_batch_size,
            )
            .instrument(tracing::info_span!(
                "db_commit_page",
                events = page_events.len()
            ))
            .await?;

            if let Some(seq) = next_cursor {
                *cursor = seq;
            }

            // Resolve + cache SEP-41 token metadata for any contract seen in
            // this page's token events whose cached row is missing or stale
            // (issue #263). Best-effort, like `fetch_invocation_metrics`
            // above: a resolution failure is logged and skipped rather than
            // failing the poll cycle.
            self.resolve_stale_token_metadata(&token_projections)
                .instrument(tracing::info_span!("resolve_token_metadata"))
                .await;

            // Delivery is not done here. The commit above wrote an outbox row
            // per event, and `redis_stream::relay` publishes them (issue #200).
            // Publishing inline would still lose events: a crash between the
            // commit and the XADD leaves the event in Postgres and off the
            // stream, with nothing to replay it.

            // An incomplete page means we have caught up to the chain tip.
            if page.events.len() < self.config.max_events_per_poll as usize {
                break;
            }

            page_cursor = last_paging_token;
        }

        // Recompute lag once the loop settles so it reflects the final cursor
        // relative to the chain tip (zero once we have caught up).
        metrics::set_ledger_lag(self.last_chain_tip.saturating_sub(*cursor) as i64);

        // Write health stats after every successful cycle (issue #62).
        // Non-fatal: log on failure so a bad health write doesn't stop indexing.
        let poll_duration = poll_start.elapsed();
        metrics::record_poll_duration(poll_duration.as_secs_f64());

        // Catch-up throughput for this cycle (issue #420). Published only while
        // meaningfully behind the tip — see `set_catchup_rates` — so the gauges
        // describe backfill speed rather than steady-state tip-following.
        metrics::set_catchup_rates(
            cursor.saturating_sub(cursor_at_start),
            total as u64,
            poll_duration.as_secs_f64(),
            lag_at_start,
        );
        if let Err(e) = db::update_health_stats(
            &self.db,
            &self.config.network,
            *cursor as i64,
            total as i32,
            poll_duration,
        )
        .await
        {
            tracing::warn!(error = %e, "Failed to update health stats");
        }

        // Alerting (issue #75) — best-effort, never aborts the poll cycle.
        if self.alerter.is_enabled() {
            match db::get_alert_state(&self.db, &self.config.network).await {
                Ok(mut alert_state) => {
                    // issue #605: checked on the same cadence as lag/RPC-degraded.
                    // A query failure (other than the view being absent, which
                    // default_partition_row_count itself already turns into
                    // Ok(None)) must not abort alerting for the checks above —
                    // logged and treated as "no signal this cycle".
                    let default_partition_row_count =
                        match db::default_partition_row_count(&self.db).await {
                            Ok(count) => count,
                            Err(e) => {
                                tracing::warn!(
                                    error = %e,
                                    "Failed to read soroban_events_default row count"
                                );
                                None
                            }
                        };
                    let ctx = AlertContext {
                        last_ledger_indexed: *cursor,
                        chain_tip_ledger: self.last_chain_tip,
                        lag_threshold: self.config.alert_lag_threshold,
                        network: self.config.network.clone(),
                        rpc_all_degraded: self.rpc.health_scorer().all_degraded(),
                        default_partition_row_count,
                    };
                    self.alerter.evaluate(&ctx, &mut alert_state).await;
                    if let Err(e) =
                        db::set_alert_state(&self.db, &self.config.network, &alert_state).await
                    {
                        tracing::warn!(error = %e, "Failed to persist alert state");
                    }
                }
                Err(e) => {
                    tracing::warn!(error = %e, "Failed to read alert state");
                }
            }
        }

        Ok(total)
    }
}

/// Build the server-side `getEvents` filter plan for the current allowlist and
/// log how the request will be narrowed (issue #203).
///
/// A `degraded` plan means the allowlist is too large to express within the
/// RPC's filter caps, so we index everything and rely on the client-side
/// allowlist check in `poll_once` — correct, just less efficient.
/// Build the server-side filter plan from the allowlist.
///
/// Takes the `index_from` map but uses only its keys: `getEvents` carries a
/// single `startLedger` for the whole request, so per-contract boundaries
/// cannot be pushed down and are applied client-side in `poll_once`
/// (issues #202, #203).
fn plan_filters(
    allowlist: Option<&HashMap<String, i64>>,
    topic_filters: &[Vec<String>],
) -> FilterPlan {
    let contract_ids: Option<HashSet<String>> = allowlist.map(|map| map.keys().cloned().collect());
    let plan = build_event_filters(contract_ids.as_ref(), topic_filters);

    if plan.degraded {
        tracing::warn!(
            contracts = allowlist.map(|s| s.len()).unwrap_or(0),
            max = crate::rpc::filters::MAX_FILTERABLE_CONTRACTS,
            "Allowlist exceeds the RPC filter caps; falling back to index-all with client-side filtering"
        );
    } else if plan.filters.is_empty() {
        tracing::info!("No contract allowlist; requesting all events (index-all mode)");
    } else {
        tracing::info!(
            filters = plan.filters.len(),
            contracts = allowlist.map(|s| s.len()).unwrap_or(0),
            topic_patterns = topic_filters.len(),
            "Server-side getEvents filtering active"
        );
    }

    plan
}

/// Decide the `(startLedger, cursor)` params for a single RPC page request.
///
/// `startLedger` and `cursor` are mutually exclusive in the Soroban `getEvents`
/// RPC, so exactly one is ever `Some`:
///   - later pages of a poll (`page_cursor` set) → resume by paging token only
///   - first page, fresh index (`cursor == 0`)   → anchor at ledger 1
///   - first page, resume (`cursor == N`)        → anchor at ledger `N + 1`,
///     i.e. the ledger after the last one fully processed (never re-scan `N`,
///     never send the ledger number in the `cursor` field)
fn page_request_params(cursor: u64, page_cursor: Option<&str>) -> (Option<u64>, Option<String>) {
    match page_cursor {
        Some(token) => (None, Some(token.to_string())),
        None if cursor == 0 => (Some(1), None),
        None => (Some(cursor + 1), None),
    }
}

/// Extract the oldest retained ledger from an out-of-range `getEvents` error.
///
/// The Soroban RPC only keeps a recent window of ledgers. Asking for one it has
/// already pruned fails with, verbatim:
///
/// ```text
/// getEvents: RPC error -32600: startLedger must be within the ledger range: 7 - 457
/// ```
///
/// There is no machine-readable field for the retained range — the same
/// limitation noted for out-of-range cursors in `RpcClient::execute` — so the
/// message is the only place the floor is available. Returns the lower bound
/// (`7` above), or `None` when this is some other error.
///
/// Matching is deliberately narrow: both the `ledger range` phrase and a
/// `<low> - <high>` pair must be present, so an unrelated RPC error can never
/// be mistaken for a retention signal and silently move the cursor.
fn parse_retained_floor(message: &str) -> Option<u64> {
    let lower = message.to_lowercase();
    if !lower.contains("ledger range") {
        return None;
    }
    let after = &lower[lower.find("ledger range")? + "ledger range".len()..];
    let (low, high) = after
        .split_once('-')
        .map(|(l, r)| (l.trim_matches(|c: char| !c.is_ascii_digit()), r))?;
    let high: String = high
        .trim_start()
        .chars()
        .take_while(|c| c.is_ascii_digit())
        .collect();
    let low: u64 = low.parse().ok()?;
    let high: u64 = high.parse().ok()?;
    // A well-formed range only; anything inverted means the message shape
    // changed and the value should not be trusted.
    (low <= high).then_some(low)
}

/// Commit a page atomically when possible, and fall back to per-event
/// isolation when it is not — so one event that cannot be persisted can never
/// wedge the cursor and block every other, otherwise-valid event in the page
/// behind it (issue #208).
///
/// Three stages:
///
/// 1. Try the whole page as one transaction (the fast path — `db::commit_page`
///    already batches this efficiently), with a few retries on failure. Most
///    failures are transient (a connection blip, a lock wait, a statement
///    timeout) and clear well within this budget.
/// 2. If the whole page still cannot be committed atomically, commit each
///    event individually instead. "An event that keeps failing even in
///    isolation, with nothing else in its transaction that could be the real
///    cause, is genuinely unpersistable right now" only holds when the
///    failure is specific to that event — during a failover, lock storm, or
///    pool exhaustion, every event fails identically in isolation too. Stage
///    3 below is what actually tells the two apart, via
///    `db::classify_storage_failure` rather than guessing at error strings
///    (issue #573).
/// 3. An event still failing after per-event retries is classified: a
///    transient storage failure (the database, not the row, is the problem)
///    propagates immediately so the whole page is retried on the next poll —
///    duplicates are already absorbed downstream by the deterministic
///    UUIDv5 keys and `ON CONFLICT DO NOTHING`, so a retry is safe in a way
///    dead-lettering healthy events is not. A permanent failure (a
///    constraint violation, a data-type error) is written to `failed_events`
///    (a dead-letter queue analogous to `parse_errors`, but for well-formed
///    events whose storage write failed rather than events that failed to
///    decode) and skipped, so the page's cursor/ledger metadata commit — and
///    therefore progress — is never blocked on it.
///
/// `invocation_metrics` and `storage_snapshots` are supplementary projections
/// keyed by (contract, transaction) rather than by individual event; they are
/// not meaningful to split per-event, so in the fallback path they ride with
/// the final cursor/ledger commit instead.
#[allow(clippy::too_many_arguments)]
async fn commit_page_with_fallback(
    db: &PgPool,
    page_events: &[trident_common::SorobanEvent],
    page_tokens: &[(usize, crate::parser::token_events::TokenEvent)],
    invocation_metrics: &[db::InvocationMetricRow<'_>],
    storage_snapshots: &[db::StorageSnapshotRow<'_>],
    network: &str,
    cursor: Option<u64>,
    ledger: Option<db::LedgerMeta<'_>>,
    batch_size: usize,
) -> Result<(), TridentError> {
    let token_projections: Vec<db::TokenProjection<'_>> = page_tokens
        .iter()
        .map(|(index, token)| db::TokenProjection {
            event: &page_events[*index],
            token,
        })
        .collect();

    let whole_page_strategy = ExponentialBackoff::from_millis(200)
        .max_delay(Duration::from_secs(2))
        .take(3);
    let whole_page_result = Retry::start(whole_page_strategy, || async {
        db::commit_page(
            db,
            db::PageCommit {
                events: page_events,
                token_events: &token_projections,
                invocation_metrics,
                storage_snapshots,
                network,
                cursor,
                ledger: ledger.as_ref().map(|l| db::LedgerMeta {
                    sequence: l.sequence,
                    hash: l.hash,
                    timestamp: l.timestamp,
                    event_count: l.event_count,
                }),
                batch_size,
            },
        )
        .await
    })
    .await;

    let whole_page_err = match whole_page_result {
        Ok(()) => return Ok(()),
        Err(e) => e,
    };
    tracing::warn!(
        error = %whole_page_err,
        events = page_events.len(),
        "Whole-page commit failed after retries; falling back to per-event isolation"
    );

    let token_by_index: HashMap<usize, &crate::parser::token_events::TokenEvent> =
        page_tokens.iter().map(|(i, t)| (*i, t)).collect();

    for (index, event) in page_events.iter().enumerate() {
        let single_event = std::slice::from_ref(event);
        let single_token: Vec<db::TokenProjection<'_>> = token_by_index
            .get(&index)
            .map(|token| vec![db::TokenProjection { event, token }])
            .unwrap_or_default();

        let per_event_strategy = ExponentialBackoff::from_millis(100)
            .max_delay(Duration::from_secs(1))
            .take(3);
        let mut attempts = 0u32;
        let result = Retry::start(per_event_strategy, || {
            attempts += 1;
            async {
                db::commit_page(
                    db,
                    db::PageCommit {
                        events: single_event,
                        token_events: &single_token,
                        invocation_metrics: &[],
                        storage_snapshots: &[],
                        network,
                        cursor: None,
                        ledger: None,
                        batch_size,
                    },
                )
                .await
            }
        })
        .await;

        if let Err(e) = result {
            // Distinguish a poison event from a database that is merely
            // unavailable right now (issue #573). Per-event isolation's
            // inference — "an event that fails even alone is unpersistable" —
            // only holds when the failure is specific to that event. During a
            // failover, lock storm, or pool exhaustion every event in the
            // page fails identically in isolation, and without this check the
            // whole (otherwise healthy) page was dead-lettered wholesale
            // instead of retried.
            if db::classify_storage_failure(&e) == db::StorageFailure::Transient {
                tracing::warn!(
                    contract_id = %event.contract_id,
                    tx_hash = %event.transaction_hash,
                    ledger = event.ledger_sequence,
                    error = %e,
                    attempts,
                    "Event failed to persist after per-event retries with a transient storage \
                     error; propagating instead of dead-lettering so the whole page retries"
                );
                return Err(e);
            }

            tracing::error!(
                contract_id = %event.contract_id,
                tx_hash = %event.transaction_hash,
                ledger = event.ledger_sequence,
                error = %e,
                attempts,
                "Event failed to persist after per-event retries; dead-lettering"
            );
            match db::insert_failed_event(db, event, &e.to_string(), attempts).await {
                Ok(()) => {
                    // The PERSIST counter, not the parse one: these answer
                    // different operational questions (issue #508), and the
                    // backlog gauge refreshes immediately so the alert sees
                    // the new row without waiting for the next active cycle.
                    metrics::record_persist_dead_lettered();
                    if let Ok(depth) = db::count_pending_failed_events(db).await {
                        metrics::set_persist_dead_letter_backlog(depth);
                    }
                }
                Err(dl_err) => {
                    // The dead-letter write goes to the same database the
                    // event's own INSERT just failed against. Failing here is
                    // therefore evidence that the *database* is unavailable,
                    // not that this event is unpersistable — the inference the
                    // isolation retry relies on does not hold.
                    //
                    // Swallowing this and letting the cursor advance below
                    // would discard the event permanently: absent from
                    // soroban_events, absent from failed_events, and the
                    // cursor moved past it. Propagating instead leaves the
                    // cursor where it is so the next poll retries the page —
                    // duplicates are already absorbed downstream by the
                    // deterministic UUIDv5 keys and ON CONFLICT DO NOTHING,
                    // so a retry is safe in a way that data loss is not.
                    tracing::error!(
                        error = %dl_err,
                        contract_id = %event.contract_id,
                        tx_hash = %event.transaction_hash,
                        "Failed to write failed_events row; refusing to advance the cursor past an unpersisted event"
                    );
                    return Err(dl_err);
                }
            }
        }
    }

    // Every event has now either been persisted or dead-lettered, so the
    // page's cursor/ledger advance (and the supplementary invocation-metrics
    // and storage-snapshot projections) can commit on their own.
    db::commit_page(
        db,
        db::PageCommit {
            events: &[],
            token_events: &[],
            invocation_metrics,
            storage_snapshots,
            network,
            cursor,
            ledger,
            batch_size,
        },
    )
    .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::redis_stream::relay::{OutboxRelay, RelayConfig};
    use crate::rpc::jitter;
    use base64::{engine::general_purpose::STANDARD, Engine};
    use stellar_xdr::curr::{Limited, Limits, ScSymbol, ScVal, WriteXdr};
    use wiremock::matchers::{body_partial_json, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    // Return the (db, redis) URLs, or skip the test when they are absent.
    // When REQUIRE_TEST_SERVICES is set (the rust-integration CI job sets it),
    // a missing URL is a hard failure instead of a silent skip — otherwise a
    // misconfigured integration job would go green without running anything.
    macro_rules! require_services {
        () => {{
            let required = std::env::var("REQUIRE_TEST_SERVICES").is_ok();
            match (
                std::env::var("TEST_DATABASE_URL"),
                std::env::var("TEST_REDIS_URL"),
            ) {
                (Ok(db), Ok(rd)) => (db, rd),
                _ if required => panic!(
                    "TEST_DATABASE_URL and TEST_REDIS_URL must be set when REQUIRE_TEST_SERVICES is set"
                ),
                _ => {
                    eprintln!("SKIP: TEST_DATABASE_URL / TEST_REDIS_URL not set");
                    return;
                }
            }
        }};
    }

    // Pure unit tests for the pagination decision — no services required, so
    // these run in the plain `rust` CI job as well as the integration job.
    #[test]
    fn page_params_fresh_index_anchors_at_ledger_1() {
        assert_eq!(page_request_params(0, None), (Some(1), None));
    }

    #[test]
    fn page_params_resume_anchors_at_next_ledger_not_cursor_field() {
        // Regression: a resume must send startLedger = cursor + 1, never the
        // ledger number in the (paging-token) cursor field.
        assert_eq!(page_request_params(100, None), (Some(101), None));
    }

    #[test]
    fn page_params_later_pages_use_paging_token_only() {
        // Regression: once paging, startLedger must be cleared (the two params
        // are mutually exclusive in the RPC).
        assert_eq!(
            page_request_params(100, Some("100-5")),
            (None, Some("100-5".to_string()))
        );
    }

    #[test]
    fn retained_floor_parsed_from_the_real_rpc_message() {
        // Verbatim from the E2E contract events job (issue #388): a fresh index
        // anchored at ledger 1 against a network retaining only 7 onwards.
        assert_eq!(
            parse_retained_floor(
                "getEvents: RPC error -32600: startLedger must be within the ledger range: 7 - 457"
            ),
            Some(7)
        );
    }

    #[test]
    fn retained_floor_ignores_unrelated_rpc_errors() {
        // Anything that is not a retention complaint must not move the cursor.
        assert_eq!(
            parse_retained_floor("getEvents: RPC error -32602: invalid cursor"),
            None
        );
        assert_eq!(parse_retained_floor("getEvents: empty result"), None);
        assert_eq!(parse_retained_floor(""), None);
    }

    #[test]
    fn retained_floor_rejects_a_malformed_range() {
        // An inverted or truncated range means the message shape changed; the
        // value must not be trusted rather than silently skipping ledgers.
        assert_eq!(
            parse_retained_floor("startLedger must be within the ledger range: 500 - 7"),
            None
        );
        assert_eq!(
            parse_retained_floor("startLedger must be within the ledger range: 7"),
            None
        );
    }

    #[test]
    fn retained_floor_maps_to_a_cursor_that_anchors_on_the_floor() {
        // The recovery stores floor - 1 because page_request_params sends
        // cursor + 1; the next request must land exactly on the floor, not
        // one past it (which would skip the oldest retained ledger).
        let floor =
            parse_retained_floor("startLedger must be within the ledger range: 7 - 457").unwrap();
        assert_eq!(page_request_params(floor - 1, None), (Some(7), None));
    }

    /// #598: a retained-floor recovery must durably record the ledgers it
    /// skips as a backfill_jobs row, not just a log line, so the range is
    /// queryable and a backfill can target it afterwards.
    #[tokio::test]
    async fn recover_retained_floor_enqueues_the_skipped_range_as_a_backfill_job() {
        let db_url = match std::env::var("TEST_DATABASE_URL") {
            Ok(url) => url,
            Err(_) if std::env::var("REQUIRE_TEST_SERVICES").is_ok() => {
                panic!("TEST_DATABASE_URL must be set when REQUIRE_TEST_SERVICES is set");
            }
            Err(_) => {
                eprintln!("SKIP: TEST_DATABASE_URL not set");
                return;
            }
        };
        let pool = sqlx::PgPool::connect(&db_url).await.unwrap();
        let network = format!("retainedfloortest-{}", uuid::Uuid::new_v4());

        let old_cursor = 100u64;
        let floor = 151u64; // RPC retains from 151 onward.
        let source_error = TridentError::rpc(anyhow::anyhow!(
            "getEvents: RPC error -32600: startLedger must be within the ledger range: 151 - 999"
        ));

        let new_cursor =
            Streamer::recover_retained_floor(&pool, &network, old_cursor, floor, &source_error)
                .await;

        // floor - 1, so the next poll's cursor + 1 lands exactly on the floor.
        assert_eq!(new_cursor, 150);

        let row: (i64, i64, String) = sqlx::query_as(
            "SELECT from_ledger, to_ledger, status FROM backfill_jobs WHERE network = $1",
        )
        .bind(&network)
        .fetch_one(&pool)
        .await
        .expect("the skipped range must be queryable as a backfill_jobs row");

        // The skipped span is strictly between the old cursor and the new
        // floor: [101, 150] — ledger 151 itself was never lost, it's where
        // the next poll resumes.
        assert_eq!(row.0, 101, "from_ledger must be old_cursor + 1");
        assert_eq!(row.1, 150, "to_ledger must be floor - 1");
        assert_eq!(row.2, "pending");

        sqlx::query("DELETE FROM backfill_jobs WHERE network = $1")
            .bind(&network)
            .execute(&pool)
            .await
            .unwrap();
    }

    /// A floor that is not actually ahead of the cursor (or lands adjacent
    /// to it, i.e. nothing was skipped) must not enqueue an empty/inverted
    /// range.
    #[tokio::test]
    async fn recover_retained_floor_enqueues_nothing_when_no_ledgers_were_actually_skipped() {
        let db_url = match std::env::var("TEST_DATABASE_URL") {
            Ok(url) => url,
            Err(_) if std::env::var("REQUIRE_TEST_SERVICES").is_ok() => {
                panic!("TEST_DATABASE_URL must be set when REQUIRE_TEST_SERVICES is set");
            }
            Err(_) => {
                eprintln!("SKIP: TEST_DATABASE_URL not set");
                return;
            }
        };
        let pool = sqlx::PgPool::connect(&db_url).await.unwrap();
        let network = format!("retainedfloortest-{}", uuid::Uuid::new_v4());

        // cursor = 100, floor = 101: the next poll resumes at exactly 101,
        // nothing in between was skipped.
        let source_error = TridentError::rpc(anyhow::anyhow!(
            "startLedger must be within the ledger range: 101 - 999"
        ));
        let new_cursor =
            Streamer::recover_retained_floor(&pool, &network, 100, 101, &source_error).await;
        assert_eq!(new_cursor, 100);

        let count: (i64,) = sqlx::query_as("SELECT COUNT(*) FROM backfill_jobs WHERE network = $1")
            .bind(&network)
            .fetch_one(&pool)
            .await
            .unwrap();
        assert_eq!(
            count.0, 0,
            "no ledgers were skipped, so no job should be enqueued"
        );
    }

    // Pure unit tests for jitter (issue #197) — no services required.
    #[test]
    fn jitter_stays_within_full_jitter_bounds() {
        let base = Duration::from_millis(1000);
        for _ in 0..1000 {
            let jittered = jitter(base);
            assert!(
                jittered >= base.mul_f64(0.5) && jittered <= base,
                "jittered duration {jittered:?} must stay within [50%, 100%] of {base:?}"
            );
        }
    }

    #[test]
    fn jitter_of_zero_is_zero() {
        assert_eq!(jitter(Duration::ZERO), Duration::ZERO);
    }

    #[test]
    fn jitter_varies_across_calls() {
        // Not a statistical test — just confirms this isn't a no-op that
        // always returns the same fraction of the input.
        let base = Duration::from_millis(1000);
        let samples: std::collections::HashSet<Duration> = (0..50).map(|_| jitter(base)).collect();
        assert!(
            samples.len() > 1,
            "expected multiple distinct jittered values, got {samples:?}"
        );
    }

    fn sym_xdr(s: &str) -> String {
        let val = ScVal::Symbol(ScSymbol::try_from(s.to_string()).unwrap());
        let mut buf = vec![];
        val.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
            .unwrap();
        STANDARD.encode(buf)
    }

    fn void_xdr() -> String {
        let val = ScVal::Void;
        let mut buf = vec![];
        val.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
            .unwrap();
        STANDARD.encode(buf)
    }

    fn events_page(ledger: u64, count: usize) -> serde_json::Value {
        let events: Vec<serde_json::Value> = (0..count)
            .map(|i| {
                serde_json::json!({
                    "type": "contract",
                    "ledger": ledger.to_string(),
                    "ledgerClosedAt": "2024-01-01T00:00:00Z",
                    "contractId": "CTEST",
                    "id": format!("{:016}-{}", ledger, i),
                    "pagingToken": format!("{}-{}", ledger, i),
                    "txHash": format!("hash{}{}", ledger, i),
                    "topic": [sym_xdr("transfer")],
                    "value": void_xdr(),
                    "inSuccessfulContractCall": true
                })
            })
            .collect();

        serde_json::json!({
            "jsonrpc": "2.0",
            "id": 1,
            "result": {
                "events": events,
                "latestLedger": ledger
            }
        })
    }

    /// Like `events_page`, but scoped to a caller-chosen `contract_id` so a
    /// test can assert on rows for its own contract without colliding with
    /// `"CTEST"` fixtures elsewhere.
    fn events_page_for_contract(contract_id: &str, ledger: u64, count: usize) -> serde_json::Value {
        let events: Vec<serde_json::Value> = (0..count)
            .map(|i| {
                serde_json::json!({
                    "type": "contract",
                    "ledger": ledger.to_string(),
                    "ledgerClosedAt": "2024-01-01T00:00:00Z",
                    "contractId": contract_id,
                    "id": format!("{:016}-{}", ledger, i),
                    "pagingToken": format!("{}-{}", ledger, i),
                    "txHash": format!("hash{:x}{}", ledger, i),
                    "topic": [sym_xdr("transfer")],
                    "value": void_xdr(),
                    "inSuccessfulContractCall": true
                })
            })
            .collect();

        serde_json::json!({
            "jsonrpc": "2.0",
            "id": 1,
            "result": {
                "events": events,
                "latestLedger": ledger
            }
        })
    }

    fn error_500() -> ResponseTemplate {
        ResponseTemplate::new(500).set_body_string("Internal Server Error")
    }

    fn rpc_ok(body: serde_json::Value) -> ResponseTemplate {
        ResponseTemplate::new(200).set_body_json(body)
    }

    /// Standalone Redis connection for tests that assert on the stream. The
    /// streamer itself no longer holds one — the relay owns publishing.
    async fn redis_conn(redis_url: &str) -> redis::aio::MultiplexedConnection {
        redis::Client::open(redis_url)
            .unwrap()
            .get_multiplexed_async_connection()
            .await
            .unwrap()
    }

    async fn make_streamer(db_url: &str, redis_url: &str, rpc_url: String) -> Streamer {
        let db = sqlx::PgPool::connect(db_url).await.unwrap();
        make_streamer_with_pool(db, db_url, redis_url, rpc_url).await
    }

    /// Like `make_streamer`, but takes an already-built pool so a test can
    /// configure it first (e.g. a single connection with a fixed
    /// `statement_timeout`, for `transient_db_outage_retries_the_page_instead_of_dead_lettering_it`).
    async fn make_streamer_with_pool(
        db: sqlx::PgPool,
        db_url: &str,
        redis_url: &str,
        rpc_url: String,
    ) -> Streamer {
        let config = Config {
            stellar_rpc_url: rpc_url.clone(),
            database_url: db_url.to_string(),
            db_pool_size: 3,
            redis_url: redis_url.to_string(),
            network: "testnet".to_string(),
            max_reorg_depth: 128,
            poll_interval: Duration::from_millis(50),
            poll_interval_floor: Duration::from_millis(50),
            poll_interval_ceiling: Duration::from_millis(500),
            lag_high_watermark: 100,
            poll_hysteresis_ledgers: 10,
            max_reorg_rewind_depth: 50,
            gap_scan_max_per_run: 100,
            stellar_rpc_urls: vec![rpc_url],
            rpc_failover_threshold: 3,
            rpc_endpoint_cooldown: Duration::from_secs(30),
            rpc_breaker_failure_threshold: 5,
            rpc_breaker_cooldown: Duration::from_secs(30),
            rpc_connect_timeout: Duration::from_secs(5),
            rpc_request_timeout: Duration::from_secs(30),
            rpc_pool_idle_timeout: Duration::from_secs(90),
            rpc_pool_max_idle_per_host: 8,
            rpc_tcp_keepalive: Duration::from_secs(60),
            rpc_max_calls_per_sec: 50,
            index_diagnostic: false,
            topic_filters: Vec::new(),
            max_events_per_poll: 200,
            db_batch_size: 1_000,
            redis_stream_maxlen: 10_000,
            outbox_poll_interval: Duration::from_millis(10),
            outbox_batch_size: 500,
            outbox_backlog_alert_threshold: 10_000,
            reconcile_enabled: false,
            reconcile_interval: Duration::from_secs(600),
            reconcile_ledger_span: 400,
            reconcile_tip_margin: 100,
            metrics_port: 0,
            alert_webhook_url: None,
            alert_lag_threshold: 200,
            alert_cooldown_minutes: 30,
            health_port: 0,
            statement_timeout_ms: 30_000,
            idle_in_transaction_timeout_ms: 60_000,
            token_metadata_refresh_interval: Duration::from_secs(86_400),
            network_passphrase: "Test SDF Network ; September 2015".to_string(),
            tracked_sac_assets: Vec::new(),
        };

        Streamer::new(config, db).await.unwrap()
    }

    async fn reset_db(pool: &sqlx::PgPool) {
        sqlx::query("DELETE FROM soroban_events")
            .execute(pool)
            .await
            .unwrap();
        sqlx::query("DELETE FROM ledger_metadata")
            .execute(pool)
            .await
            .unwrap();
        sqlx::query(
            "INSERT INTO system_state (key, value) VALUES ('latest_ledger_cursor:testnet', '0')
             ON CONFLICT (key) DO UPDATE SET value = '0'",
        )
        .execute(pool)
        .await
        .unwrap();
    }

    #[tokio::test]
    async fn events_written_to_postgres_after_poll() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(100, 3)))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(100, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        let mut cursor = db::get_cursor(&s.db, &s.config.network).await.unwrap();
        s.poll_once(&mut cursor).await.unwrap();

        let count: (i64,) = sqlx::query_as("SELECT COUNT(*) FROM soroban_events")
            .fetch_one(&s.db)
            .await
            .unwrap();
        assert_eq!(count.0, 3, "expected 3 events in soroban_events");
    }

    /// A sustained RPC outage (repeated 5xx) must trip the circuit breaker
    /// after `rpc_breaker_failure_threshold` consecutive failures, and once
    /// Open, the run loop must stop calling the RPC at all rather than
    /// retrying every poll interval (issue #197). This drives the same
    /// should_allow/poll_once/record sequence `run()` uses, since `run()`
    /// itself loops forever and is not directly testable.
    #[tokio::test]
    async fn sustained_rpc_outage_trips_breaker_and_stops_polling() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(ResponseTemplate::new(500))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;
        // Deterministic threshold/cooldown for this test, independent of the
        // Config default baked into make_streamer.
        s.rpc_breaker = CircuitBreaker::new(CircuitBreakerConfig {
            failure_threshold: 3,
            cooldown: Duration::from_secs(3600), // long enough not to elapse mid-test
        });

        let mut cursor = db::get_cursor(&s.db, &s.config.network).await.unwrap();

        for _ in 0..3 {
            assert!(s.rpc_breaker.should_allow());
            let result = s.poll_once(&mut cursor).await;
            let err = result.expect_err("mocked 500 response must surface as an error");
            assert!(err.is_rpc(), "a getEvents 5xx must classify as RpcError");
            s.rpc_breaker.record(Outcome::RpcFailure);
        }

        assert_eq!(s.rpc_breaker.state(), BreakerState::Open);
        assert_eq!(s.rpc_breaker.consecutive_failures(), 3);

        let hits_at_open = server
            .received_requests()
            .await
            .expect("request recording must be enabled by default")
            .len();
        assert!(
            hits_at_open > 0,
            "sanity check: the mock must have actually been hit while closed"
        );
        assert!(
            !s.rpc_breaker.should_allow(),
            "must stay open before cooldown elapses"
        );

        // Simulate several more loop iterations the way run() would: since
        // should_allow() is false, poll_once must never be called, so the
        // mock's hit count must not grow.
        for _ in 0..3 {
            if s.rpc_breaker.should_allow() {
                let _ = s.poll_once(&mut cursor).await;
            }
        }
        let hits_after = server.received_requests().await.unwrap().len();
        assert_eq!(
            hits_after, hits_at_open,
            "an open breaker must not issue any further RPC calls"
        );
    }

    /// scan_and_enqueue_gaps must enqueue a backfill_jobs row for a hole in
    /// ledger_metadata, and a later run — once the hole is filled — must
    /// close that job rather than leaving it pending forever (issue #216).
    #[tokio::test]
    async fn scan_and_enqueue_gaps_enqueues_then_closes() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        let base = 7_600_000u64;
        // A single-ledger hole at base+1: base and base+2 are present.
        for &seq in &[base, base + 2] {
            sqlx::query(
                "INSERT INTO ledger_metadata (ledger_sequence, ledger_hash, ledger_timestamp) \
                 VALUES ($1, $2, NOW()) ON CONFLICT (ledger_sequence) DO NOTHING",
            )
            .bind(seq as i64)
            .bind(format!("hash-{seq}"))
            .execute(&s.db)
            .await
            .unwrap();
        }

        s.scan_and_enqueue_gaps().await;

        let job: (i64, i64, String) = sqlx::query_as(
            "SELECT from_ledger, to_ledger, status FROM backfill_jobs \
             WHERE network = $1 AND from_ledger = $2",
        )
        .bind(&s.config.network)
        .bind((base + 1) as i64)
        .fetch_one(&s.db)
        .await
        .expect("a job for the detected gap must have been enqueued");
        assert_eq!(
            job,
            ((base + 1) as i64, (base + 1) as i64, "pending".to_string())
        );

        // Fill the hole, as if a backfill worker or the poll loop had caught
        // it, and re-scan: the job must now be closed.
        sqlx::query(
            "INSERT INTO ledger_metadata (ledger_sequence, ledger_hash, ledger_timestamp) \
             VALUES ($1, $2, NOW()) ON CONFLICT (ledger_sequence) DO NOTHING",
        )
        .bind((base + 1) as i64)
        .bind("hash-filled")
        .execute(&s.db)
        .await
        .unwrap();

        s.scan_and_enqueue_gaps().await;

        let status: String = sqlx::query_scalar(
            "SELECT status FROM backfill_jobs WHERE network = $1 AND from_ledger = $2",
        )
        .bind(&s.config.network)
        .bind((base + 1) as i64)
        .fetch_one(&s.db)
        .await
        .unwrap();
        assert_eq!(
            status, "done",
            "the job must be closed once the gap is filled"
        );

        sqlx::query("DELETE FROM ledger_metadata WHERE ledger_sequence BETWEEN $1 AND $2")
            .bind(base as i64)
            .bind((base + 2) as i64)
            .execute(&s.db)
            .await
            .unwrap();
        sqlx::query("DELETE FROM backfill_jobs WHERE network = $1 AND from_ledger = $2")
            .bind(&s.config.network)
            .bind((base + 1) as i64)
            .execute(&s.db)
            .await
            .unwrap();
    }

    /// A single event with an unparseable `ledgerClosedAt` poisons the whole
    /// batched INSERT (EventColumns::build fails for the chunk containing
    /// it), which used to abort the entire page and leave every otherwise-
    /// valid event unindexed with the cursor stuck. `commit_page_with_fallback`
    /// must isolate the poison event into `failed_events`, persist the rest,
    /// and still advance the cursor (issue #208).
    #[tokio::test]
    async fn poison_event_is_dead_lettered_and_page_still_advances_cursor() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        let contract_id = format!("CPOISON_{}", uuid::Uuid::new_v4());
        // Must fall inside a named partition. Migration 0017 creates 0-6M and
        // 50M-60M with a gap between, and the #525 exhaustion guard rejects
        // any ledger that gap would send to soroban_events_default — so 9M
        // failed here for a reason unrelated to what this test asserts.
        let ledger = 5_000_000u64;
        let page = serde_json::json!({
            "jsonrpc": "2.0",
            "id": 1,
            "result": {
                "events": [
                    {
                        "type": "contract",
                        "ledger": ledger.to_string(),
                        "ledgerClosedAt": "2024-01-01T00:00:00Z",
                        "contractId": contract_id,
                        "id": format!("{:016}-0", ledger),
                        "pagingToken": format!("{}-0", ledger),
                        "txHash": "good1good1good1",
                        "topic": [sym_xdr("transfer")],
                        "value": void_xdr(),
                        "inSuccessfulContractCall": true
                    },
                    {
                        "type": "contract",
                        "ledger": ledger.to_string(),
                        // Not a valid RFC3339 timestamp: EventColumns::build's
                        // `.parse::<DateTime<Utc>>()` fails on this event and
                        // only this event, poisoning any batch it rides in.
                        "ledgerClosedAt": "not-a-real-timestamp",
                        "contractId": contract_id,
                        "id": format!("{:016}-1", ledger),
                        "pagingToken": format!("{}-1", ledger),
                        "txHash": "poisonpoisonpoi",
                        "topic": [sym_xdr("transfer")],
                        "value": void_xdr(),
                        "inSuccessfulContractCall": true
                    },
                    {
                        "type": "contract",
                        "ledger": ledger.to_string(),
                        // Last event's ledgerClosedAt also feeds
                        // ledger_metadata — must stay valid so only the
                        // per-event insert (not the final cursor/ledger
                        // commit) is exercised by the poison row above.
                        "ledgerClosedAt": "2024-01-01T00:00:00Z",
                        "contractId": contract_id,
                        "id": format!("{:016}-2", ledger),
                        "pagingToken": format!("{}-2", ledger),
                        "txHash": "good2good2good2",
                        "topic": [sym_xdr("transfer")],
                        "value": void_xdr(),
                        "inSuccessfulContractCall": true
                    }
                ],
                "latestLedger": ledger
            }
        });

        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(page))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(ledger, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        let mut cursor = 0u64;
        s.poll_once(&mut cursor)
            .await
            .expect("poll_once must not fail even though one event is unpersistable");

        assert_eq!(
            cursor, ledger,
            "cursor must advance past the page despite the poison event"
        );
        let stored_cursor = db::get_cursor(&s.db, &s.config.network).await.unwrap();
        assert_eq!(stored_cursor, ledger);

        let good_count: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM soroban_events WHERE contract_id = $1")
                .bind(&contract_id)
                .fetch_one(&s.db)
                .await
                .unwrap();
        assert_eq!(good_count.0, 2, "both non-poison events must be persisted");

        let failed: (i64, String) = sqlx::query_as(
            "SELECT COUNT(*), MAX(transaction_hash) FROM failed_events WHERE contract_id = $1",
        )
        .bind(&contract_id)
        .fetch_one(&s.db)
        .await
        .unwrap();
        assert_eq!(
            failed.0, 1,
            "exactly the poison event must be dead-lettered"
        );
        assert_eq!(failed.1, "poisonpoisonpoi");

        sqlx::query("DELETE FROM soroban_events WHERE contract_id = $1")
            .bind(&contract_id)
            .execute(&s.db)
            .await
            .unwrap();
        sqlx::query("DELETE FROM failed_events WHERE contract_id = $1")
            .bind(&contract_id)
            .execute(&s.db)
            .await
            .unwrap();
    }

    /// Issue #573's DB-down case: every event in an otherwise-healthy page
    /// fails identically, in isolation, not because any of them is bad but
    /// because the database itself is unreachable — here simulated with an
    /// EXCLUSIVE table lock held by another session plus a short
    /// `statement_timeout`, so every INSERT attempt fails with a real
    /// Postgres `57014 query_canceled` (transient) rather than a mocked
    /// error. Before `classify_storage_failure`, `commit_page_with_fallback`
    /// could not tell this apart from a poison event and would dead-letter
    /// every event in the page wholesale. It must instead propagate the
    /// error so the page is retried, leaving the cursor exactly where it
    /// was and neither table touched.
    #[tokio::test]
    async fn transient_db_outage_retries_the_page_instead_of_dead_lettering_it() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        let contract_id = format!("CDBDOWN_{}", uuid::Uuid::new_v4());
        // Must fall inside a named partition, same constraint as the poison-
        // event test above: migration 0017 creates 0-6M and 50M-60M with a
        // gap between, and the #525 exhaustion guard rejects anything the
        // gap would send to soroban_events_default.
        let ledger = 5_100_000u64;
        let page = events_page_for_contract(&contract_id, ledger, 2);

        // Every getEvents call serves the same two-event page: the first
        // poll fails on the locked table, and the retry once the outage
        // clears must re-fetch those same events and persist them. A mock
        // that served an empty page on the retry would prove nothing — the
        // cursor would stay put and no rows would land, which is exactly
        // the data loss this test exists to rule out.
        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(page))
            .mount(&server)
            .await;
        // A single-connection pool with statement_timeout fixed at connect
        // time (mirroring main.rs's after_connect setup, issue #249): with
        // only one connection, every query the streamer issues — whole-page
        // and per-event alike — is guaranteed to run on the connection this
        // timeout was set on, rather than a `SET` on one pooled connection
        // that a later query might not reuse.
        let db = sqlx::postgres::PgPoolOptions::new()
            .max_connections(1)
            .after_connect(|conn, _| {
                Box::pin(async move {
                    sqlx::query("SET statement_timeout = '200ms'")
                        .execute(&mut *conn)
                        .await?;
                    Ok(())
                })
            })
            .connect(&db_url)
            .await
            .unwrap();
        let mut s = make_streamer_with_pool(db, &db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        // Hold an EXCLUSIVE lock on soroban_events from a separate session so
        // every INSERT the streamer attempts — whole-page and per-event alike
        // — blocks until it hits the statement_timeout above. The lock is
        // never committed, so it is released unconditionally when this
        // connection is dropped, even if an assertion below panics.
        let locker = sqlx::PgPool::connect(&db_url).await.unwrap();
        let mut lock_tx = locker.begin().await.unwrap();
        sqlx::query("LOCK TABLE soroban_events IN EXCLUSIVE MODE")
            .execute(&mut *lock_tx)
            .await
            .unwrap();

        let mut cursor = 0u64;
        let result = s.poll_once(&mut cursor).await;

        // Drop (implicit rollback) before asserting, so a failed assertion
        // can't leave the lock held for the rest of the test binary.
        drop(lock_tx);
        locker.close().await;

        assert!(
            result.is_err(),
            "a page that cannot reach the database at all must propagate an error, not report success"
        );
        assert_eq!(
            cursor, 0,
            "the cursor must not advance past events that were never actually persisted"
        );

        let persisted: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM soroban_events WHERE contract_id = $1")
                .bind(&contract_id)
                .fetch_one(&s.db)
                .await
                .unwrap();
        assert_eq!(
            persisted.0, 0,
            "nothing should have been persisted during the outage"
        );

        let dead_lettered: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM failed_events WHERE contract_id = $1")
                .bind(&contract_id)
                .fetch_one(&s.db)
                .await
                .unwrap();
        assert_eq!(
            dead_lettered.0, 0,
            "a transient outage must not dead-letter healthy events — issue #573"
        );

        // The outage has cleared (the lock was released above): a retry of
        // the same page must now succeed and persist both events, proving no
        // data was lost by propagating instead of dead-lettering.
        sqlx::query("SET statement_timeout = '30s'")
            .execute(&s.db)
            .await
            .unwrap();
        s.poll_once(&mut cursor)
            .await
            .expect("retry after the outage clears must succeed");
        assert_eq!(cursor, ledger);

        let persisted_after_retry: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM soroban_events WHERE contract_id = $1")
                .bind(&contract_id)
                .fetch_one(&s.db)
                .await
                .unwrap();
        assert_eq!(
            persisted_after_retry.0, 2,
            "both events must land once the database recovers"
        );

        sqlx::query("DELETE FROM soroban_events WHERE contract_id = $1")
            .bind(&contract_id)
            .execute(&s.db)
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn cursor_advances_in_system_state_after_poll() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(200, 2)))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(200, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();

        let stored = db::get_cursor(&s.db, &s.config.network).await.unwrap();
        assert_eq!(stored, 200, "cursor should advance to ledger 200");
        assert_eq!(cursor, 200);
    }

    #[tokio::test]
    async fn events_published_to_redis_stream_after_poll() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(300, 2)))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(300, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        let mut conn = redis_conn(&redis_url).await;
        reset_db(&s.db).await;
        sqlx::query("DELETE FROM event_outbox")
            .execute(&s.db)
            .await
            .unwrap();

        // Trim the stream so we start fresh.
        let _: () = redis::cmd("XTRIM")
            .arg("trident:events")
            .arg("MAXLEN")
            .arg(0)
            .query_async(&mut conn)
            .await
            .unwrap_or(());

        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();

        // The poll only commits events plus their outbox rows; the relay is
        // what puts them on the stream (issue #200).
        let mut relay = OutboxRelay::new(
            s.db.clone(),
            conn.clone(),
            RelayConfig {
                interval: Duration::from_millis(10),
                batch_size: 100,
                backlog_alert_threshold: 1_000,
                stream_maxlen: 10_000,
            },
        );
        let published = relay.publish_pending().await.unwrap();
        assert_eq!(published, 2, "relay should publish both committed events");

        let len: i64 = redis::cmd("XLEN")
            .arg("trident:events")
            .query_async(&mut conn)
            .await
            .unwrap();
        assert_eq!(len, 2, "expected 2 events in Redis stream");
    }

    /// A publish that never happens (relay not run) must leave the events
    /// recoverable: a later relay pass still delivers them, which is the
    /// crash-after-commit case from issue #200.
    #[tokio::test]
    async fn unpublished_events_are_delivered_on_a_later_relay_pass() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(400, 3)))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(400, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        let mut conn = redis_conn(&redis_url).await;
        reset_db(&s.db).await;
        sqlx::query("DELETE FROM event_outbox")
            .execute(&s.db)
            .await
            .unwrap();
        let _: () = redis::cmd("XTRIM")
            .arg("trident:events")
            .arg("MAXLEN")
            .arg(0)
            .query_async(&mut conn)
            .await
            .unwrap_or(());

        // Commit the batch, then simulate the process dying before any publish.
        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();

        let backlog = crate::db::outbox::backlog(&s.db).await.unwrap();
        assert_eq!(backlog, 3, "committed events must be queued for delivery");

        let len: i64 = redis::cmd("XLEN")
            .arg("trident:events")
            .query_async(&mut conn)
            .await
            .unwrap();
        assert_eq!(
            len, 0,
            "nothing should be on the stream before the relay runs"
        );

        // Restart equivalent: a fresh relay drains the backlog.
        let mut relay = OutboxRelay::new(
            s.db.clone(),
            conn.clone(),
            RelayConfig {
                interval: Duration::from_millis(10),
                batch_size: 100,
                backlog_alert_threshold: 1_000,
                stream_maxlen: 10_000,
            },
        );
        assert_eq!(relay.publish_pending().await.unwrap(), 3);

        let len: i64 = redis::cmd("XLEN")
            .arg("trident:events")
            .query_async(&mut conn)
            .await
            .unwrap();
        assert_eq!(len, 3, "relay must deliver every committed event");
        assert_eq!(crate::db::outbox::backlog(&s.db).await.unwrap(), 0);

        // A second pass is a no-op: published rows are not re-delivered.
        assert_eq!(relay.publish_pending().await.unwrap(), 0);
    }

    #[tokio::test]
    async fn poll_returns_error_when_rpc_consistently_fails() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        // Always return 500 so all retries exhaust.
        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(error_500())
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        let mut cursor = 0u64;
        // tokio-retry with max 5 retries and 200ms base — allow up to 10s
        let result = tokio::time::timeout(Duration::from_secs(10), s.poll_once(&mut cursor))
            .await
            .expect("poll_once timed out");
        assert!(
            result.is_err(),
            "poll_once should fail after retries exhausted"
        );
    }

    #[tokio::test]
    async fn poll_once_increments_metrics_counters() {
        use metrics_util::debugging::{DebugValue, DebuggingRecorder};
        use metrics_util::MetricKind;

        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(500, 3)))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .respond_with(rpc_ok(events_page(500, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        let recorder = DebuggingRecorder::new();
        let snapshotter = recorder.snapshotter();
        let guard = ::metrics::set_default_local_recorder(&recorder);

        let mut cursor = 0u64;
        let total = s.poll_once(&mut cursor).await.unwrap();
        drop(guard);

        assert_eq!(total, 3);

        let snapshot = snapshotter.snapshot().into_vec();
        let counter_value = |name: &str| {
            snapshot
                .iter()
                .find(|(key, _, _, _)| {
                    key.kind() == MetricKind::Counter && key.key().name() == name
                })
                .and_then(|(_, _, _, value)| match value {
                    DebugValue::Counter(n) => Some(*n),
                    _ => None,
                })
                .unwrap_or(0)
        };
        let gauge_value = |name: &str| {
            snapshot
                .iter()
                .find(|(key, _, _, _)| key.kind() == MetricKind::Gauge && key.key().name() == name)
                .and_then(|(_, _, _, value)| match value {
                    DebugValue::Gauge(n) => Some(n.into_inner()),
                    _ => None,
                })
        };

        assert_eq!(
            counter_value(metrics::EVENTS_TOTAL),
            3,
            "events_total should increment by the number of events processed"
        );
        assert_eq!(
            counter_value(metrics::POLL_ERRORS_TOTAL),
            0,
            "no poll errors occurred"
        );
        assert_eq!(
            gauge_value(metrics::LEDGER_LAG),
            Some(0.0),
            "lag should be zero once the cursor catches up to the chain tip"
        );
    }

    // -----------------------------------------------------------------------
    // Token projection (issue #211)
    // -----------------------------------------------------------------------

    /// A page of standard SEP-41 transfer events, wire-encoded the way the RPC
    /// returns them so the whole decode path runs.
    fn token_events_page(ledger: u64, count: usize) -> serde_json::Value {
        use stellar_xdr::curr::{AccountId, Int128Parts, PublicKey, ScAddress, Uint256};

        let addr = |seed: u8| {
            let val = ScVal::Address(ScAddress::Account(AccountId(
                PublicKey::PublicKeyTypeEd25519(Uint256([seed; 32])),
            )));
            let mut buf = vec![];
            val.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
                .unwrap();
            STANDARD.encode(buf)
        };
        let amount = |v: i64| {
            let val = ScVal::I128(Int128Parts {
                hi: 0,
                lo: v as u64,
            });
            let mut buf = vec![];
            val.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
                .unwrap();
            STANDARD.encode(buf)
        };

        let events: Vec<serde_json::Value> = (0..count)
            .map(|i| {
                serde_json::json!({
                    "type": "contract",
                    "ledger": ledger.to_string(),
                    "ledgerClosedAt": "2024-01-01T00:00:00Z",
                    "contractId": "CTOKEN_PROJECTION",
                    "id": format!("{:016}-{}", ledger, i),
                    "pagingToken": format!("{}-{}", ledger, i),
                    "txHash": format!("tokenhash{}{}", ledger, i),
                    "topic": [sym_xdr("transfer"), addr(1), addr(2)],
                    "value": amount(1_000 + i as i64),
                    "inSuccessfulContractCall": true
                })
            })
            .collect();

        serde_json::json!({
            "jsonrpc": "2.0",
            "id": 1,
            "result": { "events": events, "latestLedger": ledger }
        })
    }

    #[tokio::test]
    async fn token_events_are_projected_into_the_projection_table() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getLedgers" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "ledgers": [] }
            })))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(token_events_page(700, 3)))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(events_page(700, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();

        let rows: Vec<(String, Option<String>, Option<String>)> = sqlx::query_as(
            "SELECT event_type, amount, to_address FROM token_events
             WHERE contract_id = 'CTOKEN_PROJECTION' ORDER BY event_index",
        )
        .fetch_all(&s.db)
        .await
        .unwrap();

        assert_eq!(rows.len(), 3, "every transfer must be projected");
        assert!(rows.iter().all(|(kind, _, _)| kind == "transfer"));
        assert_eq!(rows[0].1.as_deref(), Some("1000"));
        assert!(rows[0].2.is_some(), "transfer must record a destination");

        // Replaying the same page must not duplicate the projection.
        let mut replay_cursor = 0u64;
        let _ = s.poll_once(&mut replay_cursor).await;
        let count: (i64,) = sqlx::query_as(
            "SELECT COUNT(*) FROM token_events WHERE contract_id = 'CTOKEN_PROJECTION'",
        )
        .fetch_one(&s.db)
        .await
        .unwrap();
        assert_eq!(count.0, 3, "replay must not duplicate projection rows");
    }

    // -----------------------------------------------------------------------
    // Invocation metrics (issue #266)
    // -----------------------------------------------------------------------

    /// Build the `envelopeXdr` + `resultXdr` base64 pair for a successful
    /// Soroban invocation declaring the given resource budget, exactly as
    /// `getTransaction` returns them.
    fn invocation_transaction_xdr(
        instructions: u32,
        disk_read_bytes: u32,
        write_bytes: u32,
        resource_fee: i64,
        fee_charged: i64,
    ) -> (String, String) {
        use stellar_xdr::curr::{
            LedgerFootprint, Memo, MuxedAccount, Operation, OperationBody, Preconditions,
            SequenceNumber, SorobanResources, SorobanTransactionData, SorobanTransactionDataExt,
            Transaction, TransactionEnvelope, TransactionExt, TransactionResult,
            TransactionResultExt, TransactionResultResult, TransactionV1Envelope, VecM,
        };

        let tx = Transaction {
            source_account: MuxedAccount::Ed25519(stellar_xdr::curr::Uint256([1u8; 32])),
            fee: 1_000_000,
            seq_num: SequenceNumber(1),
            cond: Preconditions::None,
            memo: Memo::None,
            operations: VecM::try_from(vec![Operation {
                source_account: None,
                body: OperationBody::Inflation,
            }])
            .unwrap(),
            ext: TransactionExt::V1(SorobanTransactionData {
                ext: SorobanTransactionDataExt::V0,
                resources: SorobanResources {
                    footprint: LedgerFootprint {
                        read_only: VecM::default(),
                        read_write: VecM::default(),
                    },
                    instructions,
                    disk_read_bytes,
                    write_bytes,
                },
                resource_fee,
            }),
        };
        let envelope = TransactionEnvelope::Tx(TransactionV1Envelope {
            tx,
            signatures: VecM::default(),
        });
        let result = TransactionResult {
            fee_charged,
            result: TransactionResultResult::TxSuccess(VecM::default()),
            ext: TransactionResultExt::V0,
        };

        let mut env_buf = vec![];
        envelope
            .write_xdr(&mut Limited::new(&mut env_buf, Limits::none()))
            .unwrap();
        let mut res_buf = vec![];
        result
            .write_xdr(&mut Limited::new(&mut res_buf, Limits::none()))
            .unwrap();

        (STANDARD.encode(env_buf), STANDARD.encode(res_buf))
    }

    #[tokio::test]
    async fn invocation_metrics_persisted_for_tracked_contract() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getLedgers" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "ledgers": [] }
            })))
            .mount(&server)
            .await;

        let event = serde_json::json!({
            "type": "contract",
            "ledger": "800",
            "ledgerClosedAt": "2024-01-01T00:00:00Z",
            "contractId": "CINVOKE_TRACKED",
            "id": "0000000000800000-0",
            "pagingToken": "800-0",
            "txHash": "txinvoke1",
            "topic": [sym_xdr("swap")],
            "value": void_xdr(),
            "inSuccessfulContractCall": true
        });
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "events": [event], "latestLedger": 800 }
            })))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "events": [], "latestLedger": 800 }
            })))
            .mount(&server)
            .await;

        let (envelope_xdr, result_xdr) =
            invocation_transaction_xdr(5_000_000, 2_048, 512, 12_345, 1_012_345);
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getTransaction", "params": { "hash": "txinvoke1" } }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 3,
                "result": {
                    "status": "SUCCESS",
                    "envelopeXdr": envelope_xdr,
                    "resultXdr": result_xdr,
                }
            })))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;
        sqlx::query(
            "DELETE FROM contract_invocation_metrics WHERE contract_id = 'CINVOKE_TRACKED'",
        )
        .execute(&s.db)
        .await
        .unwrap();
        set_allowlist(&s.db, &["CINVOKE_TRACKED"]).await;
        s.refresh_contract_filter().await.unwrap();

        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();

        let row: (i64, Option<i64>, Option<i64>, Option<i64>, Option<i64>, String) = sqlx::query_as(
            "SELECT fee_charged, resource_fee, cpu_instructions, read_bytes, write_bytes, provenance
             FROM contract_invocation_metrics WHERE contract_id = 'CINVOKE_TRACKED' AND transaction_hash = 'txinvoke1'",
        )
        .fetch_one(&s.db)
        .await
        .expect("invocation metrics row must be persisted");

        assert_eq!(row.0, 1_012_345, "fee_charged");
        assert_eq!(row.1, Some(12_345), "resource_fee");
        assert_eq!(row.2, Some(5_000_000), "cpu_instructions");
        assert_eq!(row.3, Some(2_048), "read_bytes");
        assert_eq!(row.4, Some(512), "write_bytes");
        assert_eq!(row.5, "declared_resources", "provenance");

        // Replaying the same page must not duplicate the row.
        let mut replay_cursor = 0u64;
        let _ = s.poll_once(&mut replay_cursor).await;
        let count: (i64,) = sqlx::query_as(
            "SELECT COUNT(*) FROM contract_invocation_metrics WHERE contract_id = 'CINVOKE_TRACKED'",
        )
        .fetch_one(&s.db)
        .await
        .unwrap();
        assert_eq!(count.0, 1, "replay must not duplicate the metrics row");

        set_allowlist(&s.db, &[]).await;
        sqlx::query(
            "DELETE FROM contract_invocation_metrics WHERE contract_id = 'CINVOKE_TRACKED'",
        )
        .execute(&s.db)
        .await
        .unwrap();
    }

    #[tokio::test]
    async fn invocation_metrics_are_not_fetched_in_index_all_mode() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getLedgers" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "ledgers": [] }
            })))
            .mount(&server)
            .await;

        let event = serde_json::json!({
            "type": "contract",
            "ledger": "810",
            "ledgerClosedAt": "2024-01-01T00:00:00Z",
            "contractId": "CINVOKE_UNTRACKED",
            "id": "0000000000810000-0",
            "pagingToken": "810-0",
            "txHash": "txinvoke2",
            "topic": [sym_xdr("swap")],
            "value": void_xdr(),
            "inSuccessfulContractCall": true
        });
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "events": [event], "latestLedger": 810 }
            })))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "events": [], "latestLedger": 810 }
            })))
            .mount(&server)
            .await;
        // Deliberately no getTransaction mock: in index-all mode (no
        // allowlist) the streamer must never call it. Wiremock returns a 404
        // for any unmatched request, which would fail the poll if this were
        // called.

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;
        set_allowlist(&s.db, &[]).await;
        s.refresh_contract_filter().await.unwrap();

        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();

        let count: (i64,) = sqlx::query_as(
            "SELECT COUNT(*) FROM contract_invocation_metrics WHERE contract_id = 'CINVOKE_UNTRACKED'",
        )
        .fetch_one(&s.db)
        .await
        .unwrap();
        assert_eq!(
            count.0, 0,
            "index-all mode must not fetch invocation metrics"
        );
    }

    // -----------------------------------------------------------------------
    // Server-side getEvents filtering (issue #203)
    // -----------------------------------------------------------------------

    /// Register the allowlist rows the streamer reads on refresh, scoped to the
    /// network the test config uses.
    async fn set_allowlist(pool: &sqlx::PgPool, contract_ids: &[&str]) {
        sqlx::query("DELETE FROM indexed_contracts WHERE network = 'testnet' OR network IS NULL")
            .execute(pool)
            .await
            .unwrap();
        for id in contract_ids {
            sqlx::query(
                "INSERT INTO indexed_contracts (contract_id, network) VALUES ($1, 'testnet')
                 ON CONFLICT (contract_id, network) DO NOTHING",
            )
            .bind(id)
            .execute(pool)
            .await
            .unwrap();
        }
    }

    #[tokio::test]
    async fn get_events_request_carries_allowlist_contract_filter() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        // The mock only matches when the outbound body contains the expected
        // filter block. If the filter were missing, nothing would match and the
        // poll would fail — this assertion cannot silently pass.
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(serde_json::json!({
                "method": "getEvents",
                "params": {
                    "filters": [{
                        "type": "contract",
                        "contractIds": ["CFILTER_A", "CFILTER_B"]
                    }]
                }
            })))
            .respond_with(rpc_ok(events_page(600, 0)))
            .expect(1..)
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;
        set_allowlist(&s.db, &["CFILTER_A", "CFILTER_B"]).await;
        s.refresh_contract_filter().await.unwrap();

        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();

        set_allowlist(&s.db, &[]).await;
    }

    #[tokio::test]
    async fn get_events_request_sends_empty_filters_in_index_all_mode() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(serde_json::json!({
                "method": "getEvents",
                "params": { "filters": [] }
            })))
            .respond_with(rpc_ok(events_page(601, 0)))
            .expect(1..)
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;
        set_allowlist(&s.db, &[]).await;
        s.refresh_contract_filter().await.unwrap();

        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();
    }

    #[tokio::test]
    async fn oversized_allowlist_degrades_to_unfiltered_request() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(serde_json::json!({
                "method": "getEvents",
                "params": { "filters": [] }
            })))
            .respond_with(rpc_ok(events_page(602, 0)))
            .expect(1..)
            .mount(&server)
            .await;

        let ids: Vec<String> = (0..crate::rpc::filters::MAX_FILTERABLE_CONTRACTS + 1)
            .map(|i| format!("CBIG_{i:03}"))
            .collect();
        let id_refs: Vec<&str> = ids.iter().map(String::as_str).collect();

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;
        set_allowlist(&s.db, &id_refs).await;
        s.refresh_contract_filter().await.unwrap();

        assert!(
            s.filter_plan.degraded,
            "an allowlist past the RPC caps must degrade to index-all"
        );

        let mut cursor = 0u64;
        s.poll_once(&mut cursor).await.unwrap();

        set_allowlist(&s.db, &[]).await;
    }

    #[tokio::test]
    async fn full_page_triggers_followup_poll_partial_page_stops() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        // getLedgers calls (made after each cursor advance) must not consume the
        // getEvents page mocks, so match them separately.
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getLedgers" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "ledgers": [] }
            })))
            .mount(&server)
            .await;
        // First getEvents call returns 200 events (full page) → triggers follow-up
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(events_page(400, 200)))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        // Second getEvents call returns 5 events (partial page) → stops pagination
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(events_page(401, 5)))
            .up_to_n_times(1)
            .mount(&server)
            .await;
        // Any further getEvents calls return empty
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(events_page(401, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        reset_db(&s.db).await;

        let mut cursor = 0u64;
        let total = s.poll_once(&mut cursor).await.unwrap();

        assert_eq!(
            total, 205,
            "should process 200 + 5 = 205 events across two pages"
        );
    }

    // -----------------------------------------------------------------------
    // Per-contract index_from gating (issue #202)
    // -----------------------------------------------------------------------

    fn events_page_multi(entries: Vec<(u64, &str, u32)>) -> serde_json::Value {
        // Computed before the into_iter() below consumes `entries`.
        let latest = entries.iter().map(|(l, _, _)| *l).max().unwrap_or(0);
        let events: Vec<serde_json::Value> = entries
            .into_iter()
            .map(|(ledger, contract_id, idx)| {
                serde_json::json!({
                    "type": "contract",
                    "ledger": ledger.to_string(),
                    "ledgerClosedAt": "2024-01-01T00:00:00Z",
                    "contractId": contract_id,
                    "id": format!("{:016}-{}", ledger, idx),
                    "pagingToken": format!("{}-{}", ledger, idx),
                    "txHash": format!("hash{}{}{}", ledger, contract_id, idx),
                    "topic": [sym_xdr("transfer")],
                    "value": void_xdr(),
                    "inSuccessfulContractCall": true,
                })
            })
            .collect();

        serde_json::json!({
            "jsonrpc": "2.0",
            "id": 1,
            "result": {
                "events": events,
                "latestLedger": latest,
            }
        })
    }

    /// Seed `indexed_contracts` with the given (contract_id, index_from, network) tuples.
    async fn seed_contracts(pool: &sqlx::PgPool, contracts: Vec<(&str, i64, &str)>) {
        for (id, index_from, network) in contracts {
            sqlx::query(
                r#"
                INSERT INTO indexed_contracts (contract_id, network, index_from)
                VALUES ($1, $2, $3)
                ON CONFLICT (contract_id, network) DO UPDATE SET index_from = EXCLUDED.index_from
                "#,
            )
            .bind(id)
            .bind(network)
            .bind(index_from)
            .execute(pool)
            .await
            .expect("seed_contracts failed");
        }
    }

    #[tokio::test]
    async fn index_from_gating_filters_out_below_threshold() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        // Register two contracts with different index_from values.
        let pool = sqlx::PgPool::connect(&db_url).await.unwrap();
        reset_db(&pool).await;
        seed_contracts(
            &pool,
            vec![("CTEST_A", 100, "testnet"), ("CTEST_B", 200, "testnet")],
        )
        .await;
        // Clean seed from streamer load_filter won't double-count.
        // Streamer is created after seeding, so its initial load picks up the contracts.

        // RPC returns events at ledgers 50, 150, and 250 for both contracts.
        // With index_from=100 for A and index_from=200 for B:
        //   A50  → skipped (< 100)
        //   A150 → indexed (≥ 100)
        //   A250 → indexed (≥ 100)
        //   B50  → skipped (< 200)
        //   B150 → skipped (< 200)
        //   B250 → indexed (≥ 200)

        // getLedgers mock (required for cursor advancement)
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getLedgers" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "ledgers": [] }
            })))
            .mount(&server)
            .await;

        // First getEvents page: events at ledgers 50, 150, 250
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(events_page_multi(vec![
                (50, "CTEST_A", 0),
                (50, "CTEST_B", 1),
                (150, "CTEST_A", 2),
                (150, "CTEST_B", 3),
                (250, "CTEST_A", 4),
                (250, "CTEST_B", 5),
            ])))
            .up_to_n_times(1)
            .mount(&server)
            .await;

        // Subsequent getEvents returns empty (stop pagination)
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(events_page_multi(vec![])))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        // Verify the filter was loaded with index_from values.
        let filter = s.contract_filter.as_ref().expect("filter should be Some");
        assert_eq!(filter.get("CTEST_A"), Some(&100));
        assert_eq!(filter.get("CTEST_B"), Some(&200));

        let mut cursor = 0u64;
        let total = s.poll_once(&mut cursor).await.unwrap();

        // Only 3 events should be indexed:
        //   CTEST_A at 150, CTEST_A at 250, CTEST_B at 250
        assert_eq!(total, 3, "only in-range events should be indexed");

        // Verify stored events in the database.
        let count_a: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM soroban_events WHERE contract_id = 'CTEST_A'")
                .fetch_one(&pool)
                .await
                .unwrap();
        assert_eq!(
            count_a.0, 2,
            "CTEST_A should have 2 events (ledgers 150, 250)"
        );

        let count_b: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM soroban_events WHERE contract_id = 'CTEST_B'")
                .fetch_one(&pool)
                .await
                .unwrap();
        assert_eq!(count_b.0, 1, "CTEST_B should have 1 event (ledger 250)");

        pool.close().await;
    }

    #[tokio::test]
    async fn reorg_detection_and_rollback_removes_stale_data() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        let pool = sqlx::PgPool::connect(&db_url).await.unwrap();
        reset_db(&pool).await;

        // Seed old events and ledger metadata at sequence 101 and 102 with cursor at 102
        sqlx::query(
            r#"
            INSERT INTO ledger_metadata (ledger_sequence, ledger_hash, ledger_timestamp, event_count)
            VALUES (100, 'hash100', '2024-01-01T00:00:00Z', 1),
                   (101, 'old_hash101', '2024-01-01T00:00:05Z', 1),
                   (102, 'old_hash102', '2024-01-01T00:00:10Z', 1)
            "#,
        )
        .execute(&pool)
        .await
        .unwrap();

        sqlx::query(
            r#"
            INSERT INTO soroban_events (id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash, event_index, event_type, topics, data)
            VALUES ('00000000-0000-0000-0000-000000000101', 'CDEMO', 101, '2024-01-01T00:00:05Z', 'tx101', 0, 'contract', '[]', '{}'),
                   ('00000000-0000-0000-0000-000000000102', 'CDEMO', 102, '2024-01-01T00:00:10Z', 'tx102', 0, 'contract', '[]', '{}')
            "#,
        )
        .execute(&pool)
        .await
        .unwrap();

        sqlx::query(
            "INSERT INTO system_state (key, value) VALUES ('latest_ledger_cursor:testnet', '102')
             ON CONFLICT (key) DO UPDATE SET value = '102'",
        )
        .execute(&pool)
        .await
        .unwrap();

        // Mock RPC latest ledger as 100 (indicating rollback of 101 & 102)
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getLatestLedger" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "sequence": 100 }
            })))
            .mount(&server)
            .await;

        // Mock getLedgers
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getLedgers" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "ledgers": [] }
            })))
            .mount(&server)
            .await;

        // Mock getEvents returning empty to complete poll
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getEvents" }),
            ))
            .respond_with(rpc_ok(events_page(100, 0)))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        let mut cursor = 102u64;

        s.poll_once(&mut cursor).await.unwrap();

        // Verify cursor was rewound to 100
        assert_eq!(cursor, 100, "cursor should be rewound to reorg tip 100");

        // Verify old stale events and metadata from 101 & 102 are deleted
        let count_events: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM soroban_events WHERE ledger_sequence > 100")
                .fetch_one(&pool)
                .await
                .unwrap();
        assert_eq!(
            count_events.0, 0,
            "stale soroban_events from reorged ledgers must be deleted"
        );

        let count_meta: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM ledger_metadata WHERE ledger_sequence > 100")
                .fetch_one(&pool)
                .await
                .unwrap();
        assert_eq!(
            count_meta.0, 0,
            "stale ledger_metadata from reorged ledgers must be deleted"
        );

        pool.close().await;
    }

    #[tokio::test]
    async fn deep_reorg_exceeding_max_depth_halts_with_error() {
        let (db_url, redis_url) = require_services!();
        let server = MockServer::start().await;

        let pool = sqlx::PgPool::connect(&db_url).await.unwrap();
        reset_db(&pool).await;

        // Mock RPC latest ledger as 100 when cursor is 500 (depth 400 > max_reorg_depth 128)
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(
                serde_json::json!({ "method": "getLatestLedger" }),
            ))
            .respond_with(rpc_ok(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "sequence": 100 }
            })))
            .mount(&server)
            .await;

        let mut s = make_streamer(&db_url, &redis_url, server.uri()).await;
        let mut cursor = 500u64;

        let result = s.poll_once(&mut cursor).await;
        assert!(
            result.is_err(),
            "deep reorg exceeding max_reorg_depth must return Err"
        );

        pool.close().await;
    }
}
