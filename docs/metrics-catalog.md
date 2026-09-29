# Metrics catalog

Every Prometheus metric Trident exports, across the indexer (Rust), the Go
API, and the internal gRPC events backend. See
[`monitoring/alerts.yml`](../monitoring/alerts.yml) for the alerts built on
top of these, and [`docs/runbooks/alerts.md`](runbooks/alerts.md) for what to
do when one fires.

## Indexer (`crates/indexer`)

Scrapeable at `GET http://<indexer-host>:<METRICS_PORT>/metrics` (default
port `9090`, set via `METRICS_PORT`). Defined in
[`crates/indexer/src/metrics.rs`](../crates/indexer/src/metrics.rs).

| Name | Type | Labels | Unit | Meaning |
|---|---|---|---|---|
| `trident_indexer_ledger_lag` | gauge | — | ledgers | Chain tip minus the indexer's cursor. Zero once caught up. |
| `trident_indexer_events_total` | counter | — | events | Cumulative events indexed since process start. |
| `trident_indexer_events_skipped_total` | counter | — | events | Events skipped: diagnostic/failed-call events, or filtered by the contract allowlist. |
| `trident_indexer_parse_errors_total` | counter | — | events | Events that failed XDR decoding and were written to `parse_errors` instead of `soroban_events`. |
| `trident_scval_unexpected_variant_total` | counter | — | values | Structurally valid ScVal variants decoded where they should never appear in event payloads (`ContractInstance` / ledger-key forms); stored faithfully, surfaced via `TridentIndexerUnexpectedScValVariant` (#506). Emitted by the shared decoder in `trident-common`. |
| `trident_indexer_dead_lettered_total` | counter | — | events | Undecodable events durably written to `parse_errors` after the dead-letter insert's own retries (#414). |
| `trident_indexer_persist_dead_lettered_total` | counter | — | events | Events that decoded fine but exhausted the persist retry budget and were durably captured in `failed_events` (#508). |
| `trident_indexer_persist_dead_letter_backlog` | gauge | — | rows | `failed_events` rows awaiting replay (`replayed_at IS NULL`), refreshed each active poll cycle and on every dead-letter write; non-empty pages via `TridentIndexerPersistDeadLetterBacklog` (#508). |
| `trident_indexer_poll_duration_seconds` | histogram | — | seconds | Wall-clock time of one `poll_once` cycle (may span multiple RPC pages). |
| `trident_indexer_poll_errors_total` | counter | — | cycles | Poll cycles that returned an error (logged, cursor unaffected, retried next interval). |
| `trident_indexer_rpc_retries_total` | counter | — | retries | Retries triggered by transient `getEvents` failures (exponential backoff). |
| `trident_indexer_rpc_call_duration_seconds` | histogram | `method` (`getEvents`\|`getLedgers`) | seconds | Stellar RPC call latency, per method. |
| `trident_indexer_rpc_errors_total` | counter | `method` | calls | Stellar RPC calls that returned an error, per method. |
| `trident_indexer_rpc_breaker_state` | gauge | — | enum (0=Closed,1=Open,2=HalfOpen) | Circuit breaker state around the poll cycle (issue #197). Open means the run loop is skipping polls entirely during a sustained RPC outage. |
| `trident_indexer_rpc_consecutive_failures` | gauge | — | failures | Consecutive RPC-layer poll failures since the last success; resets to 0 on any success. Feeds the breaker's own threshold. |
| `trident_indexer_reorgs_total` | counter | — | reorgs | Ledger reorganisations detected and repaired: affected `soroban_events`/`ledger_metadata`/projection rows deleted, cursor rewound (issue #196). |
| `trident_indexer_ledger_gaps_detected_total` | counter | — | gaps | Gaps found in the processed ledger range by the periodic `ledger_metadata` scan (issue #216). Increments on every scan that still finds an unrepaired gap, not just the first time. |
| `trident_indexer_ledger_gaps_closed_total` | counter | — | gaps | Previously-enqueued `backfill_jobs` rows confirmed filled by a later scan (issue #216). |
| `trident_indexer_last_poll_timestamp_seconds` | gauge | — | unix seconds | Set once per poll-loop iteration regardless of outcome — the dead-man's-switch (#218). Stale means the loop is hung, not just slow. |
| `trident_indexer_db_pool_size` | gauge | — | connections | Current size of the indexer's own Postgres pool. |
| `trident_indexer_db_pool_idle_connections` | gauge | — | connections | Idle connections in the indexer's own Postgres pool. |
| `trident_indexer_reconcile_passes_total` | counter | - | passes | Reconciliation passes that completed a full settled-window compare against the RPC source (#511). |
| `trident_indexer_reconcile_pass_failures_total` | counter | - | passes | Reconciliation passes that aborted before producing a report. While these grow, mismatch silence is unknown, not clean. |
| `trident_indexer_reconcile_missing_events_total` | counter | - | events | Events the RPC reports (after ingest selection rules) that the database does not account for - silent under-indexing. |
| `trident_indexer_reconcile_extra_events_total` | counter | - | events | Events in the database that the RPC does not report for the window - over-indexing. |
| `trident_indexer_reconcile_discrepant_ledgers` | gauge | - | ledgers | Ledgers in the most recent pass whose counts disagreed; stays non-zero every pass until resolved. Alerted via `TridentIndexerReconciliationMismatch`. |
| `trident_indexer_reconcile_window_end_ledger` | gauge | - | ledger | Highest ledger covered by the most recent completed pass. |
| `trident_indexer_catchup_ledgers_per_second` | gauge | — | ledgers/sec | Backfill rate while behind the chain tip (issue #420). **Only exported while catching up** — absent, not zero, once the lag drops below 10 ledgers. See [performance.md](performance.md#indexer-catch-up-throughput). |
| `trident_indexer_catchup_events_per_second` | gauge | — | events/sec | Backfill rate in events, over the same window as the gauge above. Reported alongside it because ledgers/sec alone hides whether a sparse or dense range is being processed. |

## Go API (`services/api`)

Scrapeable at `GET http://<api-host>:<PORT>/metrics` (default port `3000`,
`PORT` env var; no auth required — `/metrics` is on the public-path
allowlist in `middleware.NewDBAuth`). Defined across
[`services/api/handlers/stats.go`](../services/api/handlers/stats.go),
[`services/api/middleware/metrics.go`](../services/api/middleware/metrics.go),
and
[`services/api/middleware/grpc_metrics.go`](../services/api/middleware/grpc_metrics.go).

| Name | Type | Labels | Unit | Meaning |
|---|---|---|---|---|
| `trident_api_indexer_lag_ledgers` | gauge | — | ledgers | Mirrors `trident_indexer_ledger_lag`, but **only updated as a side effect of a `GET /v1/stats/indexer` call** — stale/zero if nothing has hit that endpoint recently. Prefer the indexer's own `trident_indexer_ledger_lag` for alerting. |
| `trident_api_indexer_last_poll_timestamp_seconds` | gauge | — | unix seconds | Same update caveat as above. |
| `trident_api_indexer_events_indexed` | gauge | — | events | Same update caveat as above. |
| `trident_api_http_requests_total` | counter | `method`, `route`, `status` | requests | Every HTTP request received. `route` is the **registered ServeMux pattern** (e.g. `GET /v1/events/{id}`), not the raw URL, so path parameters never blow up cardinality. |
| `trident_api_http_request_duration_seconds` | histogram | `method`, `route`, `status` | seconds | Request latency, same labels as above. |
| `trident_api_grpc_client_requests_total` | counter | `method`, `code` | calls | Unary gRPC calls the Go API made to the internal events backend. `method` is the full gRPC method path (e.g. `/trident.Events/ListEvents`); `code` is the gRPC status code name (`OK`, `NotFound`, `Unavailable`, ...). |
| `trident_api_grpc_client_request_duration_seconds` | histogram | `method`, `code` | seconds | gRPC client call latency, same labels. |
| `trident_api_db_pool_acquired_connections` | gauge | — | connections | Connections currently checked out of the API's Postgres pool (`pgxpool.Stat().AcquiredConns()`). |
| `trident_api_db_pool_idle_connections` | gauge | — | connections | Idle (available) connections in the pool. |
| `trident_api_db_pool_total_connections` | gauge | — | connections | Idle + acquired. |
| `trident_api_db_pool_max_connections` | gauge | — | connections | Configured pool ceiling (`GO_API_DB_POOL_SIZE`, default 5). |
| `trident_api_redis_stream_length` | gauge | — | messages | `XLEN` of the `trident:events` Redis Stream — the indexer→API consumer backlog (#201). Omitted from the scrape if Redis is unreachable at scrape time. |
| `trident_retention_rows_deleted_total` | counter | `table` | rows | Rows deleted by the periodic retention job (`startRetentionJob`, `services/api/main.go`) per table (issue #604). Makes a disk-growth alert's remediation observable: a flat counter alongside a still-firing disk alert means the job is running but not keeping up, not that it silently stopped. **Note:** unlike the rest of this table, this metric is on the separate `internal/metrics` registry (`METRICS_PORT`, default `9091`), not the public `/metrics` endpoint above — that registry has no catalog section of its own yet. |

## Internal gRPC events backend (`crates/api`)

**Known gap:** `crates/api` is a pure `tonic` gRPC server with no HTTP
listener, so it has no `/metrics` endpoint of its own today. Call latency and
error rate for this service are covered *indirectly* from the client side —
`trident_api_grpc_client_requests_total` /
`trident_api_grpc_client_request_duration_seconds` above measure every call
the Go API makes to it, which is the path that actually matters for
user-facing latency/errors.
Adding a native `/metrics` endpoint to `crates/api` itself (for
concurrency/queueing metrics only visible server-side) is a reasonable
follow-up, but needs a new Cargo dependency and a regenerated `Cargo.lock` —
left out of this change to avoid hand-editing the lockfile.

## Verifying `/metrics` is scrapeable

```bash
curl -s http://localhost:9090/metrics | head   # indexer (METRICS_PORT)
curl -s http://localhost:3000/metrics | head   # Go API (PORT)
```

Both should return `# HELP` / `# TYPE` lines followed by samples in
Prometheus text exposition format (`# TYPE ... gauge|counter|histogram`).
See [`monitoring/README.md`](../monitoring/README.md) for a scrape config
that wires both into Prometheus.
