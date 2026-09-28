//! # Ledger-range reconciliation against the RPC source (issue #511)
//!
//! Nothing else proves that what Trident indexed matches what the chain
//! actually emitted: the streamer trusts its own poll loop, and without an
//! independent check, silent under-indexing is invisible — the API returns a
//! confident, incomplete answer.
//!
//! This job periodically re-fetches a settled ledger window from `getEvents`
//! and compares per-ledger event counts against the database. The RPC side
//! applies **exactly the ingest pipeline's own selection rules** — the same
//! server-side filter plan, the diagnostic gate, the failed-call skip, and
//! the allowlist/`index_from` boundaries — because indexed counts are not raw
//! RPC counts, and comparing unlike sets would make every report a false
//! positive. The database side counts `soroban_events` plus `parse_errors`
//! (an event we saw but could not decode is accounted for, not missing).
//!
//! Discrepancies are reported as **specific ledger ranges** (contiguous
//! discrepant ledgers coalesced), logged with both counts, and surfaced via
//! the `trident_indexer_reconcile_*` metrics that the
//! `TridentIndexerReconciliationMismatch` alert fires on.
//!
//! ## Continuous, not on-demand — and why
//!
//! The job runs as a slow in-process loop (default: every 10 minutes over
//! the most recent ~400 settled ledgers, a deliberate match for the nightly
//! testnet-correctness suite's window). Continuous operation is what makes
//! under-indexing visible in minutes rather than at the next incident, and
//! at this cadence the extra RPC load is a rounding error next to the poll
//! loop. For arbitrary historical ranges there is already an on-demand path:
//! `trident-backfill --dry-run` walks any window and reports counts without
//! writing. Both choices are documented in the alert runbook.

use std::collections::HashMap;

use tokio_util::sync::CancellationToken;

use trident_common::TridentError;

use crate::config::Config;
use crate::db;
use crate::metrics;
use crate::rpc::filters::build_event_filters;
use crate::rpc::{EventFilter, RawEvent, RpcClient};

/// Page size for the reconciliation walk — same as the RPC maximum the
/// streamer uses.
const PAGE_LIMIT: u32 = 200;

/// Hard cap on pages per pass, so a pathological RPC response can never spin
/// the walk forever. 400 pages × 200 events comfortably covers the default
/// window.
const MAX_PAGES: u32 = 400;

/// One contiguous run of ledgers whose indexed counts disagree with the RPC.
#[derive(Debug, PartialEq, Eq)]
pub struct DiscrepantRange {
    pub from_ledger: u64,
    pub to_ledger: u64,
    /// Events the RPC reports for this range (after ingest selection rules).
    pub rpc_events: u64,
    /// Events accounted for in the database (indexed + parse-error rows).
    pub db_events: u64,
}

/// The outcome of one reconciliation pass.
#[derive(Debug, Default)]
pub struct ReconcileReport {
    pub window_start: u64,
    pub window_end: u64,
    pub rpc_events: u64,
    pub db_events: u64,
    pub discrepant_ranges: Vec<DiscrepantRange>,
    /// True when the walk hit MAX_PAGES before covering the requested
    /// window. The compare window is then CLAMPED to the fully-walked
    /// prefix (`window_end` reflects the clamp), so the ranges in this
    /// report are still real — but ledgers past `window_end` were not
    /// verified this pass.
    pub truncated: bool,
}

impl ReconcileReport {
    pub fn missing_events(&self) -> u64 {
        self.discrepant_ranges
            .iter()
            .map(|r| r.rpc_events.saturating_sub(r.db_events))
            .sum()
    }

    pub fn extra_events(&self) -> u64 {
        self.discrepant_ranges
            .iter()
            .map(|r| r.db_events.saturating_sub(r.rpc_events))
            .sum()
    }
}

/// The reconciliation loop. Construct with [`Reconciler::new`], then `run`
/// alongside the streamer; it stops on the shared shutdown token.
pub struct Reconciler {
    db: sqlx::PgPool,
    rpc: RpcClient,
    network: String,
    index_diagnostic: bool,
    topic_filters: Vec<Vec<String>>,
    interval: std::time::Duration,
    ledger_span: u64,
    tip_margin: u64,
}

impl Reconciler {
    pub fn new(cfg: &Config, db: sqlx::PgPool, rpc: RpcClient) -> Self {
        Self {
            db,
            rpc,
            network: cfg.network.clone(),
            index_diagnostic: cfg.index_diagnostic,
            topic_filters: cfg.topic_filters.clone(),
            interval: cfg.reconcile_interval,
            ledger_span: cfg.reconcile_ledger_span,
            tip_margin: cfg.reconcile_tip_margin,
        }
    }

    /// Loop until shutdown: one pass, log/meter the report, sleep. A pass
    /// failure is logged and retried next interval — the reconciler is a
    /// safety net and must never take the indexer down with it.
    pub async fn run(&self, shutdown: CancellationToken) {
        tracing::info!(
            interval_secs = self.interval.as_secs(),
            ledger_span = self.ledger_span,
            tip_margin = self.tip_margin,
            "Reconciliation loop starting"
        );
        loop {
            if shutdown.is_cancelled() {
                break;
            }
            match self.run_pass().await {
                Ok(report) => self.publish(&report),
                Err(e) => {
                    metrics::record_reconcile_pass_failed();
                    tracing::warn!(error = %e, "Reconciliation pass failed; will retry next interval");
                }
            }
            tokio::select! {
                _ = tokio::time::sleep(self.interval) => {}
                _ = shutdown.cancelled() => break,
            }
        }
        tracing::info!("Reconciliation loop stopping");
    }

    fn publish(&self, report: &ReconcileReport) {
        metrics::record_reconcile_pass_completed();
        metrics::set_reconcile_window_end(report.window_end);
        metrics::set_reconcile_discrepant_ledgers(
            report
                .discrepant_ranges
                .iter()
                .map(|r| r.to_ledger - r.from_ledger + 1)
                .sum::<u64>() as i64,
        );
        let missing = report.missing_events();
        let extra = report.extra_events();
        if missing > 0 {
            metrics::record_reconcile_missing_events(missing);
        }
        if extra > 0 {
            metrics::record_reconcile_extra_events(extra);
        }

        if report.truncated {
            tracing::warn!(
                window_start = report.window_start,
                window_end = report.window_end,
                "Reconciliation window was clamped at the page cap; ledgers past window_end were not verified this pass"
            );
        }
        if report.discrepant_ranges.is_empty() {
            tracing::info!(
                window_start = report.window_start,
                window_end = report.window_end,
                rpc_events = report.rpc_events,
                db_events = report.db_events,
                "Reconciliation clean: indexed counts match the RPC source"
            );
            return;
        }
        for range in &report.discrepant_ranges {
            tracing::warn!(
                from_ledger = range.from_ledger,
                to_ledger = range.to_ledger,
                rpc_events = range.rpc_events,
                db_events = range.db_events,
                "Reconciliation discrepancy: indexed counts disagree with the RPC source for this ledger range"
            );
        }
        tracing::warn!(
            window_start = report.window_start,
            window_end = report.window_end,
            ranges = report.discrepant_ranges.len(),
            missing_events = missing,
            extra_events = extra,
            "Reconciliation pass found discrepancies"
        );
    }

    /// One reconciliation pass over the most recent settled window.
    pub async fn run_pass(&self) -> Result<ReconcileReport, TridentError> {
        let tip = self.rpc.get_latest_ledger().await?;
        let window_end = tip.saturating_sub(self.tip_margin);
        let window_start = window_end
            .saturating_sub(self.ledger_span.saturating_sub(1))
            .max(1);
        if window_end == 0 || window_start > window_end {
            return Err(TridentError::rpc(anyhow::anyhow!(
                "chain tip {tip} leaves no settled window behind a margin of {}",
                self.tip_margin
            )));
        }

        // Only compare ledgers the indexer has actually passed: a window
        // ahead of the cursor is not yet indexed and would read as one giant
        // false "missing" range.
        let cursor = db::get_cursor(&self.db, &self.network).await?;
        let window_end = window_end.min(cursor);
        if window_end < window_start {
            return Err(TridentError::rpc(anyhow::anyhow!(
                "indexer cursor {cursor} has not reached the settled window starting at {window_start}; nothing to reconcile yet"
            )));
        }

        // Mirror the streamer's server-side filter plan exactly (issue #203):
        // same allowlist source, same topic patterns, same degraded-mode
        // fallback to client-side filtering.
        let allowlist = {
            let map = db::load_indexed_contracts(&self.db, &self.network).await?;
            if map.is_empty() {
                None
            } else {
                Some(map)
            }
        };
        let contract_ids = allowlist.as_ref().map(|map| {
            map.keys()
                .cloned()
                .collect::<std::collections::HashSet<_>>()
        });
        let plan = build_event_filters(contract_ids.as_ref(), &self.topic_filters);

        let rpc_counts = self
            .count_rpc_events(window_start, window_end, &plan.filters, allowlist.as_ref())
            .await?;

        // A walk that hit the page cap covered only part of the window. The
        // comparable range ends one ledger BEFORE the interruption point (the
        // interrupted ledger may be partially counted); comparing the full
        // window would turn every un-walked ledger into a fake "extra events"
        // discrepancy and page the on-call with garbage ranges.
        let mut window_end = window_end;
        if rpc_counts.truncated {
            let comparable_end = rpc_counts.last_seen_ledger.saturating_sub(1);
            if comparable_end < window_start {
                return Err(TridentError::rpc(anyhow::anyhow!(
                    "reconciliation walk hit the page cap before completing a single ledger;                      lower RECONCILE_LEDGER_SPAN (window [{window_start}, {window_end}])"
                )));
            }
            tracing::warn!(
                window_start,
                requested_end = window_end,
                comparable_end,
                "Reconciliation walk hit the page cap; comparing the covered prefix only —                  lower RECONCILE_LEDGER_SPAN if this persists"
            );
            window_end = comparable_end;
        }

        let db_counts = self.count_db_events(window_start, window_end).await?;

        Ok(build_report(
            window_start,
            window_end,
            &rpc_counts.per_ledger,
            &db_counts,
            rpc_counts.truncated,
        ))
    }

    /// Walk `getEvents` across the window and count events per ledger,
    /// applying the ingest pipeline's client-side selection rules.
    async fn count_rpc_events(
        &self,
        window_start: u64,
        window_end: u64,
        filters: &[EventFilter],
        allowlist: Option<&HashMap<String, i64>>,
    ) -> Result<RpcCounts, TridentError> {
        let mut per_ledger: HashMap<u64, u64> = HashMap::new();
        let mut cursor: Option<String> = None;
        let mut start: Option<u64> = Some(window_start);
        let mut truncated = true;
        let mut last_seen_ledger = 0u64;

        'pages: for _ in 0..MAX_PAGES {
            let page = self
                .rpc
                .get_events(start, cursor.clone(), PAGE_LIMIT, filters)
                .await?;
            // Only the first request anchors by ledger; later ones resume by
            // cursor (the RPC rejects requests carrying both).
            start = None;

            let count = page.events.len();
            if count == 0 {
                truncated = false;
                break;
            }

            for event in page.events {
                let ledger: u64 = event.ledger.parse().map_err(|_| {
                    TridentError::parse(anyhow::anyhow!(
                        "event {} reported an unparseable ledger {:?}",
                        event.id,
                        event.ledger
                    ))
                })?;
                if ledger > window_end {
                    truncated = false;
                    break 'pages;
                }
                cursor = Some(event.page_cursor());
                last_seen_ledger = last_seen_ledger.max(ledger);
                if self.counts_toward_index(&event, ledger, allowlist) {
                    *per_ledger.entry(ledger).or_insert(0) += 1;
                }
            }

            if count < PAGE_LIMIT as usize {
                truncated = false;
                break;
            }
        }

        Ok(RpcCounts {
            per_ledger,
            truncated,
            last_seen_ledger,
        })
    }

    /// The ingest pipeline's client-side selection rules, mirrored from
    /// `Parser::parse_event_with_projection` and the streamer's allowlist
    /// check. Any rule added there must be added here, or reconciliation
    /// reports false positives — the parity test below pins the behavior.
    fn counts_toward_index(
        &self,
        event: &RawEvent,
        ledger: u64,
        allowlist: Option<&HashMap<String, i64>>,
    ) -> bool {
        if event.event_type == "diagnostic" && !self.index_diagnostic {
            return false;
        }
        if !event.in_successful_contract_call {
            return false;
        }
        if let Some(filter) = allowlist {
            let contract_id = event.contract_id.as_deref().unwrap_or_default();
            match filter.get(contract_id) {
                None => return false,
                Some(&index_from) if (ledger as i64) < index_from => return false,
                _ => {}
            }
        }
        true
    }

    /// Per-ledger accounted-for counts from the database: indexed events plus
    /// parse-error rows (seen but undecodable — captured, not missing).
    async fn count_db_events(
        &self,
        window_start: u64,
        window_end: u64,
    ) -> Result<HashMap<u64, u64>, TridentError> {
        let mut per_ledger: HashMap<u64, u64> = HashMap::new();

        let indexed: Vec<(i64, i64)> = sqlx::query_as(
            "SELECT ledger_sequence, COUNT(*) FROM soroban_events
             WHERE ledger_sequence BETWEEN $1 AND $2
             GROUP BY ledger_sequence",
        )
        .bind(window_start as i64)
        .bind(window_end as i64)
        .fetch_all(&self.db)
        .await
        .map_err(|e| {
            TridentError::storage(anyhow::Error::new(e).context("reconcile count events"))
        })?;
        for (ledger, count) in indexed {
            *per_ledger.entry(ledger as u64).or_insert(0) += count as u64;
        }

        let parse_errors: Vec<(i64, i64)> = sqlx::query_as(
            "SELECT ledger_sequence, COUNT(*) FROM parse_errors
             WHERE ledger_sequence BETWEEN $1 AND $2
             GROUP BY ledger_sequence",
        )
        .bind(window_start as i64)
        .bind(window_end as i64)
        .fetch_all(&self.db)
        .await
        .map_err(|e| {
            TridentError::storage(anyhow::Error::new(e).context("reconcile count parse errors"))
        })?;
        for (ledger, count) in parse_errors {
            *per_ledger.entry(ledger as u64).or_insert(0) += count as u64;
        }

        Ok(per_ledger)
    }
}

struct RpcCounts {
    per_ledger: HashMap<u64, u64>,
    truncated: bool,
    /// Highest ledger whose events were fully consumed before the walk
    /// stopped. Meaningful only when `truncated`: the ledger the page cap
    /// interrupted may be partially counted, so the comparable window ends
    /// one ledger before it.
    last_seen_ledger: u64,
}

/// Compare per-ledger counts and coalesce contiguous discrepant ledgers into
/// ranges — the issue asks for *specific ledger ranges*, and one warn line
/// per affected range beats four hundred per-ledger lines.
fn build_report(
    window_start: u64,
    window_end: u64,
    rpc: &HashMap<u64, u64>,
    db: &HashMap<u64, u64>,
    truncated: bool,
) -> ReconcileReport {
    let mut report = ReconcileReport {
        window_start,
        window_end,
        truncated,
        ..Default::default()
    };

    // Counts outside the window (a partially walked ledger past a clamped
    // end) are deliberately ignored: the loop below only reads ledgers
    // inside [window_start, window_end].
    let mut open: Option<DiscrepantRange> = None;
    for ledger in window_start..=window_end {
        let rpc_count = rpc.get(&ledger).copied().unwrap_or(0);
        let db_count = db.get(&ledger).copied().unwrap_or(0);
        report.rpc_events += rpc_count;
        report.db_events += db_count;

        if rpc_count == db_count {
            if let Some(range) = open.take() {
                report.discrepant_ranges.push(range);
            }
            continue;
        }

        match open.as_mut() {
            Some(range) => {
                range.to_ledger = ledger;
                range.rpc_events += rpc_count;
                range.db_events += db_count;
            }
            None => {
                open = Some(DiscrepantRange {
                    from_ledger: ledger,
                    to_ledger: ledger,
                    rpc_events: rpc_count,
                    db_events: db_count,
                });
            }
        }
    }
    if let Some(range) = open.take() {
        report.discrepant_ranges.push(range);
    }

    report
}

#[cfg(test)]
mod tests {
    use super::*;

    fn counts(pairs: &[(u64, u64)]) -> HashMap<u64, u64> {
        pairs.iter().copied().collect()
    }

    #[test]
    fn matching_counts_produce_a_clean_report() {
        let rpc = counts(&[(10, 3), (11, 1)]);
        let db = counts(&[(10, 3), (11, 1)]);
        let report = build_report(10, 12, &rpc, &db, false);
        assert!(report.discrepant_ranges.is_empty());
        assert_eq!(report.rpc_events, 4);
        assert_eq!(report.db_events, 4);
    }

    #[test]
    fn contiguous_discrepant_ledgers_coalesce_into_one_range() {
        // Ledgers 11-13 all disagree; 10 and 14 agree.
        let rpc = counts(&[(10, 2), (11, 3), (12, 1), (13, 2)]);
        let db = counts(&[(10, 2), (11, 1), (13, 1), (14, 0)]);
        let report = build_report(10, 14, &rpc, &db, false);
        assert_eq!(
            report.discrepant_ranges,
            vec![DiscrepantRange {
                from_ledger: 11,
                to_ledger: 13,
                rpc_events: 6,
                db_events: 2,
            }]
        );
        assert_eq!(report.missing_events(), 4);
        assert_eq!(report.extra_events(), 0);
    }

    #[test]
    fn separate_discrepancies_report_separate_ranges() {
        // Discrepant ledgers 10, 12, 14 with clean ledgers 11 and 13 between
        // them: three distinct ranges, not one.
        let rpc = counts(&[(10, 1), (14, 1)]);
        let db = counts(&[(12, 5)]);
        let report = build_report(10, 14, &rpc, &db, false);
        assert_eq!(report.discrepant_ranges.len(), 3);
        // Extra events (indexed rows the chain never emitted) are reported
        // too — over-indexing is as wrong as under-indexing.
        assert_eq!(report.missing_events(), 2);
        assert_eq!(report.extra_events(), 5);
    }

    #[test]
    fn counts_outside_the_window_are_ignored() {
        // A clamped (truncated) walk can leave a partially counted ledger
        // past the compare window in the RPC map; it must not surface as a
        // discrepancy.
        let rpc = counts(&[(10, 1), (13, 7)]);
        let db = counts(&[(10, 1)]);
        let report = build_report(10, 12, &rpc, &db, true);
        assert!(report.discrepant_ranges.is_empty());
        assert!(report.truncated);
    }

    #[test]
    fn ledgers_with_zero_events_on_both_sides_are_clean() {
        let report = build_report(100, 500, &HashMap::new(), &HashMap::new(), false);
        assert!(report.discrepant_ranges.is_empty());
    }

    // -----------------------------------------------------------------------
    // Integration: a full pass against a mock RPC and a real database.
    // -----------------------------------------------------------------------

    use serde_json::json;
    use wiremock::matchers::{body_partial_json, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    /// Same env-gated skip the other DB-touching test modules use: silently
    /// skipped without TEST_DATABASE_URL, hard-failed when
    /// REQUIRE_TEST_SERVICES is set so CI cannot skip by accident.
    fn test_db_url() -> Option<String> {
        match std::env::var("TEST_DATABASE_URL") {
            Ok(url) if !url.is_empty() => Some(url),
            _ => {
                if std::env::var("REQUIRE_TEST_SERVICES").is_ok() {
                    panic!("REQUIRE_TEST_SERVICES is set but TEST_DATABASE_URL is missing");
                }
                eprintln!("SKIP: TEST_DATABASE_URL not set");
                None
            }
        }
    }

    fn raw_event(
        ledger: u64,
        idx: u32,
        contract: &str,
        event_type: &str,
        successful: bool,
    ) -> serde_json::Value {
        json!({
            "type": event_type,
            "ledger": ledger.to_string(),
            "ledgerClosedAt": "2024-01-01T00:00:00Z",
            "contractId": contract,
            "id": format!("{ledger:016}-{idx}"),
            "pagingToken": format!("{ledger}-{idx}"),
            "txHash": format!("rechash{ledger}{idx}"),
            "topic": ["AAAADwAAAAh0cmFuc2Zlcg=="],
            "value": "",
            "inSuccessfulContractCall": successful
        })
    }

    /// These two tests mutate genuinely global state — the `indexed_contracts`
    /// allowlist and the `latest_ledger_cursor` row — so they hold this for
    /// their whole run instead of racing each other (and use a ledger window,
    /// [30201, 30600], that no other suite's fixtures touch).
    static RECONCILE_DB_LOCK: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

    async fn seed_event(pool: &sqlx::PgPool, ledger: u64, idx: u32, contract: &str) {
        sqlx::query(
            "INSERT INTO soroban_events
               (id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash,
                event_index, event_type, topics, data)
             VALUES (gen_random_uuid(), $1, $2, NOW(), $3, $4, 'contract', '[]', '{}')",
        )
        .bind(contract)
        .bind(ledger as i64)
        .bind(format!("rechash{ledger}{idx}"))
        .bind(idx as i32)
        .execute(pool)
        .await
        .expect("seed event");
    }

    /// The acceptance scenario for issue #511: a deliberately incomplete and
    /// a deliberately over-indexed ledger are both reported with their exact
    /// ranges, the ingest pipeline's skip rules are mirrored (a failed-call
    /// event and a diagnostic event on the RPC side do NOT read as missing),
    /// and a parse-error row counts as accounted for.
    #[tokio::test]
    async fn pass_reports_missing_and_extra_ledger_ranges() {
        let _guard = RECONCILE_DB_LOCK.lock().await;
        let Some(db_url) = test_db_url() else { return };
        let pool = sqlx::PgPool::connect(&db_url).await.expect("db connect");

        sqlx::query("DELETE FROM soroban_events WHERE ledger_sequence BETWEEN 30201 AND 30600")
            .execute(&pool)
            .await
            .expect("clear events");
        sqlx::query("DELETE FROM parse_errors WHERE ledger_sequence BETWEEN 30201 AND 30600")
            .execute(&pool)
            .await
            .expect("clear parse errors");
        sqlx::query("DELETE FROM indexed_contracts")
            .execute(&pool)
            .await
            .expect("clear allowlist");
        sqlx::query(
            "INSERT INTO system_state (key, value) VALUES ('latest_ledger_cursor:testnet', '30600')
             ON CONFLICT (key) DO UPDATE SET value = '30600'",
        )
        .execute(&pool)
        .await
        .expect("set cursor");

        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(json!({"method": "getLatestLedger"})))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "jsonrpc": "2.0", "id": 1,
                "result": {"sequence": 30700}
            })))
            .mount(&server)
            .await;
        // Chain truth (window [30201, 30600] at span 400 / margin 100 / tip 1000):
        //   30550: two countable events           -> DB has 1 indexed + 1 parse
        //        error, so it is fully accounted for (clean).
        //   30551: one FAILED-call event          -> must not count (clean).
        //   30552: one diagnostic event           -> must not count (clean).
        //   30553: one countable event            -> DB has nothing (missing).
        //   30560: nothing                        -> DB has one row (extra).
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(json!({"method": "getEvents"})))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "jsonrpc": "2.0", "id": 1,
                "result": {
                    "events": [
                        raw_event(30550, 0, "CRECON", "contract", true),
                        raw_event(30550, 1, "CRECON", "contract", true),
                        raw_event(30551, 0, "CRECON", "contract", false),
                        raw_event(30552, 0, "CRECON", "diagnostic", true),
                        raw_event(30553, 0, "CRECON", "contract", true),
                    ],
                    "latestLedger": 30700
                }
            })))
            .mount(&server)
            .await;

        seed_event(&pool, 30550, 0, "CRECON").await;
        sqlx::query(
            "INSERT INTO parse_errors (ledger_sequence, event_index, raw_payload, error_message)
             VALUES (30550, 1, '{}', 'test decode failure')",
        )
        .execute(&pool)
        .await
        .expect("seed parse error");
        seed_event(&pool, 30560, 0, "CRECON").await;

        let rpc = RpcClient::with_endpoints(
            vec![server.uri()],
            &crate::rpc::RpcHttpSettings {
                connect_timeout: std::time::Duration::from_secs(5),
                request_timeout: std::time::Duration::from_secs(30),
                pool_idle_timeout: std::time::Duration::from_secs(90),
                pool_max_idle_per_host: 8,
                tcp_keepalive: std::time::Duration::from_secs(60),
                max_calls_per_sec: 50,
            },
        )
        .expect("rpc client");

        let reconciler = Reconciler {
            db: pool.clone(),
            rpc,
            network: "testnet".to_string(),
            index_diagnostic: false,
            topic_filters: Vec::new(),
            interval: std::time::Duration::from_secs(600),
            ledger_span: 400,
            tip_margin: 100,
        };

        let report = reconciler.run_pass().await.expect("pass");

        assert_eq!(report.window_start, 30201);
        assert_eq!(report.window_end, 30600);
        assert!(!report.truncated);
        assert_eq!(
            report.discrepant_ranges,
            vec![
                DiscrepantRange {
                    from_ledger: 30553,
                    to_ledger: 30553,
                    rpc_events: 1,
                    db_events: 0,
                },
                DiscrepantRange {
                    from_ledger: 30560,
                    to_ledger: 30560,
                    rpc_events: 0,
                    db_events: 1,
                },
            ],
            "exactly the corrupted ledgers must be reported, as specific ranges"
        );
        assert_eq!(report.missing_events(), 1);
        assert_eq!(report.extra_events(), 1);

        pool.close().await;
    }

    /// The allowlist and per-contract index_from boundaries are applied to
    /// the RPC side, mirroring the streamer — otherwise every skipped event
    /// would read as missing.
    #[tokio::test]
    async fn allowlist_rules_are_mirrored_on_the_rpc_side() {
        let _guard = RECONCILE_DB_LOCK.lock().await;
        let Some(db_url) = test_db_url() else { return };
        let pool = sqlx::PgPool::connect(&db_url).await.expect("db connect");

        sqlx::query("DELETE FROM soroban_events WHERE ledger_sequence BETWEEN 30201 AND 30600")
            .execute(&pool)
            .await
            .expect("clear events");
        sqlx::query("DELETE FROM parse_errors WHERE ledger_sequence BETWEEN 30201 AND 30600")
            .execute(&pool)
            .await
            .expect("clear parse errors");
        sqlx::query("DELETE FROM indexed_contracts")
            .execute(&pool)
            .await
            .expect("clear allowlist");
        sqlx::query(
            "INSERT INTO indexed_contracts (contract_id, network, index_from)
             VALUES ('CLISTED', 'testnet', 30555)",
        )
        .execute(&pool)
        .await
        .expect("seed allowlist");
        sqlx::query(
            "INSERT INTO system_state (key, value) VALUES ('latest_ledger_cursor:testnet', '30600')
             ON CONFLICT (key) DO UPDATE SET value = '30600'",
        )
        .execute(&pool)
        .await
        .expect("set cursor");

        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(json!({"method": "getLatestLedger"})))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "jsonrpc": "2.0", "id": 1,
                "result": {"sequence": 30700}
            })))
            .mount(&server)
            .await;
        // 30550: listed contract but BELOW its index_from -> not counted.
        // 30560: unlisted contract -> not counted.
        // 30570: listed, at/above index_from -> counted; DB has it (clean).
        Mock::given(method("POST"))
            .and(path("/"))
            .and(body_partial_json(json!({"method": "getEvents"})))
            .respond_with(ResponseTemplate::new(200).set_body_json(json!({
                "jsonrpc": "2.0", "id": 1,
                "result": {
                    "events": [
                        raw_event(30550, 0, "CLISTED", "contract", true),
                        raw_event(30560, 0, "CUNLISTED", "contract", true),
                        raw_event(30570, 0, "CLISTED", "contract", true),
                    ],
                    "latestLedger": 30700
                }
            })))
            .mount(&server)
            .await;

        seed_event(&pool, 30570, 0, "CLISTED").await;

        let rpc = RpcClient::with_endpoints(
            vec![server.uri()],
            &crate::rpc::RpcHttpSettings {
                connect_timeout: std::time::Duration::from_secs(5),
                request_timeout: std::time::Duration::from_secs(30),
                pool_idle_timeout: std::time::Duration::from_secs(90),
                pool_max_idle_per_host: 8,
                tcp_keepalive: std::time::Duration::from_secs(60),
                max_calls_per_sec: 50,
            },
        )
        .expect("rpc client");

        let reconciler = Reconciler {
            db: pool.clone(),
            rpc,
            network: "testnet".to_string(),
            index_diagnostic: false,
            topic_filters: Vec::new(),
            interval: std::time::Duration::from_secs(600),
            ledger_span: 400,
            tip_margin: 100,
        };

        let report = reconciler.run_pass().await.expect("pass");
        assert!(
            report.discrepant_ranges.is_empty(),
            "skip-rule parity must hold, got {:?}",
            report.discrepant_ranges
        );

        pool.close().await;
    }
}
