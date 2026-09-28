use std::time::Duration;
use trident_common::TridentError;

#[derive(Debug)]
pub struct Config {
    pub database_url: String,
    pub db_pool_size: u32,
    pub redis_url: String,
    /// Primary RPC endpoint — always the first entry of `stellar_rpc_urls`.
    pub stellar_rpc_url: String,
    /// Prioritised RPC endpoints used for health-based failover (issue #213).
    pub stellar_rpc_urls: Vec<String>,
    /// Consecutive failures on the active endpoint before failing over (issue #213).
    /// Read only by rpc::endpoints, which is currently unreferenced — see the
    /// note at the top of that module.
    #[allow(dead_code)]
    pub rpc_failover_threshold: u32,
    /// How long a failed endpoint is parked before it is probed again (issue #213).
    #[allow(dead_code)]
    pub rpc_endpoint_cooldown: Duration,
    /// Consecutive RPC-layer poll failures before the circuit breaker opens
    /// and the run loop stops attempting polls (issue #197).
    pub rpc_breaker_failure_threshold: u32,
    /// How long the breaker stays Open before allowing a single probe poll
    /// through (issue #197).
    pub rpc_breaker_cooldown: Duration,
    pub network: String,
    pub poll_interval: Duration,
    /// Shortest adaptive poll interval, applied when lag >= `lag_high_watermark`.
    pub poll_interval_floor: Duration,
    /// Longest adaptive poll interval, applied when the indexer is caught up.
    pub poll_interval_ceiling: Duration,
    /// Lag (ledgers) at or above which the floor interval applies.
    pub lag_high_watermark: u64,
    /// Hysteresis deadband (ledgers) suppressing interval churn on lag jitter.
    pub poll_hysteresis_ledgers: u64,
    /// Maximum ledgers a detected reorg is allowed to rewind before the
    /// streamer halts instead of auto-recovering (issue #196). A deeper
    /// divergence than this is treated as something an operator should look
    /// at, not something to silently delete and re-index.
    pub max_reorg_rewind_depth: u64,
    /// Maximum gaps enqueued as backfill jobs per gap-scan run (issue #216).
    /// Bounds one scan's cost and DB write volume when the table is
    /// pathologically gappy; any gaps beyond this are picked up by the next
    /// scheduled scan.
    pub gap_scan_max_per_run: i64,
    /// TCP connect timeout for RPC HTTP requests (issue #214).
    pub rpc_connect_timeout: Duration,
    /// Overall request timeout (connect, headers and body) for RPC calls (issue #214).
    pub rpc_request_timeout: Duration,
    /// How long an idle pooled connection is kept before it is dropped (issue #214).
    pub rpc_pool_idle_timeout: Duration,
    /// Maximum idle keep-alive connections retained per RPC host (issue #214).
    pub rpc_pool_max_idle_per_host: usize,
    /// TCP keep-alive probe interval for pooled RPC sockets (issue #214).
    pub rpc_tcp_keepalive: Duration,
    /// Self-imposed maximum outbound RPC calls per second (issue #661).
    pub rpc_max_calls_per_sec: u32,
    /// How often the outbox relay scans for unpublished events (issue #200).
    pub outbox_poll_interval: Duration,
    /// Maximum events published per relay pass (issue #200).
    pub outbox_batch_size: i64,
    /// Backlog size at which the relay warns that delivery is falling behind.
    pub outbox_backlog_alert_threshold: i64,
    /// Whether the ledger-range reconciliation loop runs (issue #511).
    pub reconcile_enabled: bool,
    /// Time between reconciliation passes.
    pub reconcile_interval: std::time::Duration,
    /// How many settled ledgers each pass compares against the RPC.
    pub reconcile_ledger_span: u64,
    /// How far behind the chain tip the window sits, so in-flight ledgers
    /// the streamer has not committed yet never read as discrepancies.
    pub reconcile_tip_margin: u64,
    pub index_diagnostic: bool,
    /// Topic patterns pushed into the `getEvents` RPC filter alongside the
    /// contract allowlist (issue #203). Empty means "no topic narrowing".
    pub topic_filters: Vec<Vec<String>>,
    pub max_events_per_poll: u32,
    /// Maximum rows per batched INSERT when committing a page (issue #199).
    pub db_batch_size: usize,
    pub redis_stream_maxlen: u64,
    pub metrics_port: u16,
    pub health_port: u16,
    pub alert_webhook_url: Option<String>,
    pub alert_lag_threshold: u64,
    pub alert_cooldown_minutes: u64,
    /// statement_timeout for every DB connection (ms). Prevents runaway queries
    /// from holding the pool indefinitely (#249).
    pub statement_timeout_ms: u64,
    /// idle_in_transaction_session_timeout (ms). Reclaims connections leaked by
    /// open transactions (#249).
    pub idle_in_transaction_timeout_ms: u64,
    /// How long a cached `token_metadata` row is considered fresh before the
    /// indexer re-simulates name()/symbol()/decimals() for that contract
    /// (issue #263). Applies to both positive and negative (non-token)
    /// results.
    pub token_metadata_refresh_interval: Duration,
    /// Network passphrase used to derive Stellar Asset Contract ids (issue
    /// #262). Defaults from `network` for the two well-known networks.
    pub network_passphrase: String,
    /// Operator-configured classic assets to resolve SAC events for (issue
    /// #262). Each is a `code:issuer` pair, or the literal `native` for XLM.
    pub tracked_sac_assets: Vec<crate::parser::sac::TrackedAsset>,
    /// Maximum allowable reorg depth in ledgers before halting for operator intervention (issue #196).
    pub max_reorg_depth: u64,
}

/// Default Postgres pool size for the indexer. It is a single writer with low
/// write concurrency, so a small pool is correct (issue #87).
const DEFAULT_DB_POOL_SIZE: u32 = 3;

impl Config {
    pub fn from_env() -> Result<Self, TridentError> {
        let mut errors: Vec<String> = Vec::new();

        // ── Required env vars ───────────────────────────────────────────────
        let database_url = collect_required("DATABASE_URL", &mut errors);
        if let Some(url) = &database_url {
            if let Err(e) = check_url_scheme("DATABASE_URL", url, &["postgres://", "postgresql://"])
            {
                errors.push(e);
            }
        }
        let redis_url = collect_required("REDIS_URL", &mut errors);
        if let Some(url) = &redis_url {
            if let Err(e) = check_url_scheme(
                "REDIS_URL",
                url,
                &["redis://", "rediss://", "redis+unix://"],
            ) {
                errors.push(e);
            }
        }

        // Endpoint list for failover (issue #213). STELLAR_RPC_URLS is the
        // prioritised, comma-separated form; STELLAR_RPC_URL remains valid as a
        // single-value alias so existing deployments keep working unchanged.
        let stellar_rpc_urls = match parse_endpoint_list(
            std::env::var("STELLAR_RPC_URLS").ok(),
            std::env::var("STELLAR_RPC_URL").ok(),
        ) {
            Ok(urls) => {
                if urls.is_empty() {
                    errors.push("[trident-indexer] STELLAR_RPC_URL (or STELLAR_RPC_URLS): at least one RPC endpoint is required, e.g. https://soroban-testnet.stellar.org".into());
                }
                urls
            }
            Err(e) => {
                errors.push(format!("[trident-indexer] STELLAR_RPC_URL(S): {e}"));
                Vec::new()
            }
        };

        // ── Network ─────────────────────────────────────────────────────────
        // NETWORK is written into every row this process indexes (soroban_events,
        // ledger_metadata, token_events, ...), so a typo here silently creates an
        // invisible data partition rather than failing loudly. Validated against
        // the same allowed set the database CHECK constraints enforce (migration
        // 0029) and the API layer validates (services/api/validation/events.go,
        // validNetworks) — issue #252. 'pubnet' is accepted as an alias for
        // 'mainnet', matching default_network_passphrase below.
        let network =
            match normalize_network(&std::env::var("NETWORK").unwrap_or_else(|_| "testnet".into()))
            {
                Ok(v) => v,
                Err(e) => {
                    errors.push(e);
                    String::new() // placeholder; won't be used if errors is non-empty
                }
            };

        // Network passphrase for SAC contract id derivation (issue #262).
        let network_passphrase = match std::env::var("NETWORK_PASSPHRASE") {
            Ok(v) if !v.is_empty() => v,
            _ => match default_network_passphrase(&network) {
                Ok(v) => v,
                Err(e) => {
                    errors.push(format!("{e}"));
                    String::new() // placeholder; won't be used if errors is non-empty
                }
            },
        };

        // Tracked classic assets whose SAC events should carry asset context.
        let tracked_sac_assets = match std::env::var("TRACKED_SAC_ASSETS") {
            Ok(spec) if !spec.trim().is_empty() => match parse_tracked_sac_assets(&spec) {
                Ok(v) => v,
                Err(e) => {
                    errors.push(format!("{e}"));
                    Vec::new()
                }
            },
            _ => Vec::new(),
        };

        // ── Numeric ranges (all validated in one pass) ───────────────────────
        let poll_interval_ms = parse_bounded_u64("POLL_INTERVAL_MS", 1000, 100, 60_000);
        let max_events_per_poll = parse_bounded_u64("MAX_EVENTS_PER_POLL", 200, 1, 10_000);
        let db_batch_size = parse_bounded_u64("DB_BATCH_SIZE", 1_000, 1, 10_000);
        let poll_interval_floor_ms = parse_bounded_u64("POLL_INTERVAL_FLOOR_MS", 250, 50, 60_000);
        let poll_interval_ceiling_ms =
            parse_bounded_u64("POLL_INTERVAL_CEILING_MS", 5000, 100, 600_000);
        let lag_high_watermark = parse_bounded_u64("LAG_HIGH_WATERMARK", 100, 1, 100_000_000);
        let poll_hysteresis_ledgers =
            parse_bounded_u64("POLL_HYSTERESIS_LEDGERS", 10, 0, 1_000_000);
        // Bounded reorg rewind (issue #196): Stellar reorgs in practice are
        // shallow (a handful of ledgers); a divergence deeper than this
        // default is far more likely to be an RPC serving inconsistent data
        // than a genuine consensus rollback, so it halts for an operator
        // rather than deleting a large swath of history automatically.
        let max_reorg_rewind_depth = parse_bounded_u64("MAX_REORG_REWIND_DEPTH", 50, 1, 100_000);
        let gap_scan_max_per_run = parse_bounded_u64("GAP_SCAN_MAX_PER_RUN", 100, 1, 10_000);
        let rpc_connect_timeout_ms =
            parse_bounded_u64("RPC_CONNECT_TIMEOUT_MS", 5_000, 100, 60_000);
        let rpc_request_timeout_ms =
            parse_bounded_u64("RPC_REQUEST_TIMEOUT_MS", 30_000, 500, 600_000);
        let rpc_pool_idle_timeout_ms =
            parse_bounded_u64("RPC_POOL_IDLE_TIMEOUT_MS", 90_000, 1_000, 600_000);
        let rpc_pool_max_idle_per_host =
            parse_bounded_u64("RPC_POOL_MAX_IDLE_PER_HOST", 8, 1, 1_024);
        let rpc_tcp_keepalive_ms =
            parse_bounded_u64("RPC_TCP_KEEPALIVE_MS", 60_000, 1_000, 600_000);
        // Self-imposed outbound rate limit (issue #661), tunable independently
        // of POLL_INTERVAL_MS: without it a catch-up cycle can burst hundreds
        // of getEvents pages, or one getTransaction/getLedgerEntries call per
        // transaction/contract touched in a page, back-to-back with zero
        // delay, since poll_interval only sleeps between cycles.
        let rpc_max_calls_per_sec = parse_bounded_u64("RPC_MAX_CALLS_PER_SEC", 50, 1, 10_000);
        let rpc_failover_threshold = parse_bounded_u64("RPC_FAILOVER_THRESHOLD", 3, 1, 100);
        let rpc_endpoint_cooldown_ms =
            parse_bounded_u64("RPC_ENDPOINT_COOLDOWN_MS", 30_000, 1_000, 3_600_000);
        // Circuit breaker for sustained RPC outages (issue #197). Distinct
        // from RPC_FAILOVER_THRESHOLD above: failover picks a different
        // endpoint from the configured pool, while the breaker stops polling
        // altogether once the (possibly single) endpoint has failed enough
        // consecutive times in a row.
        let rpc_breaker_failure_threshold =
            parse_bounded_u64("RPC_BREAKER_FAILURE_THRESHOLD", 5, 1, 1_000);
        let rpc_breaker_cooldown_ms =
            parse_bounded_u64("RPC_BREAKER_COOLDOWN_MS", 30_000, 1_000, 3_600_000);
        let outbox_poll_interval_ms = parse_bounded_u64("OUTBOX_POLL_INTERVAL_MS", 100, 10, 60_000);
        let outbox_batch_size = parse_bounded_u64("OUTBOX_BATCH_SIZE", 500, 1, 10_000);
        let outbox_backlog_alert_threshold =
            parse_bounded_u64("OUTBOX_BACKLOG_ALERT_THRESHOLD", 10_000, 1, 10_000_000);
        let reconcile_interval_ms =
            parse_bounded_u64("RECONCILE_INTERVAL_MS", 600_000, 10_000, 86_400_000);
        let reconcile_ledger_span = parse_bounded_u64("RECONCILE_LEDGER_SPAN", 400, 10, 100_000);
        let reconcile_tip_margin = parse_bounded_u64("RECONCILE_TIP_MARGIN", 100, 0, 10_000);
        let alert_lag_threshold = parse_bounded_u64("ALERT_LAG_THRESHOLD", 200, 1, 1_000_000);
        let alert_cooldown_minutes = parse_bounded_u64("ALERT_COOLDOWN_MINUTES", 30, 1, 10_080);
        let statement_timeout_ms =
            parse_bounded_u64("DB_STATEMENT_TIMEOUT_MS", 30_000, 100, 3_600_000);
        let idle_in_transaction_timeout_ms =
            parse_bounded_u64("DB_IDLE_IN_TRANSACTION_TIMEOUT_MS", 10_000, 100, 3_600_000);
        let token_metadata_refresh_interval_secs = parse_bounded_u64(
            "TOKEN_METADATA_REFRESH_INTERVAL_SECS",
            86_400,
            60,
            2_592_000,
        );
        let max_reorg_depth = parse_bounded_u64("MAX_REORG_DEPTH", 128, 1, 10_000);
        let db_pool_size = parse_pool_size("INDEXER_DB_POOL_SIZE", DEFAULT_DB_POOL_SIZE);
        // #215 names redis_stream_maxlen among the knobs that must be
        // range-checked. These three previously used
        // `.ok().and_then(|s| s.parse().ok()).unwrap_or(default)`, which
        // silently swallows a malformed or out-of-range value and boots on the
        // default — the exact "silently wrong defaults" the issue calls out.
        // Ports are capped at 65535; a maxlen of 0 would disable trimming and
        // let the stream grow unbounded.
        let redis_stream_maxlen = parse_bounded_u64("REDIS_STREAM_MAXLEN", 10_000, 1, 100_000_000);
        let metrics_port = parse_bounded_u64("METRICS_PORT", 9090, 1, 65_535);
        let health_port = parse_bounded_u64("HEALTH_PORT", 8080, 1, 65_535);

        // Collect all parse/range errors at once.
        for (key, result) in [
            ("POLL_INTERVAL_MS", poll_interval_ms.as_ref()),
            ("MAX_EVENTS_PER_POLL", max_events_per_poll.as_ref()),
            ("DB_BATCH_SIZE", db_batch_size.as_ref()),
            ("POLL_INTERVAL_FLOOR_MS", poll_interval_floor_ms.as_ref()),
            (
                "POLL_INTERVAL_CEILING_MS",
                poll_interval_ceiling_ms.as_ref(),
            ),
            ("LAG_HIGH_WATERMARK", lag_high_watermark.as_ref()),
            ("POLL_HYSTERESIS_LEDGERS", poll_hysteresis_ledgers.as_ref()),
            ("MAX_REORG_REWIND_DEPTH", max_reorg_rewind_depth.as_ref()),
            ("GAP_SCAN_MAX_PER_RUN", gap_scan_max_per_run.as_ref()),
            ("RPC_CONNECT_TIMEOUT_MS", rpc_connect_timeout_ms.as_ref()),
            ("RPC_REQUEST_TIMEOUT_MS", rpc_request_timeout_ms.as_ref()),
            (
                "RPC_POOL_IDLE_TIMEOUT_MS",
                rpc_pool_idle_timeout_ms.as_ref(),
            ),
            (
                "RPC_POOL_MAX_IDLE_PER_HOST",
                rpc_pool_max_idle_per_host.as_ref(),
            ),
            ("RPC_TCP_KEEPALIVE_MS", rpc_tcp_keepalive_ms.as_ref()),
            ("RPC_MAX_CALLS_PER_SEC", rpc_max_calls_per_sec.as_ref()),
            ("RPC_FAILOVER_THRESHOLD", rpc_failover_threshold.as_ref()),
            (
                "RPC_ENDPOINT_COOLDOWN_MS",
                rpc_endpoint_cooldown_ms.as_ref(),
            ),
            (
                "RPC_BREAKER_FAILURE_THRESHOLD",
                rpc_breaker_failure_threshold.as_ref(),
            ),
            ("RPC_BREAKER_COOLDOWN_MS", rpc_breaker_cooldown_ms.as_ref()),
            ("OUTBOX_POLL_INTERVAL_MS", outbox_poll_interval_ms.as_ref()),
            ("OUTBOX_BATCH_SIZE", outbox_batch_size.as_ref()),
            (
                "OUTBOX_BACKLOG_ALERT_THRESHOLD",
                outbox_backlog_alert_threshold.as_ref(),
            ),
            ("RECONCILE_INTERVAL_MS", reconcile_interval_ms.as_ref()),
            ("RECONCILE_LEDGER_SPAN", reconcile_ledger_span.as_ref()),
            ("RECONCILE_TIP_MARGIN", reconcile_tip_margin.as_ref()),
            ("ALERT_LAG_THRESHOLD", alert_lag_threshold.as_ref()),
            ("ALERT_COOLDOWN_MINUTES", alert_cooldown_minutes.as_ref()),
            ("DB_STATEMENT_TIMEOUT_MS", statement_timeout_ms.as_ref()),
            (
                "DB_IDLE_IN_TRANSACTION_TIMEOUT_MS",
                idle_in_transaction_timeout_ms.as_ref(),
            ),
            (
                "TOKEN_METADATA_REFRESH_INTERVAL_SECS",
                token_metadata_refresh_interval_secs.as_ref(),
            ),
            ("REDIS_STREAM_MAXLEN", redis_stream_maxlen.as_ref()),
            ("METRICS_PORT", metrics_port.as_ref()),
            ("HEALTH_PORT", health_port.as_ref()),
            ("MAX_REORG_DEPTH", max_reorg_depth.as_ref()),
        ] {
            if let Err(e) = result {
                errors.push(format!("[indexer] {key}: {e}"));
            }
        }
        // db_pool_size is u32, separate from the u64 batch above.
        if let Err(e) = db_pool_size.as_ref() {
            errors.push(format!("[indexer] INDEXER_DB_POOL_SIZE: {e}"));
        }

        // ── Cross-field relationship checks ──────────────────────────────────
        if let (&Ok(floor), &Ok(ceiling)) = (&poll_interval_floor_ms, &poll_interval_ceiling_ms) {
            if ceiling <= floor {
                errors.push(format!(
                    "[indexer] POLL_INTERVAL_CEILING_MS ({ceiling}) must exceed POLL_INTERVAL_FLOOR_MS ({floor})"
                ));
            }
        }
        if let (&Ok(conn), &Ok(req)) = (&rpc_connect_timeout_ms, &rpc_request_timeout_ms) {
            if req < conn {
                errors.push(format!(
                    "[indexer] RPC_REQUEST_TIMEOUT_MS ({req}) must be >= RPC_CONNECT_TIMEOUT_MS ({conn})"
                ));
            }
        }

        // ── Optional settings (parsed but not fatal on failure) ──────────────
        let index_diagnostic = std::env::var("INDEX_DIAGNOSTIC")
            .map(|v| v.eq_ignore_ascii_case("true"))
            .unwrap_or(false);

        // Enabled unless explicitly turned off: the reconciler is the proof
        // that indexed data matches the chain, so it defaults on (issue #511).
        let reconcile_enabled = std::env::var("RECONCILE_ENABLED")
            .map(|v| !v.eq_ignore_ascii_case("false"))
            .unwrap_or(true);

        let topic_filters = match std::env::var("INDEX_TOPIC_FILTERS") {
            Ok(spec) => match crate::rpc::filters::parse_topic_filters(&spec) {
                Ok(f) => f,
                Err(e) => {
                    errors.push(format!("[indexer] INDEX_TOPIC_FILTERS: {e}"));
                    Vec::new()
                }
            },
            Err(_) => Vec::new(),
        };

        let alert_webhook_url = std::env::var("ALERT_WEBHOOK_URL")
            .ok()
            .filter(|s| !s.is_empty());

        // ── Bail if any errors were collected ────────────────────────────────
        if !errors.is_empty() {
            return Err(TridentError::config(anyhow::anyhow!(
                "[trident-indexer] configuration errors (fix all and restart):\n{}",
                errors.join("\n")
            )));
        }

        // Unwrap is safe: all errors were collected above and we bailed.
        let redis_stream_maxlen = redis_stream_maxlen.unwrap();
        let metrics_port = metrics_port.unwrap() as u16;
        let health_port = health_port.unwrap() as u16;
        let poll_interval_ms = poll_interval_ms.unwrap();
        let max_events_per_poll = max_events_per_poll.unwrap();
        let db_batch_size = db_batch_size.unwrap();
        let poll_interval_floor_ms = poll_interval_floor_ms.unwrap();
        let poll_interval_ceiling_ms = poll_interval_ceiling_ms.unwrap();
        let lag_high_watermark = lag_high_watermark.unwrap();
        let poll_hysteresis_ledgers = poll_hysteresis_ledgers.unwrap();
        let max_reorg_rewind_depth = max_reorg_rewind_depth.unwrap();
        let gap_scan_max_per_run = gap_scan_max_per_run.unwrap() as i64;
        let rpc_connect_timeout_ms = rpc_connect_timeout_ms.unwrap();
        let rpc_request_timeout_ms = rpc_request_timeout_ms.unwrap();
        let rpc_pool_idle_timeout_ms = rpc_pool_idle_timeout_ms.unwrap();
        let rpc_pool_max_idle_per_host = rpc_pool_max_idle_per_host.unwrap() as usize;
        let rpc_tcp_keepalive_ms = rpc_tcp_keepalive_ms.unwrap();
        let rpc_max_calls_per_sec = rpc_max_calls_per_sec.unwrap() as u32;
        let rpc_failover_threshold = rpc_failover_threshold.unwrap() as u32;
        let rpc_endpoint_cooldown_ms = rpc_endpoint_cooldown_ms.unwrap();
        let rpc_breaker_failure_threshold = rpc_breaker_failure_threshold.unwrap() as u32;
        let rpc_breaker_cooldown_ms = rpc_breaker_cooldown_ms.unwrap();
        let outbox_poll_interval_ms = outbox_poll_interval_ms.unwrap();
        let outbox_batch_size = outbox_batch_size.unwrap() as i64;
        let outbox_backlog_alert_threshold = outbox_backlog_alert_threshold.unwrap() as i64;
        let reconcile_interval = std::time::Duration::from_millis(reconcile_interval_ms.unwrap());
        let reconcile_ledger_span = reconcile_ledger_span.unwrap();
        let reconcile_tip_margin = reconcile_tip_margin.unwrap();
        let alert_lag_threshold = alert_lag_threshold.unwrap();
        let alert_cooldown_minutes = alert_cooldown_minutes.unwrap();
        let statement_timeout_ms = statement_timeout_ms.unwrap();
        let idle_in_transaction_timeout_ms = idle_in_transaction_timeout_ms.unwrap();
        let token_metadata_refresh_interval_secs = token_metadata_refresh_interval_secs.unwrap();
        let db_pool_size = db_pool_size.unwrap();

        Ok(Self {
            database_url: database_url.unwrap(),
            db_pool_size,
            redis_url: redis_url.unwrap(),
            stellar_rpc_url: stellar_rpc_urls[0].clone(),
            stellar_rpc_urls,
            rpc_failover_threshold,
            rpc_endpoint_cooldown: Duration::from_millis(rpc_endpoint_cooldown_ms),
            rpc_breaker_failure_threshold,
            rpc_breaker_cooldown: Duration::from_millis(rpc_breaker_cooldown_ms),
            network,
            poll_interval: Duration::from_millis(poll_interval_ms),
            poll_interval_floor: Duration::from_millis(poll_interval_floor_ms),
            poll_interval_ceiling: Duration::from_millis(poll_interval_ceiling_ms),
            lag_high_watermark,
            poll_hysteresis_ledgers,
            max_reorg_rewind_depth,
            gap_scan_max_per_run,
            rpc_connect_timeout: Duration::from_millis(rpc_connect_timeout_ms),
            rpc_request_timeout: Duration::from_millis(rpc_request_timeout_ms),
            rpc_pool_idle_timeout: Duration::from_millis(rpc_pool_idle_timeout_ms),
            rpc_pool_max_idle_per_host,
            rpc_tcp_keepalive: Duration::from_millis(rpc_tcp_keepalive_ms),
            rpc_max_calls_per_sec,
            outbox_poll_interval: Duration::from_millis(outbox_poll_interval_ms),
            outbox_batch_size,
            outbox_backlog_alert_threshold,
            reconcile_enabled,
            reconcile_interval,
            reconcile_ledger_span,
            reconcile_tip_margin,
            index_diagnostic,
            topic_filters,
            max_events_per_poll: max_events_per_poll as u32,
            db_batch_size: db_batch_size as usize,
            redis_stream_maxlen,
            metrics_port,
            health_port,
            alert_webhook_url,
            alert_lag_threshold,
            alert_cooldown_minutes,
            statement_timeout_ms,
            idle_in_transaction_timeout_ms,
            token_metadata_refresh_interval: Duration::from_secs(
                token_metadata_refresh_interval_secs,
            ),
            network_passphrase,
            tracked_sac_assets,
            max_reorg_depth: max_reorg_depth.unwrap(),
        })
    }

    /// Log the effective configuration once at startup, with credentials
    /// redacted from `DATABASE_URL`/`REDIS_URL` and the webhook URL reduced to
    /// a boolean. Misconfiguration is otherwise invisible until it surfaces as
    /// a connection failure or a knob silently defaulting; a single log line
    /// naming every value actually in effect makes that diagnosable from logs
    /// alone (issue #215).
    pub fn log_effective_config(&self) {
        tracing::info!(
            database_url = %redact_url(&self.database_url),
            redis_url = %redact_url(&self.redis_url),
            stellar_rpc_urls = ?self.stellar_rpc_urls,
            network = %self.network,
            network_passphrase_configured = !self.network_passphrase.is_empty(),
            poll_interval_ms = self.poll_interval.as_millis() as u64,
            poll_interval_floor_ms = self.poll_interval_floor.as_millis() as u64,
            poll_interval_ceiling_ms = self.poll_interval_ceiling.as_millis() as u64,
            lag_high_watermark = self.lag_high_watermark,
            max_reorg_rewind_depth = self.max_reorg_rewind_depth,
            gap_scan_max_per_run = self.gap_scan_max_per_run,
            max_events_per_poll = self.max_events_per_poll,
            db_batch_size = self.db_batch_size,
            db_pool_size = self.db_pool_size,
            redis_stream_maxlen = self.redis_stream_maxlen,
            metrics_port = self.metrics_port,
            health_port = self.health_port,
            alert_webhook_configured = self.alert_webhook_url.is_some(),
            alert_lag_threshold = self.alert_lag_threshold,
            alert_cooldown_minutes = self.alert_cooldown_minutes,
            index_diagnostic = self.index_diagnostic,
            topic_filters_count = self.topic_filters.len(),
            tracked_sac_assets_count = self.tracked_sac_assets.len(),
            statement_timeout_ms = self.statement_timeout_ms,
            idle_in_transaction_timeout_ms = self.idle_in_transaction_timeout_ms,
            rpc_breaker_failure_threshold = self.rpc_breaker_failure_threshold,
            rpc_breaker_cooldown_ms = self.rpc_breaker_cooldown.as_millis() as u64,
            rpc_max_calls_per_sec = self.rpc_max_calls_per_sec,
            "Effective configuration"
        );
    }
}

/// Strip `user:pass@` userinfo from a connection URL before it is ever
/// logged. `DATABASE_URL`/`REDIS_URL` commonly embed credentials directly, and
/// a startup log line is otherwise the easiest way for a secret to leak into
/// log aggregation (issue #215).
fn redact_url(url: &str) -> String {
    let Some(scheme_end) = url.find("://") else {
        return url.to_string();
    };
    let (scheme, rest) = url.split_at(scheme_end + 3);

    // Credentials live only in the authority segment, which ends at the first
    // '/', '?' or '#'. Bounding the search there stops a later '@' — in a path
    // or query string — from being mistaken for the credential delimiter.
    let authority_end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let (authority, tail) = rest.split_at(authority_end);

    // rfind, not find: '@' is legal inside a password and common in generated
    // secrets. Splitting on the FIRST '@' in "user:p@ssw0rd@host" yields
    // "***@ssw0rd@host" — the tail of the password logged in plaintext.
    match authority.rfind('@') {
        Some(at) => format!("{scheme}***@{}{tail}", &authority[at + 1..]),
        None => url.to_string(),
    }
}

/// Validates `NETWORK` against the allowed set and normalises the `pubnet`
/// alias to `mainnet` (issue #252). Rejecting unknown values here — rather
/// than only when a DB write later trips the CHECK constraint — surfaces a
/// typo like `tesnet` at startup instead of after it has already been used
/// to derive a passphrase, filter contracts, and tag every indexed row.
///
/// `pub(crate)` so the `replay` CLI subcommand (`main::run_replay`) can
/// validate/normalise the same `NETWORK` env var the daemon does, rather than
/// duplicating the allowed-values list (issue #595).
pub(crate) fn normalize_network(network: &str) -> Result<String, String> {
    match network {
        "mainnet" | "pubnet" => Ok("mainnet".to_string()),
        "testnet" => Ok("testnet".to_string()),
        other => Err(format!(
            "[trident-indexer] NETWORK={other:?} is not a recognised network; expected one of: mainnet, testnet, pubnet"
        )),
    }
}

/// Well-known Stellar network passphrases (issue #262). Any network name
/// other than these two must set `NETWORK_PASSPHRASE` explicitly — guessing
/// would silently derive wrong SAC contract ids.
fn default_network_passphrase(network: &str) -> Result<String, TridentError> {
    match network {
        "testnet" => Ok("Test SDF Network ; September 2015".to_string()),
        "mainnet" | "pubnet" => Ok("Public Global Stellar Network ; September 2015".to_string()),
        other => Err(TridentError::config(anyhow::anyhow!(
            "[indexer] NETWORK={other:?} has no well-known passphrase; set NETWORK_PASSPHRASE explicitly"
        ))),
    }
}

/// Parse `TRACKED_SAC_ASSETS` as a comma-separated list of `CODE:ISSUER`
/// pairs, or the bare literal `native` for XLM (issue #262).
fn parse_tracked_sac_assets(
    spec: &str,
) -> Result<Vec<crate::parser::sac::TrackedAsset>, TridentError> {
    let mut assets = Vec::new();
    for part in spec.split(',') {
        let part = part.trim();
        if part.is_empty() {
            continue;
        }
        if part.eq_ignore_ascii_case("native") {
            assets.push(crate::parser::sac::TrackedAsset {
                code: "native".to_string(),
                issuer: String::new(),
            });
            continue;
        }
        let (code, issuer) = part.split_once(':').ok_or_else(|| {
            TridentError::config(anyhow::anyhow!(
                "[indexer] TRACKED_SAC_ASSETS entry {part:?} must be CODE:ISSUER or 'native'"
            ))
        })?;
        if code.is_empty() || issuer.is_empty() {
            return Err(TridentError::config(anyhow::anyhow!(
                "[indexer] TRACKED_SAC_ASSETS entry {part:?} must be CODE:ISSUER or 'native'"
            )));
        }
        assets.push(crate::parser::sac::TrackedAsset {
            code: code.to_string(),
            issuer: issuer.to_string(),
        });
    }
    Ok(assets)
}

/// Validate that a required URL-shaped env var starts with one of the
/// accepted schemes. Catches the common misconfiguration of pasting a bare
/// host/port or the wrong service's connection string (e.g. a Redis URL in
/// `DATABASE_URL`) at boot instead of surfacing it as an opaque connection
/// failure once the pool starts (issue #215).
fn check_url_scheme(key: &str, value: &str, accepted_schemes: &[&str]) -> Result<(), String> {
    if accepted_schemes.iter().any(|s| value.starts_with(s)) {
        Ok(())
    } else {
        Err(format!(
            "[indexer] {key} must start with one of {accepted_schemes:?}, got {value:?}"
        ))
    }
}

/// Read a required env var. Returns `Some(value)` on success, or pushes
/// a descriptive error into `errors` and returns `None` on failure.
fn collect_required(key: &str, errors: &mut Vec<String>) -> Option<String> {
    match std::env::var(key) {
        Ok(v) if !v.is_empty() => Some(v),
        _ => {
            errors.push(format!(
                "[indexer] {key} is required but not set (e.g. export {key}=<value>)"
            ));
            None
        }
    }
}

/// Build the prioritised endpoint list from `STELLAR_RPC_URLS` (comma-separated)
/// falling back to the single-value `STELLAR_RPC_URL` alias (issue #213).
///
/// Duplicates are dropped so a repeated URL cannot mask a real failover target,
/// and a list that contains only blanks is rejected rather than silently
/// collapsing to "no endpoints".
fn parse_endpoint_list(
    list: Option<String>,
    single: Option<String>,
) -> Result<Vec<String>, TridentError> {
    let raw = match list.filter(|s| !s.trim().is_empty()) {
        Some(s) => s,
        None => single.unwrap_or_default(),
    };

    let mut urls: Vec<String> = Vec::new();
    for part in raw.split(',') {
        let url = part.trim();
        if url.is_empty() {
            continue;
        }
        if !url.starts_with("http://") && !url.starts_with("https://") {
            return Err(TridentError::config(anyhow::anyhow!(
                "[indexer] Stellar RPC endpoint {url:?} must start with http:// or https://"
            )));
        }
        if !urls.iter().any(|existing| existing == url) {
            urls.push(url.to_string());
        }
    }

    Ok(urls)
}

/// Parse an env var as u64 with a default and inclusive [min, max] bounds.
fn parse_bounded_u64(key: &str, default: u64, min: u64, max: u64) -> Result<u64, TridentError> {
    match std::env::var(key) {
        Err(_) => Ok(default),
        Ok(raw) => {
            let v: u64 = raw.parse().map_err(|_| {
                TridentError::config(anyhow::anyhow!(
                    "[indexer] {key} must be a positive integer, got {raw:?}"
                ))
            })?;
            if v < min || v > max {
                return Err(TridentError::config(anyhow::anyhow!(
                    "[indexer] {key} must be between {min} and {max}, got {v}"
                )));
            }
            Ok(v)
        }
    }
}

/// Parse an optional positive pool-size env var, falling back to `default`.
/// A present-but-invalid value (non-numeric or zero) is a hard configuration
/// error rather than a silent fallback.
fn parse_pool_size(key: &str, default: u32) -> Result<u32, TridentError> {
    match std::env::var(key) {
        Err(_) => Ok(default),
        Ok(raw) => raw.parse::<u32>().ok().filter(|&n| n > 0).ok_or_else(|| {
            TridentError::config(anyhow::anyhow!("{key} must be a positive integer"))
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::env;

    /// Process environment is global state shared by every test thread, so all
    /// env-mutating tests serialise on this lock. Without it one test clearing
    /// `REDIS_URL` can fail an unrelated test mid-`from_env`.
    static ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

    fn env_guard() -> std::sync::MutexGuard<'static, ()> {
        // A panicking test must not poison the lock for the rest of the suite.
        ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn with_env<F: FnOnce()>(pairs: &[(&str, &str)], f: F) {
        let _guard = env_guard();
        for (k, v) in pairs {
            env::set_var(k, v);
        }
        f();
        for (k, _) in pairs {
            env::remove_var(k);
        }
    }

    fn required_vars() -> Vec<(&'static str, &'static str)> {
        vec![
            ("DATABASE_URL", "postgres://localhost/test"),
            ("REDIS_URL", "redis://localhost:6379"),
            ("STELLAR_RPC_URL", "https://soroban-testnet.stellar.org"),
        ]
    }

    #[test]
    fn missing_all_required_vars_lists_all_in_error() {
        let _guard = env_guard();
        env::remove_var("DATABASE_URL");
        env::remove_var("REDIS_URL");
        env::remove_var("STELLAR_RPC_URL");
        env::remove_var("POLL_INTERVAL_MS");
        env::remove_var("MAX_EVENTS_PER_POLL");

        let err = Config::from_env().unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("DATABASE_URL"),
            "error should mention DATABASE_URL"
        );
        assert!(msg.contains("REDIS_URL"), "error should mention REDIS_URL");
        assert!(
            msg.contains("STELLAR_RPC_URL"),
            "error should mention STELLAR_RPC_URL"
        );
    }

    #[test]
    fn missing_single_required_var_names_it() {
        let _guard = env_guard();
        env::set_var("DATABASE_URL", "postgres://localhost/test");
        env::set_var("STELLAR_RPC_URL", "https://soroban-testnet.stellar.org");
        env::remove_var("REDIS_URL");
        env::remove_var("POLL_INTERVAL_MS");
        env::remove_var("MAX_EVENTS_PER_POLL");

        let err = Config::from_env().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("REDIS_URL"));
        assert!(
            !msg.contains("DATABASE_URL"),
            "DATABASE_URL should not appear"
        );

        env::remove_var("DATABASE_URL");
        env::remove_var("STELLAR_RPC_URL");
    }

    #[test]
    fn multiple_errors_accumulated_in_single_pass() {
        let _guard = env_guard();
        env::remove_var("DATABASE_URL");
        env::remove_var("REDIS_URL");
        env::remove_var("STELLAR_RPC_URL");
        env::set_var("POLL_INTERVAL_MS", "50"); // below minimum
        env::set_var("MAX_EVENTS_PER_POLL", "99999"); // above maximum

        let err = Config::from_env().unwrap_err();
        let msg = err.to_string();
        // Required vars should all appear.
        assert!(msg.contains("DATABASE_URL"), "missing DATABASE_URL");
        assert!(msg.contains("REDIS_URL"), "missing REDIS_URL");
        assert!(msg.contains("STELLAR_RPC_URL"), "missing STELLAR_RPC_URL");
        // Out-of-range numeric vars should also appear.
        assert!(
            msg.contains("POLL_INTERVAL_MS"),
            "missing POLL_INTERVAL_MS range error"
        );
        assert!(
            msg.contains("MAX_EVENTS_PER_POLL"),
            "missing MAX_EVENTS_PER_POLL range error"
        );

        env::remove_var("POLL_INTERVAL_MS");
        env::remove_var("MAX_EVENTS_PER_POLL");
    }

    #[test]
    fn poll_interval_default_is_1000ms() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("POLL_INTERVAL_MS");
            env::remove_var("MAX_EVENTS_PER_POLL");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.poll_interval.as_millis(), 1000);
        });
    }

    #[test]
    fn poll_interval_custom_value() {
        let mut vars = required_vars();
        vars.push(("POLL_INTERVAL_MS", "500"));
        with_env(&vars, || {
            env::remove_var("MAX_EVENTS_PER_POLL");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.poll_interval.as_millis(), 500);
        });
    }

    #[test]
    fn poll_interval_below_minimum_is_rejected() {
        let mut vars = required_vars();
        vars.push(("POLL_INTERVAL_MS", "50"));
        with_env(&vars, || {
            env::remove_var("MAX_EVENTS_PER_POLL");
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("POLL_INTERVAL_MS"));
        });
    }

    #[test]
    fn poll_interval_above_maximum_is_rejected() {
        let mut vars = required_vars();
        vars.push(("POLL_INTERVAL_MS", "90000"));
        with_env(&vars, || {
            env::remove_var("MAX_EVENTS_PER_POLL");
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("POLL_INTERVAL_MS"));
        });
    }

    #[test]
    fn poll_interval_non_integer_is_rejected() {
        let mut vars = required_vars();
        vars.push(("POLL_INTERVAL_MS", "abc"));
        with_env(&vars, || {
            env::remove_var("MAX_EVENTS_PER_POLL");
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("POLL_INTERVAL_MS"));
        });
    }

    #[test]
    fn poll_interval_boundary_min_accepted() {
        let mut vars = required_vars();
        vars.push(("POLL_INTERVAL_MS", "100"));
        with_env(&vars, || {
            env::remove_var("MAX_EVENTS_PER_POLL");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.poll_interval.as_millis(), 100);
        });
    }

    #[test]
    fn poll_interval_boundary_max_accepted() {
        let mut vars = required_vars();
        vars.push(("POLL_INTERVAL_MS", "60000"));
        with_env(&vars, || {
            env::remove_var("MAX_EVENTS_PER_POLL");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.poll_interval.as_millis(), 60000);
        });
    }

    #[test]
    fn max_events_per_poll_default_is_200() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("POLL_INTERVAL_MS");
            env::remove_var("MAX_EVENTS_PER_POLL");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.max_events_per_poll, 200);
        });
    }

    #[test]
    fn max_events_per_poll_custom_value() {
        let mut vars = required_vars();
        vars.push(("MAX_EVENTS_PER_POLL", "500"));
        with_env(&vars, || {
            env::remove_var("POLL_INTERVAL_MS");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.max_events_per_poll, 500);
        });
    }

    #[test]
    fn max_events_per_poll_below_minimum_is_rejected() {
        let mut vars = required_vars();
        vars.push(("MAX_EVENTS_PER_POLL", "0"));
        with_env(&vars, || {
            env::remove_var("POLL_INTERVAL_MS");
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("MAX_EVENTS_PER_POLL"));
        });
    }

    #[test]
    fn max_events_per_poll_above_maximum_is_rejected() {
        let mut vars = required_vars();
        vars.push(("MAX_EVENTS_PER_POLL", "10001"));
        with_env(&vars, || {
            env::remove_var("POLL_INTERVAL_MS");
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("MAX_EVENTS_PER_POLL"));
        });
    }

    #[test]
    fn max_events_per_poll_invalid_string_is_rejected() {
        let mut vars = required_vars();
        vars.push(("MAX_EVENTS_PER_POLL", "not-a-number"));
        with_env(&vars, || {
            env::remove_var("POLL_INTERVAL_MS");
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("MAX_EVENTS_PER_POLL"));
        });
    }

    #[test]
    fn max_events_per_poll_boundary_min_accepted() {
        let mut vars = required_vars();
        vars.push(("MAX_EVENTS_PER_POLL", "1"));
        with_env(&vars, || {
            env::remove_var("POLL_INTERVAL_MS");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.max_events_per_poll, 1);
        });
    }

    #[test]
    fn max_events_per_poll_boundary_max_accepted() {
        let mut vars = required_vars();
        vars.push(("MAX_EVENTS_PER_POLL", "10000"));
        with_env(&vars, || {
            env::remove_var("POLL_INTERVAL_MS");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.max_events_per_poll, 10000);
        });
    }

    #[test]
    fn endpoint_list_prefers_the_multi_value_form() {
        let urls = parse_endpoint_list(
            Some("https://a.example, https://b.example".into()),
            Some("https://legacy.example".into()),
        )
        .unwrap();
        assert_eq!(urls, vec!["https://a.example", "https://b.example"]);
    }

    #[test]
    fn endpoint_list_falls_back_to_single_value_alias() {
        let urls = parse_endpoint_list(None, Some("https://legacy.example".into())).unwrap();
        assert_eq!(urls, vec!["https://legacy.example"]);
    }

    #[test]
    fn endpoint_list_drops_duplicates_and_blanks() {
        let urls = parse_endpoint_list(
            Some("https://a.example,,  ,https://a.example,https://b.example".into()),
            None,
        )
        .unwrap();
        assert_eq!(urls, vec!["https://a.example", "https://b.example"]);
    }

    #[test]
    fn endpoint_list_rejects_non_http_scheme() {
        let err = parse_endpoint_list(Some("ftp://a.example".into()), None).unwrap_err();
        assert!(err.to_string().contains("http://"));
    }

    #[test]
    fn endpoint_list_is_empty_when_nothing_is_configured() {
        assert!(parse_endpoint_list(None, None).unwrap().is_empty());
    }

    #[test]
    fn multiple_endpoints_are_read_from_env() {
        let mut vars = required_vars();
        vars.push((
            "STELLAR_RPC_URLS",
            "https://primary.example,https://secondary.example",
        ));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.stellar_rpc_urls.len(), 2);
            assert_eq!(cfg.stellar_rpc_url, "https://primary.example");
            env::remove_var("STELLAR_RPC_URLS");
        });
    }

    #[test]
    fn failover_knobs_have_defaults() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("RPC_FAILOVER_THRESHOLD");
            env::remove_var("RPC_ENDPOINT_COOLDOWN_MS");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.rpc_failover_threshold, 3);
            assert_eq!(cfg.rpc_endpoint_cooldown.as_millis(), 30_000);
        });
    }

    #[test]
    fn breaker_knobs_have_defaults() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("RPC_BREAKER_FAILURE_THRESHOLD");
            env::remove_var("RPC_BREAKER_COOLDOWN_MS");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.rpc_breaker_failure_threshold, 5);
            assert_eq!(cfg.rpc_breaker_cooldown.as_millis(), 30_000);
        });
    }

    #[test]
    fn breaker_knobs_read_custom_values() {
        let mut vars = required_vars();
        vars.push(("RPC_BREAKER_FAILURE_THRESHOLD", "10"));
        vars.push(("RPC_BREAKER_COOLDOWN_MS", "60000"));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.rpc_breaker_failure_threshold, 10);
            assert_eq!(cfg.rpc_breaker_cooldown.as_millis(), 60_000);
        });
    }

    #[test]
    fn breaker_failure_threshold_zero_is_rejected() {
        let mut vars = required_vars();
        vars.push(("RPC_BREAKER_FAILURE_THRESHOLD", "0"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("RPC_BREAKER_FAILURE_THRESHOLD"));
        });
    }

    #[test]
    fn db_batch_size_defaults_to_1000() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("DB_BATCH_SIZE");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.db_batch_size, 1_000);
        });
    }

    #[test]
    fn outbox_relay_knobs_have_defaults() {
        let vars = required_vars();
        with_env(&vars, || {
            for key in [
                "OUTBOX_POLL_INTERVAL_MS",
                "OUTBOX_BATCH_SIZE",
                "OUTBOX_BACKLOG_ALERT_THRESHOLD",
            ] {
                env::remove_var(key);
            }
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.outbox_poll_interval.as_millis(), 100);
            assert_eq!(cfg.outbox_batch_size, 500);
            assert_eq!(cfg.outbox_backlog_alert_threshold, 10_000);
            assert!(cfg.reconcile_enabled);
            assert_eq!(
                cfg.reconcile_interval,
                std::time::Duration::from_millis(600_000)
            );
            assert_eq!(cfg.reconcile_ledger_span, 400);
            assert_eq!(cfg.reconcile_tip_margin, 100);
        });
    }

    #[test]
    fn db_batch_size_custom_value() {
        let mut vars = required_vars();
        vars.push(("DB_BATCH_SIZE", "250"));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.db_batch_size, 250);
        });
    }

    #[test]
    fn db_batch_size_zero_is_rejected() {
        let mut vars = required_vars();
        vars.push(("DB_BATCH_SIZE", "0"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("DB_BATCH_SIZE"));
        });
    }

    #[test]
    fn topic_filters_default_to_empty() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("INDEX_TOPIC_FILTERS");
            let cfg = Config::from_env().unwrap();
            assert!(cfg.topic_filters.is_empty());
        });
    }

    #[test]
    fn outbox_batch_size_is_bounded() {
        let mut vars = required_vars();
        vars.push(("OUTBOX_BATCH_SIZE", "0"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("OUTBOX_BATCH_SIZE"));
        });
    }

    #[test]
    fn max_reorg_rewind_depth_defaults_to_50() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("MAX_REORG_REWIND_DEPTH");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.max_reorg_rewind_depth, 50);
        });
    }

    #[test]
    fn gap_scan_max_per_run_defaults_to_100() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("GAP_SCAN_MAX_PER_RUN");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.gap_scan_max_per_run, 100);
        });
    }

    #[test]
    fn max_reorg_rewind_depth_reads_custom_value() {
        let mut vars = required_vars();
        vars.push(("MAX_REORG_REWIND_DEPTH", "10"));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.max_reorg_rewind_depth, 10);
        });
    }

    #[test]
    fn max_reorg_rewind_depth_zero_is_rejected() {
        let mut vars = required_vars();
        vars.push(("MAX_REORG_REWIND_DEPTH", "0"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("MAX_REORG_REWIND_DEPTH"));
        });
    }

    #[test]
    fn gap_scan_max_per_run_zero_is_rejected() {
        let mut vars = required_vars();
        vars.push(("GAP_SCAN_MAX_PER_RUN", "0"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("GAP_SCAN_MAX_PER_RUN"));
        });
    }

    #[test]
    fn rpc_timeouts_have_sensible_defaults() {
        let vars = required_vars();
        with_env(&vars, || {
            for key in [
                "RPC_CONNECT_TIMEOUT_MS",
                "RPC_REQUEST_TIMEOUT_MS",
                "RPC_POOL_IDLE_TIMEOUT_MS",
                "RPC_POOL_MAX_IDLE_PER_HOST",
                "RPC_TCP_KEEPALIVE_MS",
            ] {
                env::remove_var(key);
            }
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.rpc_connect_timeout.as_millis(), 5_000);
            assert_eq!(cfg.rpc_request_timeout.as_millis(), 30_000);
            assert_eq!(cfg.rpc_pool_idle_timeout.as_millis(), 90_000);
            assert_eq!(cfg.rpc_pool_max_idle_per_host, 8);
            assert_eq!(cfg.rpc_tcp_keepalive.as_millis(), 60_000);
        });
    }

    #[test]
    fn rpc_timeouts_are_env_configurable() {
        let mut vars = required_vars();
        vars.push(("RPC_CONNECT_TIMEOUT_MS", "1500"));
        vars.push(("RPC_REQUEST_TIMEOUT_MS", "9000"));
        vars.push(("RPC_POOL_MAX_IDLE_PER_HOST", "32"));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.rpc_connect_timeout.as_millis(), 1_500);
            assert_eq!(cfg.rpc_request_timeout.as_millis(), 9_000);
            assert_eq!(cfg.rpc_pool_max_idle_per_host, 32);
        });
    }

    #[test]
    fn topic_filters_parsed_from_spec() {
        let mut vars = required_vars();
        vars.push(("INDEX_TOPIC_FILTERS", "transfer/*/*"));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.topic_filters.len(), 1);
            assert_eq!(cfg.topic_filters[0].len(), 3);
        });
    }

    #[test]
    fn rpc_request_timeout_below_connect_timeout_is_rejected() {
        let mut vars = required_vars();
        vars.push(("RPC_CONNECT_TIMEOUT_MS", "10000"));
        vars.push(("RPC_REQUEST_TIMEOUT_MS", "1000"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("RPC_REQUEST_TIMEOUT_MS"));
        });
    }

    #[test]
    fn rpc_timeout_out_of_bounds_is_rejected() {
        let mut vars = required_vars();
        vars.push(("RPC_CONNECT_TIMEOUT_MS", "0"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("RPC_CONNECT_TIMEOUT_MS"));
        });
    }

    #[test]
    fn invalid_topic_filter_spec_is_rejected() {
        let mut vars = required_vars();
        vars.push(("INDEX_TOPIC_FILTERS", "a/b/c/d/e"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("INDEX_TOPIC_FILTERS"));
        });
    }

    #[test]
    fn token_metadata_refresh_interval_defaults_to_24h() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("TOKEN_METADATA_REFRESH_INTERVAL_SECS");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.token_metadata_refresh_interval.as_secs(), 86_400);
        });
    }

    #[test]
    fn parse_pool_size_uses_default_when_unset() {
        std::env::remove_var("TEST_POOL_UNSET");
        assert_eq!(parse_pool_size("TEST_POOL_UNSET", 7).unwrap(), 7);
    }

    #[test]
    fn parse_pool_size_reads_valid_value() {
        std::env::set_var("TEST_POOL_VALID", "12");
        assert_eq!(parse_pool_size("TEST_POOL_VALID", 3).unwrap(), 12);
        std::env::remove_var("TEST_POOL_VALID");
    }

    #[test]
    fn parse_pool_size_rejects_zero_and_garbage() {
        std::env::set_var("TEST_POOL_BAD", "0");
        assert!(parse_pool_size("TEST_POOL_BAD", 3).is_err());
        std::env::set_var("TEST_POOL_BAD", "abc");
        assert!(parse_pool_size("TEST_POOL_BAD", 3).is_err());
        std::env::remove_var("TEST_POOL_BAD");
    }

    #[test]
    fn database_url_wrong_scheme_is_rejected() {
        let mut vars = required_vars();
        vars.push(("DATABASE_URL", "redis://localhost/test"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("DATABASE_URL"));
        });
    }

    #[test]
    fn database_url_bare_host_is_rejected() {
        let mut vars = required_vars();
        vars.push(("DATABASE_URL", "localhost:5432/test"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("DATABASE_URL"));
        });
    }

    #[test]
    fn database_url_accepts_postgresql_scheme() {
        let mut vars = required_vars();
        vars.push(("DATABASE_URL", "postgresql://localhost/test"));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.database_url, "postgresql://localhost/test");
        });
    }

    #[test]
    fn redis_url_wrong_scheme_is_rejected() {
        let mut vars = required_vars();
        vars.push(("REDIS_URL", "postgres://localhost/test"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("REDIS_URL"));
        });
    }

    #[test]
    fn redis_url_accepts_rediss_scheme() {
        let mut vars = required_vars();
        vars.push(("REDIS_URL", "rediss://localhost:6380"));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.redis_url, "rediss://localhost:6380");
        });
    }

    #[test]
    fn check_url_scheme_accepts_listed_scheme() {
        assert!(check_url_scheme("X", "postgres://h/d", &["postgres://"]).is_ok());
    }

    #[test]
    fn check_url_scheme_rejects_unlisted_scheme() {
        let err =
            check_url_scheme("X", "ftp://h/d", &["postgres://", "postgresql://"]).unwrap_err();
        assert!(err.contains('X'));
    }

    #[test]
    fn redact_url_strips_credentials() {
        assert_eq!(
            redact_url("postgres://user:secret@localhost:5432/trident"),
            "postgres://***@localhost:5432/trident"
        );
    }

    #[test]
    fn redact_url_leaves_credential_free_url_unchanged() {
        assert_eq!(
            redact_url("redis://localhost:6379"),
            "redis://localhost:6379"
        );
    }

    #[test]
    fn redact_url_leaves_non_url_unchanged() {
        assert_eq!(redact_url("not-a-url"), "not-a-url");
    }

    /// '@' is legal in a URL password and common in generated secrets.
    /// Splitting on the first '@' leaked the password's tail in plaintext.
    #[test]
    fn redact_url_handles_at_sign_inside_password() {
        let redacted = redact_url("postgres://user:p@ssw0rd@localhost:5432/trident");
        assert_eq!(redacted, "postgres://***@localhost:5432/trident");
        assert!(
            !redacted.contains("ssw0rd"),
            "password tail must not survive redaction: {redacted}"
        );
    }

    /// An '@' after the authority (in a path or query) is not a credential
    /// delimiter and must not be treated as one.
    #[test]
    fn redact_url_ignores_at_sign_outside_authority() {
        assert_eq!(
            redact_url("postgres://localhost:5432/db?user=a@b"),
            "postgres://localhost:5432/db?user=a@b"
        );
        assert_eq!(
            redact_url("postgres://user:secret@localhost:5432/db?opt=x@y"),
            "postgres://***@localhost:5432/db?opt=x@y"
        );
    }

    #[test]
    fn normalize_network_accepts_known_values() {
        assert_eq!(normalize_network("mainnet").unwrap(), "mainnet");
        assert_eq!(normalize_network("testnet").unwrap(), "testnet");
    }

    #[test]
    fn normalize_network_rejects_futurenet() {
        // Not yet a supported backend value: no write path (this API or the
        // indexer) accepts it today, only the SDK's client-side Network enum.
        let err = normalize_network("futurenet").unwrap_err();
        assert!(err.contains("futurenet"));
    }

    #[test]
    fn normalize_network_maps_pubnet_alias_to_mainnet() {
        assert_eq!(normalize_network("pubnet").unwrap(), "mainnet");
    }

    #[test]
    fn normalize_network_rejects_unknown_value() {
        let err = normalize_network("tesnet").unwrap_err();
        assert!(
            err.contains("tesnet"),
            "error should name the bad value: {err}"
        );
    }

    #[test]
    fn from_env_rejects_unknown_network() {
        let mut vars = required_vars();
        vars.push(("NETWORK", "tesnet"));
        with_env(&vars, || {
            let err = Config::from_env().unwrap_err();
            assert!(err.to_string().contains("NETWORK"));
            assert!(err.to_string().contains("tesnet"));
        });
    }

    #[test]
    fn from_env_normalizes_pubnet_to_mainnet() {
        let mut vars = required_vars();
        vars.push(("NETWORK", "pubnet"));
        with_env(&vars, || {
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.network, "mainnet");
        });
    }

    #[test]
    fn from_env_defaults_network_to_testnet() {
        let vars = required_vars();
        with_env(&vars, || {
            env::remove_var("NETWORK");
            let cfg = Config::from_env().unwrap();
            assert_eq!(cfg.network, "testnet");
        });
    }
}
