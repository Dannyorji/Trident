use clap::Parser;
use indicatif::{ProgressBar, ProgressStyle};
use sqlx::PgPool;
use std::sync::atomic::{AtomicU64, Ordering};
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

    #[arg(long, default_value_t = String::from("testnet"))]
    network: String,

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
                for raw in &page.events {
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
                                    Ok(_) => {
                                        events_indexed.fetch_add(1, Ordering::Relaxed);
                                    }
                                    Err(_) => {
                                        duplicates_skipped.fetch_add(1, Ordering::Relaxed);
                                    }
                                }
                            }
                        }
                        Ok(None) => {}
                        Err(e) => tracing::warn!(error = %e, "parse error"),
                    }
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
    let rpc = rpc_client::RpcClient::new(std::env::var("STELLAR_RPC_URL")?);
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

    let rpc = Arc::new(rpc_client::RpcClient::new(std::env::var(
        "STELLAR_RPC_URL",
    )?));

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
        let pb = pb.clone();
        let rpc_delay = args.rpc_delay_ms;
        let network = args.network.clone();

        let handle = tokio::spawn(async move {
            while let Some((s, e)) = rx.lock().await.recv().await {
                tracing::info!(start = s, end = e, "Worker got range");
                let pb = pb.clone();
                let _ = backfill_range(
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

    Ok(())
}
