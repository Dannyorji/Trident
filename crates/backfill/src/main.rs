use clap::Parser;
use indicatif::{ProgressBar, ProgressStyle};
use sqlx::PgPool;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use tokio::sync::{mpsc, Mutex};
use tokio::time::{sleep, Duration, Instant};
use tracing_subscriber::EnvFilter;

mod db;
mod parser;
mod rpc_client;

#[derive(Parser, Debug)]
#[command(author, version, about, long_about = None)]
struct Args {
    #[arg(long)]
    from_ledger: Option<u64>,

    #[arg(long)]
    to_ledger: Option<u64>,

    #[arg(long)]
    contract: Option<String>,

    #[arg(long, default_value_t = 4)]
    workers: usize,

    #[arg(long, default_value_t = 0)]
    rpc_delay_ms: u64,

    #[arg(long)]
    dry_run: bool,

    /// Validated against trident_common::Network's known set
    /// (mainnet/testnet/futurenet/sandbox), the same set
    /// chk_soroban_events_network enforces at the database layer (migration
    /// 0031). Previously a bare String accepted anything and the typo only
    /// surfaced as a check-constraint violation on the first insert, deep
    /// into a potentially long-running backfill (#773).
    #[arg(long, default_value_t = trident_common::types::Network::Testnet)]
    network: trident_common::types::Network,

    /// Run as a long-lived worker that claims ranges from `backfill_jobs`
    /// (issue #216) instead of backfilling a single --from-ledger/--to-ledger
    /// range. The indexer's gap scan enqueues rows there; this is the
    /// consumer side of that queue.
    #[arg(long)]
    from_queue: bool,

    /// How long to sleep between queue polls when no job is pending
    /// (--from-queue mode only).
    #[arg(long, default_value_t = 5_000)]
    queue_poll_interval_ms: u64,
}

/// Backfill one ledger range against the RPC, inserting events (or just
/// counting them, in dry-run mode). Shared by both the single-range CLI path
/// and the --from-queue worker (issue #216) so the two stay behaviourally
/// identical.
///
/// `max_consecutive_rpc_failures`: `None` retries an RPC failure forever
/// (the original CLI behaviour — a human operator is watching and can
/// Ctrl-C). `Some(n)` gives up and returns `Err` after `n` consecutive
/// failures, which `--from-queue` mode uses so one bad range can't wedge an
/// unattended worker forever; the job is then marked `failed` for an
/// operator to look at rather than silently retried indefinitely.
#[allow(clippy::too_many_arguments)]
async fn backfill_range(
    db: &PgPool,
    rpc: &rpc_client::RpcClient,
    parser: &parser::Parser,
    contract: Option<&str>,
    dry_run: bool,
    rpc_delay_ms: u64,
    network: &str,
    from_ledger: u64,
    to_ledger: u64,
    events_indexed: &AtomicU64,
    duplicates_skipped: &AtomicU64,
    max_consecutive_rpc_failures: Option<u32>,
    on_progress: impl Fn(u64),
) -> Result<(), trident_common::TridentError> {
    let mut page_cursor: Option<String> = None;
    let mut seq = from_ledger;
    let mut consecutive_rpc_failures: u32 = 0;
    while seq <= to_ledger {
        match rpc.get_events(Some(seq), page_cursor.clone()).await {
            Ok(page) => {
                consecutive_rpc_failures = 0;
                if page.events.is_empty() {
                    break;
                }

                // getEvents has no endLedger parameter (GetEventsParams has
                // no such field; this is an RPC limitation, not an
                // oversight) and keeps paging past to_ledger while pages
                // are full. Filter client-side and stop as soon as an event
                // crosses the requested upper bound, so a range never
                // writes rows above to_ledger regardless of how far the RPC
                // itself would otherwise page (#772).
                let mut crossed_upper_bound = false;
                for raw in &page.events {
                    if let Ok(event_ledger) = raw.ledger.parse::<u64>() {
                        if event_ledger > to_ledger {
                            crossed_upper_bound = true;
                            break;
                        }
                    }
                    match parser.parse_event(raw) {
                        Ok(Some(ev)) => {
                            if let Some(c) = contract {
                                if ev.contract_id != c {
                                    continue;
                                }
                            }
                            if dry_run {
                                events_indexed.fetch_add(1, Ordering::Relaxed);
                                if events_indexed.load(Ordering::Relaxed) <= 10 {
                                    println!("DRY: event {:?}", ev);
                                }
                            } else {
                                match db::insert_event(db, &ev, network).await {
                                    Ok(rows_affected) if rows_affected > 0 => {
                                        events_indexed.fetch_add(1, Ordering::Relaxed);
                                    }
                                    // rows_affected == 0: ON CONFLICT DO
                                    // NOTHING, a genuine replay of an
                                    // already-indexed event. Not an error.
                                    Ok(_) => {
                                        duplicates_skipped.fetch_add(1, Ordering::Relaxed);
                                    }
                                    // A real DB error (dropped connection,
                                    // constraint violation, timeout) is not
                                    // a duplicate and must not be silently
                                    // absorbed into duplicates_skipped
                                    // (#772) — fail the range so the caller
                                    // (CLI exit code or queue job status)
                                    // reflects the real outcome.
                                    Err(err) => return Err(err),
                                }
                            }
                        }
                        Ok(None) => {}
                        Err(e) => tracing::warn!(error = %e, "parse error"),
                    }
                }

                if crossed_upper_bound {
                    break;
                }

                // advance to last event ledger
                if let Some(last) = page.events.last() {
                    if let Ok(last_seq) = last.ledger.parse::<u64>() {
                        let advanced = last_seq.saturating_sub(seq) + 1;
                        seq = last_seq + 1;
                        on_progress(advanced);
                    } else {
                        seq += 1;
                        on_progress(1);
                    }
                } else {
                    break;
                }

                if page.events.len() < 200 {
                    break;
                }

                page_cursor = page.events.last().map(|e| e.page_cursor());

                if rpc_delay_ms > 0 {
                    sleep(Duration::from_millis(rpc_delay_ms)).await;
                }
            }
            Err(err) => {
                if let trident_common::TridentError::RpcError { source, .. } = &err {
                    tracing::warn!(error = %source, "RPC error");
                } else {
                    tracing::warn!(error = %err, "RPC error");
                }
                consecutive_rpc_failures += 1;
                if let Some(max) = max_consecutive_rpc_failures {
                    if consecutive_rpc_failures >= max {
                        return Err(err);
                    }
                }
                sleep(Duration::from_millis(500)).await;
            }
        }
    }
    Ok(())
}

/// --from-queue mode (issue #216): poll `backfill_jobs` for pending rows,
/// claim one with `FOR UPDATE SKIP LOCKED` (safe for multiple workers
/// running this mode concurrently), run it through the same
/// `backfill_range` path the single-range CLI uses, and mark it done/failed.
/// Runs until Ctrl-C.
async fn run_from_queue(args: Args, db: PgPool) -> Result<(), Box<dyn std::error::Error>> {
    let rpc = rpc_client::RpcClient::new(std::env::var("STELLAR_RPC_URL")?)?;
    let parser = parser::Parser::new(false);
    let events_indexed = AtomicU64::new(0);
    let duplicates_skipped = AtomicU64::new(0);

    tracing::info!(
        poll_interval_ms = args.queue_poll_interval_ms,
        "Starting backfill queue worker"
    );

    loop {
        match db::claim_next_job(&db).await {
            Ok(Some(job)) => {
                tracing::info!(
                    id = %job.id,
                    from = job.from_ledger,
                    to = job.to_ledger,
                    network = %job.network,
                    "Claimed backfill job"
                );

                // Bounded, unlike CLI mode: an unattended queue worker must
                // not spin forever on one bad range.
                const MAX_CONSECUTIVE_RPC_FAILURES: u32 = 10;
                let result = backfill_range(
                    &db,
                    &rpc,
                    &parser,
                    args.contract.as_deref(),
                    args.dry_run,
                    args.rpc_delay_ms,
                    &job.network,
                    job.from_ledger,
                    job.to_ledger,
                    &events_indexed,
                    &duplicates_skipped,
                    Some(MAX_CONSECUTIVE_RPC_FAILURES),
                    |_| {},
                )
                .await;

                match result {
                    Ok(()) => {
                        if let Err(e) = db::complete_job(&db, job.id).await {
                            tracing::error!(id = %job.id, error = %e, "Failed to mark job complete");
                        } else {
                            tracing::info!(id = %job.id, "Backfill job complete");
                        }
                    }
                    Err(e) => {
                        tracing::error!(id = %job.id, error = %e, "Backfill job failed");
                        if let Err(fail_err) = db::fail_job(&db, job.id, &e.to_string()).await {
                            tracing::error!(id = %job.id, error = %fail_err, "Failed to mark job failed");
                        }
                    }
                }
            }
            Ok(None) => {
                sleep(Duration::from_millis(args.queue_poll_interval_ms)).await;
            }
            Err(e) => {
                tracing::warn!(error = %e, "Failed to claim next backfill job; retrying");
                sleep(Duration::from_millis(args.queue_poll_interval_ms)).await;
            }
        }
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::from_default_env())
        .init();

    let args = Args::parse();

    let db = PgPool::connect(&std::env::var("DATABASE_URL")?).await?;

    if args.from_queue {
        return run_from_queue(args, db).await;
    }

    let (Some(from_ledger), Some(to_ledger)) = (args.from_ledger, args.to_ledger) else {
        return Err("--from-ledger and --to-ledger are required unless --from-queue is set".into());
    };
    if to_ledger < from_ledger {
        return Err("--to-ledger must be >= --from-ledger".into());
    }

    tracing::info!(
        from = from_ledger,
        to = to_ledger,
        workers = args.workers,
        "Starting backfill"
    );

    let total_ledgers = to_ledger - from_ledger + 1;
    let pb = ProgressBar::new(total_ledgers);
    pb.set_style(
        ProgressStyle::with_template("{msg} {bar:40.cyan/blue} {pos}/{len} ({percent}%) {eta}")?
            .progress_chars("=> "),
    );
    pb.set_message(format!(
        "Backfilling ledgers {}–{}:",
        from_ledger, to_ledger
    ));

    let (tx, rx) = mpsc::channel::<(u64, u64)>(args.workers * 2);

    // Split range into chunks for workers
    let chunk_size = (total_ledgers as usize).div_ceil(args.workers);
    let mut start = from_ledger;
    while start <= to_ledger {
        let end = std::cmp::min(start + chunk_size as u64 - 1, to_ledger);
        tx.send((start, end)).await?;
        start = end + 1;
    }
    drop(tx);

    let events_indexed = Arc::new(AtomicU64::new(0));
    let duplicates_skipped = Arc::new(AtomicU64::new(0));
    // A real error from backfill_range (a genuine DB failure, not a
    // duplicate) was previously discarded here (`let _ = ...`), so an
    // injected DB error completed with exit code 0 same as success (#772).
    let any_range_failed = Arc::new(AtomicBool::new(false));

    let rpc = Arc::new(rpc_client::RpcClient::new(std::env::var(
        "STELLAR_RPC_URL",
    )?)?);

    let rx = Arc::new(Mutex::new(rx));
    let mut handles = vec![];

    // Spawn worker tasks
    for _ in 0..args.workers {
        let rx = Arc::clone(&rx);
        let rpc = rpc.clone();
        let db = db.clone();
        let parser = parser::Parser::new(false);
        let contract = args.contract.clone();
        let dry_run = args.dry_run;
        let events_indexed = events_indexed.clone();
        let duplicates_skipped = duplicates_skipped.clone();
        let any_range_failed = any_range_failed.clone();
        let pb = pb.clone();
        let rpc_delay = args.rpc_delay_ms;
        let network = args.network.as_str().to_string();

        let handle = tokio::spawn(async move {
            while let Some((s, e)) = rx.lock().await.recv().await {
                tracing::info!(start = s, end = e, "Worker got range");
                let pb = pb.clone();
                let result = backfill_range(
                    &db,
                    &rpc,
                    &parser,
                    contract.as_deref(),
                    dry_run,
                    rpc_delay,
                    &network,
                    s,
                    e,
                    &events_indexed,
                    &duplicates_skipped,
                    None, // unbounded retries: matches the pre-#216 CLI behaviour
                    move |advanced| pb.inc(advanced),
                )
                .await;
                if let Err(err) = result {
                    tracing::error!(start = s, end = e, error = %err, "Backfill range failed");
                    any_range_failed.store(true, Ordering::Relaxed);
                }
            }
        });

        handles.push(handle);
    }

    let start_time = Instant::now();
    for h in handles {
        let _ = h.await;
    }

    pb.finish_and_clear();

    let duration = start_time.elapsed();
    let events = events_indexed.load(Ordering::Relaxed);
    let dups = duplicates_skipped.load(Ordering::Relaxed);

    let summary = serde_json::json!({
        "ledgers_processed": total_ledgers,
        "events_indexed": events,
        "duplicates_skipped": dups,
        "duration_seconds": duration.as_secs()
    });

    println!("{}", serde_json::to_string_pretty(&summary)?);

    if any_range_failed.load(Ordering::Relaxed) {
        return Err("one or more backfill ranges failed; see logged errors above".into());
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicU64;
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};

    /// A minimal event that parses successfully without real XDR: `topic:
    /// []` and `value: ""` both take the empty-data fast path in
    /// `Parser::parse_event`, so only `ledger` and `event_type` need to be
    /// realistic for this test.
    fn raw_event_json(ledger: u64, id: &str) -> serde_json::Value {
        serde_json::json!({
            "type": "system",
            "ledger": ledger.to_string(),
            "ledgerClosedAt": "2024-01-01T00:00:00Z",
            "contractId": "CA00000000000000000000000000000000000000000000000000000000",
            "id": id,
            "txHash": format!("tx-{id}"),
            "topic": [],
            "value": "",
            "inSuccessfulContractCall": true,
        })
    }

    async fn mock_get_events_response(server: &MockServer, events: serde_json::Value) {
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": {
                    "events": events,
                    "latestLedger": 1_000_000,
                }
            })))
            .mount(server)
            .await;
    }

    /// getEvents has no endLedger parameter and keeps paging while pages are
    /// full. A single page can legitimately contain events past the
    /// requested to_ledger; backfill_range must stop processing at the
    /// first one that crosses the bound rather than indexing (or even
    /// counting, in dry-run mode) everything the RPC happened to return
    /// (#772).
    #[tokio::test]
    async fn backfill_range_stops_at_to_ledger_even_when_page_extends_past_it() {
        let server = MockServer::start().await;
        // Page contains ledgers 100, 101, 102 but to_ledger is 101: the
        // event at 102 must not be counted.
        mock_get_events_response(
            &server,
            serde_json::json!([
                raw_event_json(100, "evt-100"),
                raw_event_json(101, "evt-101"),
                raw_event_json(102, "evt-102"),
            ]),
        )
        .await;

        let rpc = rpc_client::RpcClient::new(server.uri()).unwrap();
        let parser = parser::Parser::new(false);
        let events_indexed = AtomicU64::new(0);
        let duplicates_skipped = AtomicU64::new(0);

        // dry_run skips db::insert_event entirely, so this needs no
        // database: the bound check happens before the dry-run/insert
        // branch either way.
        let pool_opts = sqlx::postgres::PgPoolOptions::new();
        let db = pool_opts.connect_lazy("postgres://unused/unused").unwrap();

        backfill_range(
            &db,
            &rpc,
            &parser,
            None,
            true, // dry_run
            0,
            "testnet",
            100,
            101, // to_ledger
            &events_indexed,
            &duplicates_skipped,
            Some(1),
            |_| {},
        )
        .await
        .unwrap();

        assert_eq!(
            events_indexed.load(Ordering::Relaxed),
            2,
            "only the two events at or below to_ledger (100, 101) should be counted, \
             not the one at 102 that the page happened to also include"
        );
    }

    /// --network previously accepted any String; a typo only surfaced as a
    /// check-constraint violation on the first insert. It's now validated at
    /// CLI-parse time against the same set the database enforces (#773).
    #[test]
    fn network_arg_rejects_unknown_value_and_accepts_known_ones() {
        let err = Args::try_parse_from([
            "trident-backfill",
            "--from-ledger",
            "1",
            "--to-ledger",
            "2",
            "--network",
            "bogus",
        ])
        .expect_err("an unrecognised network must be rejected at parse time");
        assert!(
            err.to_string().contains("mainnet")
                && err.to_string().contains("testnet")
                && err.to_string().contains("futurenet")
                && err.to_string().contains("sandbox"),
            "the error should list the known networks, got: {err}"
        );

        for valid in ["mainnet", "testnet", "futurenet", "sandbox"] {
            let args = Args::try_parse_from([
                "trident-backfill",
                "--from-ledger",
                "1",
                "--to-ledger",
                "2",
                "--network",
                valid,
            ])
            .unwrap_or_else(|e| panic!("{valid} must be accepted, got: {e}"));
            assert_eq!(args.network.as_str(), valid);
        }
    }

    /// Unset --network must keep defaulting to testnet, matching the
    /// pre-#773 behaviour, not silently change the default while adding
    /// validation.
    #[test]
    fn network_arg_defaults_to_testnet() {
        let args =
            Args::try_parse_from(["trident-backfill", "--from-ledger", "1", "--to-ledger", "2"])
                .unwrap();
        assert_eq!(args.network.as_str(), "testnet");
    }
}
