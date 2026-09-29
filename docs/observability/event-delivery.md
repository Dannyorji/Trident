# Event delivery: outbox, at-least-once semantics and alerting

The indexer does not publish events to Redis inline with the poll loop. It
commits each event to Postgres **together with an `event_outbox` row in the same
transaction**, and a relay task publishes unpublished rows to the
`trident:events` stream (issue #200).

## Why

Writing to Postgres and then publishing to Redis as two independent steps loses
events. If the process dies — or Redis errors — after the commit but before the
`XADD`, the event exists in Postgres but never reaches live subscribers, and
there is no replay path. The gap is only visible to a client that also polls
REST, which is exactly the kind of silent hole that erodes trust in a real-time
product.

With the outbox, a committed event always carries a delivery record, so the
relay picks it up on the next pass after a restart.

## Delivery semantics: at-least-once

The relay publishes a row and then marks it published. A crash between those two
steps re-delivers the event on the next pass. **Exactly-once is not the target.**

**Consumers must dedupe by event id.** Every stream entry carries an `event_id`
field: the deterministic UUIDv5 derived from
`(contract_id, ledger_sequence, event_index)`. The same logical event always
produces the same `event_id`, so a consumer that tracks recently seen ids can
discard a repeat safely. The same id is the primary key of the `soroban_events`
row, so REST and stream data can be correlated directly.

Ordering within the stream follows the outbox `seq`. A batch stops at the first
publish failure and only the rows published before it are marked, so the next
pass resumes at the failed row rather than skipping past it.

## Metrics

| Metric | Type | Meaning |
|---|---|---|
| `trident_indexer_outbox_backlog` | gauge | Committed events not yet published |
| `trident_indexer_outbox_published_total` | counter | Events delivered to the stream by the relay |
| `trident_indexer_outbox_publish_failures_total` | counter | Failed publish attempts |
| `trident_indexer_rpc_timeouts_total` | counter | RPC calls aborted by the connect or request timeout |
| `trident_indexer_rpc_active_endpoint` | gauge | Index of the RPC endpoint in use, `0` = primary |
| `trident_indexer_rpc_failovers_total` | counter | Switches to a different RPC endpoint |
| `trident_indexer_rpc_call_duration_seconds{method,endpoint}` | histogram | RPC call latency, labelled by method (`getEvents`, `getLedgers`, `getTransaction`, `getLedgerEntries`) and endpoint pool index. Recorded for every call regardless of outcome, so `_count` also gives per-method/per-endpoint call volume (issue #294) |
| `trident_indexer_rpc_errors_total{method,error_type}` | counter | RPC failures labelled by method and `error_type`: `timeout`, `rate_limited`, `http_4xx`, `http_5xx`, `invalid_cursor`, `rpc_error`, `empty_result`, or `transport` (issue #294) |

## Alerting

A healthy relay keeps `trident_indexer_outbox_backlog` near zero. A backlog that
grows without recovering means live subscribers are missing data, even though
Postgres is up to date.

```yaml
- alert: TridentOutboxBacklogGrowing
  expr: trident_indexer_outbox_backlog > 10000
  for: 5m
  annotations:
    summary: "Outbox backlog above threshold — live subscribers are falling behind"
```

`OUTBOX_BACKLOG_ALERT_THRESHOLD` (default `10000`) controls the matching
warning the relay logs, so the log line and the alert fire on the same
condition. Tune both together.

A sustained non-zero `trident_indexer_rpc_active_endpoint` is worth alerting on
as well: the indexer is running on a fallback provider and the primary has not
recovered.

### RPC provider health (issue #294)

`trident_indexer_rpc_call_duration_seconds` and `trident_indexer_rpc_errors_total`
exist to answer one question ops otherwise has to guess at: is ingest lag
because the chain is quiet, or because the RPC provider is degraded? The
`method` and `error_type`/`endpoint` labels let a dashboard or query break a
generic "RPC is slow" alert down into "which call, against which endpoint,
failing how" without grepping logs.

The `error_type` label is deliberately coarse (not the raw upstream message)
so it's alertable and doesn't blow up cardinality:

| `error_type` | Meaning |
|---|---|
| `timeout` | Connect or request timeout (`RpcHttpSettings`, issue #214) |
| `rate_limited` | HTTP 429 |
| `http_4xx` | Non-429 4xx response |
| `http_5xx` | 5xx response |
| `invalid_cursor` | JSON-RPC error whose message mentions the pagination cursor |
| `rpc_error` | Any other JSON-RPC-level error |
| `empty_result` | 200 OK with neither `result` nor `error` set |
| `transport` | Non-timeout `reqwest` failure (connection reset, DNS, TLS, decode) |

Full rule definitions live in `observability/rpc-alerts.yml`:

- `TridentRPCHighErrorRate` — RPC error ratio above 10% for 5m (page).
- `TridentRPCHighLatency` — p95 latency above 5s for a method, for 10m (ticket).
- `TridentRPCFailoverActive` — running on a non-primary endpoint for 5m+ (ticket).
- `TridentRPCRateLimited` — sustained `rate_limited` errors for 5m+ (ticket).

A Grafana dashboard covering latency percentiles, per-method/endpoint call
volume, error rate by type, and active-endpoint/failover status is in
`observability/dashboards/rpc-health.json`.

## Public Webhook Delivery Guarantee

Webhook delivery targets at-least-once semantics, with one known gap in the
final hop.

The indexer-to-stream hop is genuinely at-least-once: the event row and its
`event_outbox` row are committed in the same Postgres transaction, and a relay
publishes unpublished rows afterwards (`crates/indexer/src/db/outbox.rs`). A
crash between commit and publish is recoverable, because the outbox row is still
there to be relayed.

The stream-to-subscriber hop is weaker. Delivery workers consume with
`XReadGroup` and retry in-process (`services/api/webhooks.go`). A worker that
crashes partway through the retry ladder leaves its entry in the consumer
group's pending list, and nothing reclaims it: there is no `XAutoClaim`,
`XClaim`, or `XPending` recovery on the webhook path today. Until that is
implemented, at-least-once holds for subscriber errors and timeouts, but **not**
for a delivery-worker crash. Do not present webhook delivery to users as an
unconditional guarantee.

Retries can reorder callbacks for the same subscription: if event A is retrying
and event B succeeds immediately, consumers may observe B before A. Webhook
consumers must deduplicate by event id and use ledger sequence plus event index
when they need deterministic ordering.

Operators should alert on three launch SLOs: delivery success ratio,
dead-letter count, and outbox-to-delivered latency.

**Replay is time-bounded.** A dead-lettered event is not silently dropped, but
`webhook_deliveries` rows are pruned hourly by
`DELETE FROM webhook_deliveries WHERE delivered_at < NOW() - INTERVAL '7 days'`
(`services/api/webhooks.go`). Replay and manual investigation are therefore
available for 7 days, after which the delivery record is gone.