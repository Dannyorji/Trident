use std::net::SocketAddr;

use clap::{Parser, Subcommand};
use opentelemetry_otlp::WithExportConfig;
use sqlx::postgres::PgPoolOptions;
use std::time::Duration;
use tokio_util::sync::CancellationToken;
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::EnvFilter;

mod alerting;
mod config;
mod db;
mod health;
mod metrics;
mod parser;
mod poll;
mod reconcile;
mod redis_stream;
mod rpc;
mod spec;
mod storage;
mod streamer;
/// Scheduled testnet ingest-correctness suite (issue #419). Test-only; compiled
/// out of the binary entirely.
#[cfg(test)]
mod testnet_correctness;
mod token_metadata;

/// `trident-indexer` runs the poll-loop daemon when invoked with no
/// subcommand (every existing deployment — Docker, Helm, `cargo run`), so
/// this and `Command` are additive: nothing about the daemon path changes.
#[derive(Parser, Debug)]
#[command(author, version, about)]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
}

/// Operator tooling for the `failed_events` dead-letter queue (issue #574).
/// `DATABASE_URL` is read the same way the daemon reads it.
#[derive(Subcommand, Debug)]
enum Command {
    /// Replay dead-lettered events back into `soroban_events`.
    Replay {
        /// Replay one event by its `failed_events.id`.
        #[arg(long, conflicts_with = "all")]
        id: Option<uuid::Uuid>,

        /// Replay every row still pending (`replayed_at IS NULL`), oldest
        /// first.
        #[arg(long, conflicts_with = "id")]
        all: bool,

        /// List pending rows instead of replaying them. Combine with
        /// neither `--id` nor `--all`, or use on its own — the runbook
        /// query, run from the CLI instead of psql.
        #[arg(long)]
        list: bool,

        /// Cap on rows considered by `--all` or `--list`.
        #[arg(long, default_value_t = 1000)]
        limit: i64,
    },

    /// Compare the live chain tip against the highest named `soroban_events`
    /// partition and fail if headroom is below a safety margin (issue #643).
    ///
    /// The pre-built mainnet partitions in migration 0017 were sized against
    /// mainnet's ledger height at authoring time; by the time a mainnet
    /// indexer is actually pointed at the network, that height is stale and
    /// the ceiling may already be exhausted. This is a launch-day gap
    /// distinct from routine partition exhaustion (which
    /// TridentPartitionExhaustionWarning/Exhausted already alert on once the
    /// indexer is running) — this check exists to catch it *before* cutover.
    PartitionCheck {
        /// Minimum ledgers of headroom required between the live chain tip
        /// and the highest named partition's upper bound. Defaults to 5M
        /// ledgers (~289 days at mainnet's ~17,280 ledgers/day), matching the
        /// TridentPartitionExhaustionWarning threshold so pre-flight and
        /// runtime alerting agree on what "enough headroom" means.
        #[arg(long, default_value_t = 5_000_000)]
        min_headroom_ledgers: i64,
    },
}

/// Runs a `replay` subcommand to completion and exits — never starts the
/// poll-loop daemon, the metrics/health servers, or a signal handler, since
/// this is a one-shot operator command, not a long-running service.
async fn run_replay(
    id: Option<uuid::Uuid>,
    all: bool,
    list: bool,
    limit: i64,
) -> Result<(), Box<dyn std::error::Error>> {
    let database_url = std::env::var("DATABASE_URL")
        .map_err(|_| "DATABASE_URL must be set to use `trident-indexer replay`")?;
    let db = sqlx::PgPool::connect(&database_url).await?;

    // Replayed rows must be tagged with this deployment's actual network
    // (issue #595) — `failed_events` has no network column of its own, so
    // there is nothing to recover it from other than the same `NETWORK` env
    // var the daemon validates at startup.
    let network =
        config::normalize_network(&std::env::var("NETWORK").unwrap_or_else(|_| "testnet".into()))?;

    // Bare `replay` (no flags) behaves like `--list`: report what's
    // outstanding — the runbook's query, run from the CLI instead of psql —
    // without replaying anything, since a no-argument invocation is more
    // likely a mistake or a status check than an intent to replay
    // everything.
    let list = list || (!all && id.is_none());
    if list {
        print_pending(&db, limit).await?;
    }
    if !all && id.is_none() {
        return Ok(());
    }

    // `--list --all` previews before replaying; `--list --id` prints the
    // full pending table before replaying just that one row.
    let ids: Vec<uuid::Uuid> = match id {
        Some(id) => vec![id],
        None => db::list_pending_failed_events(&db, limit)
            .await?
            .into_iter()
            .map(|row| row.id)
            .collect(),
    };

    let mut replayed = 0u32;
    let mut skipped = 0u32;
    for id in ids {
        match db::replay_failed_event(&db, id, &network).await {
            Ok(db::ReplayOutcome::Replayed) => {
                println!("replayed {id}");
                replayed += 1;
            }
            Ok(db::ReplayOutcome::AlreadyReplayedOrMissing) => {
                println!("skipped {id} (already replayed or not found)");
                skipped += 1;
            }
            Err(e) => {
                eprintln!("failed to replay {id}: {e}");
                skipped += 1;
            }
        }
    }
    println!("{replayed} replayed, {skipped} skipped");

    Ok(())
}

/// Collapse a stored `error_message` to one line for the `--list` table.
/// Dead-letter messages are raw driver errors and are routinely multi-line;
/// printed verbatim they break the column layout. The full text stays in the
/// row for anyone querying `failed_events` directly.
fn truncate_error(msg: &str) -> String {
    const MAX: usize = 120;
    let one_line = msg.split_whitespace().collect::<Vec<_>>().join(" ");
    match one_line.char_indices().nth(MAX) {
        Some((byte_idx, _)) => format!("{}…", &one_line[..byte_idx]),
        None => one_line,
    }
}

/// Runs the `partition-check` subcommand to completion and exits (issue #643).
///
/// Compares the live Stellar RPC chain tip against the highest named
/// `soroban_events` partition upper bound and fails loudly (non-zero exit)
/// if the remaining headroom is below `min_headroom_ledgers`. Intended to run
/// as an explicit pre-cutover gate, not as an ongoing check — routine
/// exhaustion once the indexer is live is already covered by
/// TridentPartitionExhaustionWarning/Exhausted in monitoring/alerts.yml.
async fn run_partition_check(
    min_headroom_ledgers: i64,
) -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(EnvFilter::from_default_env())
        .init();

    let cfg = config::Config::from_env()?;
    let db = sqlx::PgPool::connect(&cfg.database_url).await?;

    let ranges = db::named_partition_ranges(&db).await?;
    let highest_upper = ranges.iter().map(|&(_, hi)| hi).max().ok_or_else(|| {
        "no named soroban_events partitions found — only the DEFAULT catch-all exists"
    })?;

    let rpc = rpc::RpcClient::with_endpoints(
        cfg.stellar_rpc_urls.clone(),
        &rpc::RpcHttpSettings {
            connect_timeout: cfg.rpc_connect_timeout,
            request_timeout: cfg.rpc_request_timeout,
            pool_idle_timeout: cfg.rpc_pool_idle_timeout,
            pool_max_idle_per_host: cfg.rpc_pool_max_idle_per_host,
            tcp_keepalive: cfg.rpc_tcp_keepalive,
        },
    )?;
    let tip = rpc.get_latest_ledger().await?;

    let headroom = highest_upper - tip as i64;

    println!("network:                  {}", cfg.network);
    println!("live chain tip:           {tip}");
    println!("highest named partition:  {highest_upper} (exclusive upper bound)");
    println!("headroom:                 {headroom} ledgers");
    println!("required headroom:        {min_headroom_ledgers} ledgers");

    if headroom < min_headroom_ledgers {
        eprintln!(
            "NO-GO: partition headroom ({headroom} ledgers) is below the required minimum \
             ({min_headroom_ledgers} ledgers). Run `SELECT create_soroban_partition({highest_upper}, {});` \
             (repeating as needed) to extend coverage before cutover (issue #643).",
            highest_upper + 2_000_000,
        );
        std::process::exit(1);
    }

    println!("GO: partition headroom is sufficient.");
    Ok(())
}

async fn print_pending(db: &sqlx::PgPool, limit: i64) -> Result<(), Box<dyn std::error::Error>> {
    let pending = db::list_pending_failed_events(db, limit).await?;
    if pending.is_empty() {
        println!("No pending failed_events rows.");
        return Ok(());
    }
    println!(
        "{:<36}  {:>12}  {:<56}  {:<26}  attempts  {:<24}  error",
        "id", "ledger", "contract_id", "tx_hash", "occurred_at"
    );
    for row in &pending {
        println!(
            "{:<36}  {:>12}  {:<56}  {:<26}  {:>8}  {:<24}  {}",
            row.id,
            row.ledger_sequence,
            row.contract_id,
            row.transaction_hash,
            row.attempts,
            row.occurred_at.to_rfc3339(),
            truncate_error(&row.error_message),
        );
    }
    println!("{} pending", pending.len());
    Ok(())
}

fn init_tracer() -> Option<opentelemetry_sdk::trace::Tracer> {
    let endpoint = std::env::var("OTEL_EXPORTER_OTLP_ENDPOINT").ok()?;
    let sampling_ratio = std::env::var("OTEL_SAMPLING_RATIO")
        .ok()
        .and_then(|s| s.parse::<f64>().ok())
        .unwrap_or(0.1);

    opentelemetry::global::set_text_map_propagator(
        opentelemetry_sdk::propagation::TraceContextPropagator::new(),
    );

    match opentelemetry_otlp::new_pipeline()
        .tracing()
        .with_exporter(
            opentelemetry_otlp::new_exporter()
                .tonic()
                .with_endpoint(endpoint),
        )
        .with_trace_config(
            opentelemetry_sdk::trace::Config::default()
                .with_sampler(opentelemetry_sdk::trace::Sampler::TraceIdRatioBased(
                    sampling_ratio,
                ))
                .with_resource(opentelemetry_sdk::Resource::new(vec![
                    opentelemetry::KeyValue::new("service.name", "trident-indexer"),
                ])),
        )
        .install_batch(opentelemetry_sdk::runtime::Tokio)
    {
        Ok(tracer) => Some(tracer),
        Err(e) => {
            eprintln!("Failed to initialise OpenTelemetry tracer: {e}");
            None
        }
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();
    if let Some(Command::Replay {
        id,
        all,
        list,
        limit,
    }) = cli.command
    {
        // A one-shot operator command: connect, do the work, exit. None of
        // the daemon's tracer/metrics/health/signal-handler setup below runs
        // for this path.
        tracing_subscriber::fmt()
            .with_env_filter(EnvFilter::from_default_env())
            .init();
        return run_replay(id, all, list, limit).await;
    }
    if let Some(Command::PartitionCheck {
        min_headroom_ledgers,
    }) = cli.command
    {
        return run_partition_check(min_headroom_ledgers).await;
    }

    init_tracing(init_tracer());

    tracing::info!("Trident indexer starting");

    let cfg = config::Config::from_env().unwrap_or_else(|e| {
        eprintln!("{e}");
        std::process::exit(1);
    });
    cfg.log_effective_config();

    metrics::install(cfg.metrics_port)?;

    // Set statement_timeout and idle_in_transaction_session_timeout on every
    // new connection so a pathological query or leaked transaction cannot hold
    // the pool indefinitely (#249).
    let stmt_timeout = cfg.statement_timeout_ms;
    let idle_timeout = cfg.idle_in_transaction_timeout_ms;
    let db_pool = PgPoolOptions::new()
        .max_connections(cfg.db_pool_size)
        .after_connect(move |conn, _| {
            Box::pin(async move {
                sqlx::query(&format!("SET statement_timeout = '{stmt_timeout}ms'"))
                    .execute(&mut *conn)
                    .await?;
                sqlx::query(&format!(
                    "SET idle_in_transaction_session_timeout = '{idle_timeout}ms'"
                ))
                .execute(&mut *conn)
                .await?;
                Ok(())
            })
        })
        .connect(&cfg.database_url)
        .await?;
    tracing::info!(
        pool_size = cfg.db_pool_size,
        statement_timeout_ms = stmt_timeout,
        idle_in_transaction_timeout_ms = idle_timeout,
        "Database connected with timeout defaults"
    );

    let redis_client = redis::Client::open(cfg.redis_url.as_str())?;
    let redis_conn = redis_client.get_multiplexed_async_connection().await?;
    tracing::info!("Redis connected");

    // Spawn health and readiness endpoints on HEALTH_PORT (default 8080, separate
    // from the Prometheus /metrics listener on METRICS_PORT) (#206).
    let health_addr: SocketAddr = ([0, 0, 0, 0], cfg.health_port).into();
    let health_db = db_pool.clone();
    let health_redis_url = cfg.redis_url.clone();
    tokio::spawn(async move {
        health::serve(health_addr, health_db, health_redis_url).await;
    });

    let shutdown = CancellationToken::new();

    // Spawn signal watcher — cancels the token on SIGTERM or SIGINT.
    let shutdown_trigger = shutdown.clone();
    tokio::spawn(async move {
        #[cfg(unix)]
        {
            use tokio::signal::unix::{signal, SignalKind};
            let mut sigterm =
                signal(SignalKind::terminate()).expect("failed to register SIGTERM handler");
            tokio::select! {
                _ = tokio::signal::ctrl_c() => {
                    tracing::info!("Received SIGINT, initiating graceful shutdown");
                }
                _ = sigterm.recv() => {
                    tracing::info!("Received SIGTERM, initiating graceful shutdown");
                }
            }
        }
        #[cfg(not(unix))]
        {
            let _ = tokio::signal::ctrl_c().await;
            tracing::info!("Received SIGINT, initiating graceful shutdown");
        }

        shutdown_trigger.cancel();
    });

    // Outbox relay (issue #200): the poll loop only commits events; this task
    // delivers them to Redis. It runs alongside the streamer and stops on the
    // same shutdown signal.
    let mut relay = redis_stream::relay::OutboxRelay::new(
        db_pool.clone(),
        redis_conn.clone(),
        redis_stream::relay::RelayConfig {
            interval: cfg.outbox_poll_interval,
            batch_size: cfg.outbox_batch_size,
            backlog_alert_threshold: cfg.outbox_backlog_alert_threshold,
            stream_maxlen: cfg.redis_stream_maxlen,
        },
    );
    let relay_shutdown = shutdown.clone();
    let relay_handle = tokio::spawn(async move { relay.run(relay_shutdown).await });

    // Ledger-range reconciliation (issue #511): periodically re-fetches a
    // settled window from the RPC and compares per-ledger event counts
    // against the database, so silent under-indexing surfaces in minutes
    // instead of at the next incident. Read-only; stops on the same shutdown
    // signal, and needs no drain — a pass in flight holds no state worth
    // finishing.
    if cfg.reconcile_enabled {
        let reconcile_rpc = rpc::RpcClient::with_endpoints(
            cfg.stellar_rpc_urls.clone(),
            &rpc::RpcHttpSettings {
                connect_timeout: cfg.rpc_connect_timeout,
                request_timeout: cfg.rpc_request_timeout,
                pool_idle_timeout: cfg.rpc_pool_idle_timeout,
                pool_max_idle_per_host: cfg.rpc_pool_max_idle_per_host,
                tcp_keepalive: cfg.rpc_tcp_keepalive,
                max_calls_per_sec: cfg.rpc_max_calls_per_sec,
            },
        )?;
        let reconciler = reconcile::Reconciler::new(&cfg, db_pool.clone(), reconcile_rpc);
        let reconcile_shutdown = shutdown.clone();
        tokio::spawn(async move { reconciler.run(reconcile_shutdown).await });
    } else {
        tracing::warn!(
            "RECONCILE_ENABLED=false: nothing is verifying indexed counts against the RPC source"
        );
    }

    // Allow the shutdown drain to finish its in-flight work before the process
    // is killed. Kubernetes/Fly terminationGracePeriodSeconds should be ≥ this
    // value + a small buffer (recommended: SHUTDOWN_GRACE_SECS + 5).
    const SHUTDOWN_GRACE_SECS: u64 = 30;

    let mut s = streamer::Streamer::new(cfg, db_pool).await?;
    let result = s.run(shutdown).await;

    // Let the relay drain its current pass before the process exits, bounded
    // by the shutdown grace period (issue #205).
    match tokio::time::timeout(Duration::from_secs(SHUTDOWN_GRACE_SECS), relay_handle).await {
        Ok(Ok(_)) => {}
        Ok(Err(e)) => {
            tracing::warn!(error = %e, "Outbox relay task did not shut down cleanly");
        }
        Err(_) => {
            tracing::warn!(
                grace_seconds = SHUTDOWN_GRACE_SECS,
                "Graceful shutdown exceeded grace period; forcing exit"
            );
        }
    }
    result?;

    tracing::info!("Trident indexer stopped");
    Ok(())
}

/// Initialise tracing/logging, optionally enabling the tokio-console async
/// profiler.
///
/// tokio-console is opt-in on two levels so it can never be reached in a normal
/// deployment: it is compiled out unless the `tokio-console` cargo feature is
/// built, and even then it stays off unless `TOKIO_CONSOLE_ENABLED=true`. The
/// console server binds to `127.0.0.1:6669` by default (see console-subscriber
/// docs / `TOKIO_CONSOLE_BIND`), so it is not publicly reachable.
fn init_tracing(tracer: Option<opentelemetry_sdk::trace::Tracer>) {
    // Shared JSON log schema (#294) composed with OpenTelemetry trace export
    // (#290) so log lines and traces share the same correlation ids.
    let otel_layer = tracer.map(|t| tracing_opentelemetry::layer().with_tracer(t));

    #[cfg(feature = "tokio-console")]
    if std::env::var("TOKIO_CONSOLE_ENABLED").as_deref() == Ok("true") {
        use tracing_subscriber::Layer;
        let console_layer = console_subscriber::spawn();
        let json_layer =
            trident_common::logging::JsonLayer::new("trident-indexer", std::io::stdout)
                .with_filter(default_filter());
        // OTel export is intentionally omitted while attached to tokio-console —
        // this is a local debug mode, not a production tracing path.
        tracing_subscriber::registry()
            .with(console_layer)
            .with(json_layer)
            .init();
        tracing::warn!(
            "tokio-console ENABLED on 127.0.0.1:6669 — internal/debug use only, never expose publicly"
        );
        return;
    }

    tracing_subscriber::registry()
        .with(EnvFilter::from_default_env())
        .with(trident_common::logging::JsonLayer::new(
            "trident-indexer",
            std::io::stdout,
        ))
        .with(otel_layer)
        .init();
}

#[cfg(feature = "tokio-console")]
fn default_filter() -> tracing_subscriber::EnvFilter {
    tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"))
}
