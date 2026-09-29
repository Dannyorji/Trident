# Runbook: mainnet event volume spike exceeds ingest capacity

This runbook covers the **indexer-side** failure mode where a genuine surge of
on-chain activity (an airdrop, a popular contract launch, a market event)
produces more Soroban events per ledger than the indexer can fetch and commit.
It is the counterpart to the API-side traffic-spike defences in
[`docs/threat-model.md`](../threat-model.md) (threat #5: rate limiting and the
global in-flight cap), which do nothing for ingest.

This is mainly a **mainnet** problem. Testnet volume never approaches the level
where per-poll page size or batch size becomes the bottleneck, so this failure
mode will not show up in staging.

Related: [`alerts.md`](alerts.md), [`../metrics-catalog.md`](../metrics-catalog.md),
[`../performance.md`](../performance.md) (storage projections).

## How ingest capacity works

Ledgers close roughly every 5 seconds (~17,280 per day). Each poll cycle the
indexer pages through `getEvents`, fetching up to `MAX_EVENTS_PER_POLL` events
per request, and commits each page in one transaction, split into INSERT
statements of at most `DB_BATCH_SIZE` rows. Between poll cycles it sleeps for
an adaptive interval between `POLL_INTERVAL_FLOOR_MS` (default 250 ms, used when
far behind) and `POLL_INTERVAL_CEILING_MS` (default 5000 ms, used when caught
up).

Sustained ingest capacity is therefore bounded by the slowest of:

1. RPC `getEvents` latency multiplied by the number of pages per cycle.
2. Database commit time per page (`DB_BATCH_SIZE`, pool size, disk IOPS).
3. The gap between poll cycles.

The indexer keeps up while `events per ledger x 0.2 ledgers/s` stays below that
capacity. A spike breaks the inequality; the cursor then falls behind the chain
tip instead of following it.

## Detection: volume, not RPC failure

The key distinction is that during a volume spike the RPC and database are
**healthy but saturated** — requests succeed, they are just not enough.

| Signal | Volume spike | RPC outage / degradation |
|---|---|---|
| `trident_indexer_ledger_lag` | rising steadily | rising or flat |
| `rate(trident_indexer_events_total[5m])` | **high and roughly constant at the indexer's ceiling** | drops toward zero |
| `rate(trident_indexer_rpc_errors_total[5m])` | ~0 | elevated |
| `rate(trident_indexer_rpc_retries_total[5m])` | ~0 | elevated |
| `trident_indexer_poll_duration_seconds` | high (many pages per cycle), no timeouts | high or timing out |
| `trident_indexer_rpc_request_duration_seconds{method="getEvents"}` | normal or mildly elevated | high |
| `trident_indexer_poll_errors_total` | ~0 | rising |
| `trident_indexer_heartbeat_timestamp_seconds` | fresh | fresh (unless hung) |
| Postgres write latency / CPU / IOPS | elevated | normal |

Useful queries:

```promql
# Ingest throughput vs. the lag it is failing to close
rate(trident_indexer_events_total[5m])
deriv(trident_indexer_ledger_lag[10m])

# Error ratio: near zero means this is not an RPC failure
rate(trident_indexer_rpc_errors_total[5m]) / rate(trident_indexer_rpc_request_duration_seconds_count[5m])
```

Confirm on-chain volume directly rather than inferring it. The events in the
most recent poll are in `system_state.events_in_last_poll`, and per-ledger
volume can be read from the table:

```sql
SELECT ledger_sequence, count(*) AS events
FROM soroban_events
WHERE ledger_sequence > (SELECT max(ledger_sequence) - 200 FROM soroban_events)
GROUP BY 1 ORDER BY 1 DESC LIMIT 20;
```

If a single `contract_id` dominates the recent rows, the spike is one contract
and a filter (below) is the fastest fix:

```sql
SELECT contract_id, count(*) FROM soroban_events
WHERE ledger_sequence > (SELECT max(ledger_sequence) - 500 FROM soroban_events)
GROUP BY 1 ORDER BY 2 DESC LIMIT 10;
```

`TridentIndexerLagWarning` / `TridentIndexerLagCritical` are the alerts that
fire; use this table to decide whether to follow this runbook or the RPC-error
path in [`alerts.md`](alerts.md).

## Immediate mitigation

Work down the list; each step is independent and reversible. Restart the
indexer after changing environment variables.

1. **Confirm it is a volume problem** using the table above. If RPC errors are
   elevated, stop here and follow `alerts.md` instead.
2. **Make sure diagnostic events are off.** `INDEX_DIAGNOSTIC=false`. Diagnostic
   events are high-volume and are the cheapest thing to drop.
3. **Raise `MAX_EVENTS_PER_POLL`** (default 200, max 10000) so each `getEvents`
   round trip carries more events. Step up gradually (200 -> 1000 -> 2500) and
   watch `trident_indexer_rpc_request_duration_seconds` and indexer memory; stop
   when latency per event stops improving or the RPC provider rejects the limit.
4. **Raise `DB_BATCH_SIZE`** (default 1000, max 10000) in step with the page
   size so each page commits in few statements. Keep it at or below
   `MAX_EVENTS_PER_POLL` — larger has no effect.
5. **Keep the indexer in fast-poll mode.** Ensure `POLL_INTERVAL_FLOOR_MS` is
   low (default 250, min 50) and `LAG_HIGH_WATERMARK` is not so high that the
   indexer stays in the slow interval while behind.
6. **Narrow what is indexed.** If one noisy contract is not one you serve,
   restrict ingest with the contract allowlist (`indexed_contracts`) and/or
   `INDEX_TOPIC_FILTERS` (see
   [`../indexer-event-filtering.md`](../indexer-event-filtering.md)). Filters
   reduce load at the RPC, not only in the database. Note this is a product
   decision: events for excluded contracts are not stored, and lifting the
   filter later means a backfill.
7. **Give the writer more database headroom.** Raise `INDEXER_DB_POOL_SIZE`
   only if pool metrics (`trident_indexer_db_pool_idle_connections` at 0) show
   the pool is saturated, and re-check PgBouncer sizing per
   [`../deployment.md`](../deployment.md#sizing-pools-as-replicas-scale). If
   Postgres disk IOPS or CPU is the limit, scale the instance instead.
8. **Protect stream consumers.** The Redis stream is capped at
   `REDIS_STREAM_MAXLEN` (default 10000). Under a spike it can trim entries
   before slow WebSocket/gRPC consumers read them. Watch
   `trident_api_redis_stream_length`; raise `REDIS_STREAM_MAXLEN` if it sits at
   the cap and Redis has memory. Consumers can recover missed events through the
   REST API by cursor.
9. **Communicate.** The API is serving correct but stale data while lag is high.
   `GET /v1/ready` reports `indexer_lag`; tell downstream consumers the data
   window is delayed rather than incorrect.

Do **not** raise `POLL_INTERVAL_MS`/ceiling, add API replicas, or tighten API
rate limits — none of them affect ingest, and API replicas add database
connections that compete with the indexer.

### Verifying recovery

`trident_indexer_ledger_lag` should trend downward: the indexer now ingests
faster than the chain produces. If the lag is flat, capacity roughly equals
inflow; if it is still rising, move to the next mitigation or the capacity
response below. Once lag is back under 200 and the alert clears, revert
temporary changes you do not want long term (allowlist, topic filters).

## Longer-term capacity response

- **Vertical first.** The indexer is a single writer, so it does not scale
  horizontally. Give Postgres faster storage (provisioned IOPS/NVMe) and enough
  CPU, and put the indexer close to the database (same region/zone) to cut
  per-commit latency.
- **Tune with a measured baseline.** Record steady-state `events_total` rate,
  events per ledger p50/p99, and poll duration. Choose `MAX_EVENTS_PER_POLL`
  and `DB_BATCH_SIZE` so a p99 ledger fits in one or two pages.
- **Use a dedicated or private RPC node.** Public endpoints rate-limit; a
  provider plan or self-hosted node with generous `getEvents` limits raises the
  ceiling. Configure a second endpoint for failover (`RPC_FAILOVER_THRESHOLD`,
  `RPC_ENDPOINT_COOLDOWN_MS`). See the mainnet RPC section of
  [`../deployment.md`](../deployment.md).
- **Watch RPC retention.** Soroban RPC only serves a bounded window of history.
  If lag grows past that window the missed ledgers cannot be fetched from that
  node and need a node with longer retention or a history archive backfill. Do
  not let a spike run unattended for days.
- **Plan storage.** A spike multiplies row growth. Re-check disk against
  [`../performance.md`](../performance.md#mainnet-storage-capacity-and-provisioning)
  and consider `RETENTION_SOROBAN_EVENTS_DAYS`.
- **Alert earlier.** Lag alerts fire after 10 minutes above 200 ledgers. For
  mainnet consider an alert on `deriv(trident_indexer_ledger_lag[15m]) > 0`
  sustained for 15 minutes with `rpc_errors` near zero, which identifies volume
  saturation before lag is large.
- **Load test.** Replay a synthetic burst at your expected p99 events-per-ledger
  in staging before launch to find the actual ceiling for your configuration.

## Post-incident

Record peak events per ledger, peak lag, time to recover, and which mitigation
worked. Update the capacity numbers in `performance.md` and the settings in
`.env.example` if the defaults proved inadequate for mainnet.
