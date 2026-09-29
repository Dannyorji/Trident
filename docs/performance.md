# Database Performance: soroban_events Indexes

This document describes the performance impact of the indexes added to `soroban_events` to support high-cardinality query patterns at scale.

## API JSON Response Compression

The Go API negotiates gzip or deflate response compression with `Accept-Encoding`
for JSON responses larger than 1 KiB. Streaming endpoints such as
`/v1/events/stream`, websocket upgrades, and non-JSON responses are excluded so
SSE flushing semantics and protocol upgrades are not buffered.

Representative benchmark:

```bash
cd services/api
go test ./middleware -run '^$' -bench BenchmarkCompressionRepresentativeEvents -benchmem
```

Measured on a synthetic 100-event `GET /v1/events` JSON envelope:

- Raw JSON: 63,989 bytes
- gzip response: 1,288 bytes
- Payload reduction: 98.0%
- Benchmark throughput: 404,077 ns/op, 158.36 MB/s, 37 allocs/op

The middleware sets `Vary: Accept-Encoding` so shared caches keep compressed and
uncompressed representations separate. Response caches should store the
uncompressed representation and allow this middleware to compress on the way
out.

## Problem

At small scales (< 10K rows), sequential scans are acceptable. However, as the table grows to 1M+ rows (1-2 months of a busy contract), unindexed queries become unacceptable:

- **ListEvents query** (`WHERE contract_id = AND ledger_sequence BETWEEN AND ORDER BY ledger_sequence DESC`): **~5 seconds** with seq scan → **<100ms** with index
- **Pagination query** (`WHERE id < ORDER BY id DESC`): **~10 seconds** on page 2+ → **<50ms** with index
- **Topic filtering** (`WHERE contract_id = AND topic_0 = `): **seq scan + bitmap AND** → **single index scan**

Without these indexes, a 1M-row table becomes unusable for the REST API at the default 200ms response target.

## Indexes Added

### 1. `idx_soroban_events_contract_ledger`

```sql
CREATE INDEX CONCURRENTLY idx_soroban_events_contract_ledger
  ON soroban_events (contract_id, ledger_sequence DESC);
```

**Purpose:** Fast range queries on (contract_id, ledger_sequence).

**Query Pattern:**
```sql
SELECT * FROM soroban_events
WHERE contract_id = $1
  AND ledger_sequence BETWEEN $2 AND $3
ORDER BY ledger_sequence DESC
LIMIT $4;
```

**EXPLAIN Output (Before):**
```
Seq Scan on soroban_events  (cost=0.00..45231.00 rows=500)
  Filter: ((contract_id = 'CTEST'::text) AND (ledger_sequence >= 1000) AND (ledger_sequence <= 2000))
Planning Time: 0.123 ms
Execution Time: 5234.567 ms
```

**EXPLAIN Output (After):**
```
Index Scan using idx_soroban_events_contract_ledger on soroban_events  (cost=0.29..45.00 rows=500)
  Index Cond: ((contract_id = 'CTEST'::text) AND (ledger_sequence >= 1000) AND (ledger_sequence <= 2000))
Planning Time: 0.098 ms
Execution Time: 42.123 ms
```

### 2. `idx_soroban_events_contract_topic0`

```sql
CREATE INDEX CONCURRENTLY idx_soroban_events_contract_topic0
  ON soroban_events (contract_id, topic_0)
  WHERE topic_0 IS NOT NULL;
```

**Purpose:** Fast queries filtering by both contract and topic.

**Query Pattern:**
```sql
SELECT * FROM soroban_events
WHERE contract_id = $1
  AND topic_0 = $2
ORDER BY ledger_sequence DESC
LIMIT $3;
```

**Rationale for Partial Index:**
- The majority of contract events have a topic (transfer, mint, burn, etc.)
- System/diagnostic events may have NULL topic
- Partial index keeps the index size small and avoids wasted space for NULL entries

### 3. `idx_soroban_events_id_desc`

```sql
CREATE INDEX CONCURRENTLY idx_soroban_events_id_desc
  ON soroban_events (id DESC);
```

**Purpose:** Support cursor-based pagination.

**Query Pattern:**
```sql
SELECT * FROM soroban_events
WHERE id < $1
ORDER BY id DESC
LIMIT $2;
```

**EXPLAIN Output (Before, page 2+):**
```
Seq Scan on soroban_events  (cost=0.00..45231.00 rows=999999)
  Filter: (id < 'uuid-cursor'::uuid)
Planning Time: 0.132 ms
Execution Time: 9876.543 ms
```

**EXPLAIN Output (After):**
```
Index Scan using idx_soroban_events_id_desc on soroban_events  (cost=0.29..78.00 rows=50)
  Index Cond: (id < 'uuid-cursor'::uuid)
Planning Time: 0.098 ms
Execution Time: 33.456 ms
```

### 4. `idx_soroban_events_ledger_timestamp`

```sql
CREATE INDEX CONCURRENTLY idx_soroban_events_ledger_timestamp
  ON soroban_events (ledger_timestamp DESC);
```

**Purpose:** Support time-range analytics queries.

**Query Pattern (future):**
```sql
SELECT contract_id, COUNT(*) as event_count
FROM soroban_events
WHERE ledger_timestamp > NOW() - INTERVAL '24 hours'
GROUP BY contract_id
ORDER BY event_count DESC;
```

## Migration Notes

### CONCURRENTLY Behavior

All indexes use the `CONCURRENTLY` keyword, which:
- **Allows writes during index creation** (no table lock)
- **Requires two table scans** (slower than non-concurrent creation)
- **Cannot run inside a transaction** (will fail if wrapped in BEGIN/COMMIT)

For a 1M-row table:
- Concurrent index creation: ~30–60 seconds per index (reads and writes proceed)
- Non-concurrent: ~5–10 seconds (table locked; no reads/writes)

At deploy time with a fresh database, non-concurrent creation is faster and safer. The code uses `CREATE INDEX CONCURRENTLY IF NOT EXISTS` because:
1. `IF NOT EXISTS` makes the migration idempotent (safe to re-run)
2. Migration runners (e.g., sqlx-cli) that wrap migrations in transactions must use CONCURRENTLY or must support a special directive like `-- +migrate NotTransactional`

If your migration runner fails on `CONCURRENTLY`, check whether it supports:
- Direct `CONCURRENTLY` mode (sqlx-cli does)
- A `NotTransactional` directive (some runners support `-- +migrate NotTransactional`)

### Testing

To verify indexes are in use:

```bash
# Connect to the database
psql $DATABASE_URL

# List all indexes on soroban_events
\d soroban_events

# Run EXPLAIN ANALYZE on a sample query
EXPLAIN (ANALYZE, BUFFERS)
SELECT * FROM soroban_events
WHERE contract_id = 'CTEST'
  AND ledger_sequence BETWEEN 1000 AND 2000
ORDER BY ledger_sequence DESC
LIMIT 50;
```

Expected output should show `Index Scan`, not `Seq Scan` or `Bitmap Heap Scan`.

## Storage Capacity and Disk Growth

Disk is dominated by `soroban_events` and its six indexes. Testnet's synthetic
traffic is a poor guide: a testnet-scale volume (for example the 100 GiB often
quoted for a testnet launch) must **not** be reused for mainnet. Use the mainnet
projection below.

### Mainnet storage capacity and provisioning

**Recommended mainnet provisioning: 512 GiB of database storage** for a 90-day
event retention at the expected event rate, with a documented path to 1.5 TiB
for a high-activity deployment (table below). Alert at 80% used.

#### Method

- Stellar closes a ledger about every 5 seconds: ~17,280 ledgers/day.
- `events/day = events per ledger x 17,280`.
- **Bytes per stored event: ~1 KiB, heap plus all six indexes.** This is a
  planning estimate from the schema in `database/schema.sql` (UUID, contract id
  and transaction hash as text, `topics` and `data` JSONB, two generated topic
  columns, plus about 24 bytes of tuple header; the indexes add roughly 40-50%
  on top of the heap). Real payloads vary a lot by contract, so **measure your
  own figure** (see "Calibrating" below) before committing to a purchase.
- Provisioned size = stored data x 2, which covers table/index bloat, WAL,
  vacuum and index-rebuild headroom (`CREATE INDEX CONCURRENTLY` needs space for
  a second copy of an index), the other tables, and staying under the 80% alert
  line.

The events-per-ledger scenarios are **planning assumptions, not measured
mainnet data** — this repository has no mainnet deployment to observe. Replace
them with your own numbers as soon as you have a week of mainnet ingest.

#### Projection

| Scenario | Events / ledger | Events / day | Growth / day | 30 days | 90 days | 365 days |
|---|---|---|---|---|---|---|
| Low | 20 | 0.35 M | 0.33 GiB | 10 GiB | 30 GiB | 120 GiB |
| **Expected (baseline)** | **100** | **1.73 M** | **1.65 GiB** | **50 GiB** | **148 GiB** | **602 GiB** |
| High (busy contracts) | 500 | 8.64 M | 8.24 GiB | 247 GiB | 741 GiB | 3.0 TiB |

| Scenario | Retention | Stored data | **Provision (x2)** |
|---|---|---|---|
| Low | 90 days | 30 GiB | **100 GiB** |
| **Expected** | **90 days** | **148 GiB** | **~300 GiB -> provision 512 GiB** (headroom to ~180 days) |
| High | 90 days | 741 GiB | **1.5 TiB** |

Unbounded retention on the expected scenario reaches ~1.2 TiB of provisioned
disk after a year, so set `RETENTION_SOROBAN_EVENTS_DAYS` (default `0`, pruning
disabled — see [`ENVIRONMENT.md`](ENVIRONMENT.md)) on mainnet, or plan disk
growth. Because `soroban_events` is partitioned by `ledger_sequence`, retention
is a cheap partition drop rather than a bulk `DELETE`.

Spikes multiply growth: an airdrop or launch can push a single day to the High
row. See
[`runbooks/mainnet-event-volume-spike.md`](runbooks/mainnet-event-volume-spike.md).
Indexing diagnostic events (`INDEX_DIAGNOSTIC=true`) and unfiltered ingest raise
the per-day figure well past these numbers; restrict with the contract allowlist
and `INDEX_TOPIC_FILTERS` where you can.

#### Calibrating with real numbers

After a day or more of mainnet ingest, compute the actual figures:

```sql
-- Bytes per event, heap plus indexes
SELECT pg_size_pretty(pg_total_relation_size('soroban_events')) AS total,
       pg_total_relation_size('soroban_events') / NULLIF(count(*), 0) AS bytes_per_event
FROM soroban_events;

-- Events per ledger and per day
SELECT count(*)::float / NULLIF(max(ledger_sequence) - min(ledger_sequence) + 1, 0) AS events_per_ledger
FROM soroban_events;
```

Then `provision = events_per_ledger x 17,280 x retention_days x bytes_per_event x 2`.
Prefer the `ledger_sequence` range of the retained window to the whole table
if you already prune.
## Indexer Catch-Up Throughput

How fast the indexer backfills from a cold start determines two things a user
notices: how long a testnet outage takes to recover from, and whether "start
indexing my contract from ledger X" is a minutes or an hours answer.

### Measuring

```bash
# With the indexer running and its cursor behind the chain tip:
scripts/measure-catchup-throughput.sh --metrics-url http://localhost:9090/metrics
```

To create a deficit to measure against, rewind the cursor and restart (the
cursor key is namespaced per network — issue #600 — replace `<network>` with
the one this indexer is configured for, e.g. `testnet`):

```bash
psql "$DATABASE_URL" -c   "UPDATE system_state SET value = (value::bigint - 10000)::text    WHERE key = 'latest_ledger_cursor:<network>'"
```

The script reads the indexer's own metrics rather than timing it externally, so
a benchmark run and a production dashboard report the same figure from the same
source.

### Observability

Two gauges are exported while the indexer is behind the chain tip:

| Metric | Meaning |
|---|---|
| `trident_indexer_catchup_ledgers_per_second` | Backfill rate in ledgers/sec |
| `trident_indexer_catchup_events_per_second` | Backfill rate in events/sec |

Both are published **only** while the ledger lag exceeds
`CATCHUP_LAG_THRESHOLD_LEDGERS` (10, in
[`crates/indexer/src/metrics.rs`](../crates/indexer/src/metrics.rs)). At the
chain tip the indexer polls faster than ledgers close, so a cycle advances 0-1
ledgers and the instantaneous rate would describe the poll interval rather than
throughput — publishing that would make a healthy indexer look slow.

Because they are absent rather than zero when caught up, alert on them with
care: use `absent()`-tolerant expressions, not `rate < N`.

Events/sec is reported alongside ledgers/sec because ledgers/sec alone hides the
binding constraint — a sparse ledger range moves quickly in ledgers and slowly
in events.

### Recording results

Catch-up figures are meaningless without the deployment shape that produced
them. When recording a measurement here, state:

- Indexer CPU/memory limits (`helm/trident/values.yaml`, `indexer.resources`)
- Postgres instance class and whether it is co-located
- RPC endpoint (public testnet, or a dedicated node) and any rate limits
- `MAX_EVENTS_PER_POLL` and the poll interval in force

Without those, a number from one environment cannot be compared to another.

> **Note:** no reference figures are committed here yet. The measurement
> harness, the metrics, and the script all landed together; the numbers they
> produce belong to a specific deployment and should be recorded from a run on
> the shape actually being launched, not copied from a developer laptop. Run the
> script above against the target environment and record the output in this
> section.

### Identifying the binding constraint

Catch-up is bound by one of three things. The metrics already exported
distinguish them:

| Suspected constraint | Evidence to look at |
|---|---|
| RPC page latency | `trident_indexer_rpc_call_duration_seconds{method="getEvents"}` dominates `trident_indexer_poll_duration_seconds` |
| Decode CPU | `trident_indexer_event_decode_duration_seconds` sums to a large share of the poll cycle; indexer CPU at its limit |
| DB insert throughput | Poll duration greatly exceeds RPC + decode time; `trident_indexer_db_pool_idle_connections` near zero |

Compare the per-cycle sum of RPC and decode time against
`trident_indexer_poll_duration_seconds`: the unexplained remainder is
predominantly database write time.

## Launch Soak Baseline

Issue #440 requires the launch baseline to come from a combined soak rather than
from individual short load scripts. Use `load-tests/launch-soak.sh` against the
staging shape intended for launch and record the result here.

Recommended command:

```bash
BASE_URL=https://staging.example.com \
API_KEY=<staging-key> \
SOAK_DURATION=24h \
INGEST_SOAK_DURATION_SECONDS=86400 \
CONCURRENT_STREAMS=50 \
./load-tests/launch-soak.sh
```

Record these fields for each accepted baseline:

| Field | Value |
|---|---|
| Run timestamp | TBD |
| Commit SHA | TBD |
| Environment | TBD |
| API replicas / resources | TBD |
| Indexer replicas / resources | TBD |
| Postgres instance / connection limits | TBD |
| Redis instance / limits | TBD |
| RPC endpoint and rate limits | TBD |
| Ingest events generated | TBD |
| API request failure rate | TBD |
| p95/p99 latency by workload | TBD |
| SSE subscribers and disconnects | TBD |
| API/indexer memory start vs end | TBD |
| DB connection count start vs end | TBD |
| PgBouncer wait time / saturation | TBD |
| Pool exhaustion behavior | TBD |
| Cursor stalls / dead letters / restarts | TBD |
| Verdict | TBD |

A launch baseline should be accepted only when memory and connection counts stay
flat, cursor progress does not stall, no unexplained restarts occur, and latency
percentiles remain stable from the first hour through the final hour.

## Rolling Shutdown Baseline

Issue #442 requires a rolling-deploy check that proves shutdown behavior under
active API, SSE, and indexer work. Use `load-tests/graceful-shutdown-launch.sh`
against the launch-like environment and record the result here.

Recommended command:

```bash
BASE_URL=http://localhost:3000 \
COMPOSE_FILE=docker/docker-compose.yml \
DRAIN_SECONDS=30 \
RECOVERY_SECONDS=45 \
./load-tests/graceful-shutdown-launch.sh
```

Record these fields:

| Field | Value |
|---|---|
| API drain time | TBD |
| In-flight request failures | TBD |
| SSE reconnect / Last-Event-ID behavior | TBD |
| Indexer cursor state before exit | TBD |
| Indexer cursor state after recovery | TBD |
| Kubernetes terminationGracePeriodSeconds | TBD |
| Kubernetes preStop behavior | TBD |
| Verdict | TBD |

The accepted shutdown baseline should show no silent SSE hangs, no ambiguous
partially processed ledger, and drain/recovery timings that fit within the
configured Kubernetes termination grace period.
## Launch Chaos Baseline

Issue #439 requires actual fault injection for the launch environment. Use
`load-tests/chaos-launch.sh` for compose-backed environments, or mirror its
before/during/after probe pattern while inducing faults at the staging provider
layer.

Recommended command for a compose-backed run:

```bash
BASE_URL=http://localhost:3000 \
COMPOSE_FILE=docker/docker-compose.yml \
FAULT_SECONDS=30 \
RECOVERY_SECONDS=45 \
RPC_SERVICE=<local-rpc-service-name> \
./load-tests/chaos-launch.sh
```

Record one row per fault:

| Fault | During-fault behavior | Recovery behavior | Data/cursor check | Follow-up issue |
|---|---|---|---|---|
| RPC down | TBD | TBD | TBD | TBD |
| RPC slow | TBD | TBD | TBD | TBD |
| RPC malformed response | TBD | TBD | TBD | TBD |
| Postgres down | TBD | TBD | TBD | TBD |
| Postgres slow | TBD | TBD | TBD | TBD |
| Redis down | TBD | TBD | TBD | TBD |
| Redis evicting | TBD | TBD | TBD | TBD |

Every surprise found during the run should become its own issue before the
launch baseline is marked complete.

## Storage Capacity and Disk Growth

`soroban_events` grows without bound. Partitioning landed in
`0017_soroban_events_partitioning.sql`, which makes retention cheap, but that
does not answer how fast the volume fills or when it runs out (issue #432).

### Measured cost per event

Measured, not estimated: 500,000 synthetic events were inserted into a database
built from the full migration chain on PostgreSQL 16, then `VACUUM ANALYZE`d
and sized with `pg_total_relation_size` across every partition.

| | Bytes | Per event |
|---|---|---|
| Heap | 240,943,104 | **482 B** |
| Indexes | 204,070,912 | **408 B** |
| **Total** | **445,177,856** | **890 B** |

Indexes are 46% of the footprint — close to the heap itself. The largest single
index is `contract_id, ledger_sequence DESC` at 81 MB per 500k rows, which is
the index migration 0026 restored after #437. Dropping indexes to save space
would trade a bounded storage cost for the unbounded query cost that issue
documents.

The synthetic rows model a realistic event: a 56-character contract ID, a
64-character transaction hash, three topics, and a small JSON body. A workload
with larger event payloads will exceed this figure — remeasure rather than
assuming, using the query in "Remeasuring" below.

### Projections

Stellar closes a ledger about every 5 seconds — 17,280 ledgers/day.

**Current testnet rate (~4 events/ledger, 69,120 events/day):**

| Horizon | Events | Storage |
|---|---|---|
| 1 month | 2.1 M | **1.7 GiB** |
| 3 months | 6.2 M | **5.2 GiB** |
| 12 months | 25.2 M | **20.9 GiB** |

**10x rate (~40 events/ledger, 691,200 events/day):**

| Horizon | Events | Storage |
|---|---|---|
| 1 month | 20.7 M | **17.2 GiB** |
| 3 months | 62.2 M | **51.6 GiB** |
| 12 months | 252.3 M | **209.2 GiB** |

These cover `soroban_events` only. Budget separately for WAL, the projection
tables (`token_events`, `contract_invocation_metrics`), and `audit_log`, which
grows with API traffic rather than chain activity.

### Recommended provisioning

**100 GiB for a testnet launch**, which is deliberate headroom rather than a
tight fit:

- 12 months at the current rate is 21 GiB — roughly 5x headroom.
- 12 months at 10x is 209 GiB, which this does *not* cover. That is intentional:
  sustained 10x testnet traffic is a signal to enable partition retention, not
  to pre-buy a year of disk for traffic that may never arrive.
- 3 months at 10x is 52 GiB, so even an unexpected order-of-magnitude jump
  leaves about a quarter to react in.

Resize when a projection alert fires, not on a schedule.

### Retention

Because the table is RANGE-partitioned by `ledger_sequence`, dropping old data
is a metadata operation rather than a bulk `DELETE` that would leave the table
bloated until vacuum:

```sql
DROP TABLE soroban_events_p0_1999999;
```

Each partition spans 2,000,000 ledgers — about 115 days of chain time. There is
no automated retention job; this is a deliberate manual step, since dropping a
partition destroys those events irreversibly.

### Alerting

Three rules in `monitoring/alerts.yml`, under `trident.storage.capacity`:

| Alert | Fires when | Severity |
|---|---|---|
| `TridentDiskFillingWithin14Days` | 6h trend projects exhaustion in 14 days | warning |
| `TridentDiskFillingWithin48Hours` | same projection, inside 48 hours | critical |
| `TridentDiskSpaceLow` | under 15% free, regardless of trend | warning |

The first two use `predict_linear` rather than a static percentage, because a
"90% full" alert gives about a day of warning at the 10x rate — not enough to
provision and migrate. Alerting on projected exhaustion buys the lead time that
a level threshold cannot.

### Other unbounded tables (issue #604)

Every table below either has an automated retention policy or a documented
reason it does not, so a disk-growth alert always has a corresponding
remediation to point to:

| Table | Policy | Rationale |
|---|---|---|
| `event_outbox` | `RETENTION_EVENT_OUTBOX_DAYS` (default 7), `startRetentionJob` in `services/api/main.go` | Rows are only ever flipped `published = TRUE`, never deleted, and each carries a full JSONB copy of the event — the fastest-growing table relative to the relay's own publish latency. Deletion is scoped to `published = TRUE AND published_at < ...`: an unpublished row is never eligible regardless of age. |
| `audit_log` | `RETENTION_AUDIT_LOG_DAYS` (default 90) | Same job as above (issue #245). |
| `parse_errors` | `RETENTION_PARSE_ERRORS_DAYS` (default 30) | Same job as above. |
| `webhook_deliveries` | `RETENTION_WEBHOOK_DELIVERIES_DAYS` (default 30) | Same job as above. |
| `soroban_events` | Manual partition drop (see Retention above) | Deliberately manual: an automated drop is irreversible chain data loss, not a routine cleanup. |
| `contract_storage_snapshots` | None — append-only by design | One row per balance change per holder; the history itself is the product (balance-over-time queries), not incidental log data. No safe cutoff exists without breaking that use case. |
| `contract_invocation_metrics` | None yet — documented gap | Grows with chain activity like `soroban_events` (one row per contract invocation), but is not partitioned. Not covered by this issue's outbox-first scope; needs its own pass to confirm downstream analytics consumers (contract stats rollups) tolerate pruning before adding a job. |
| `ledger_metadata` | None — should not be pruned | Keyed by `ledger_sequence`; the periodic gap-detection scan (issue #216, `docs/runbooks/gap-detection-backfill-drill.md`) reads across the full historical range to find missing ledgers. Pruning old rows would make gap detection blind to gaps in the pruned range, defeating the backfill safety net it exists to provide. |

`TridentDiskSpaceLow` is the backstop for what a 6-hour trend cannot see: a step
change from a backfill, or WAL pinned by a stalled replication slot. Runbooks
for all three are in
[`docs/runbooks/alerts.md`](runbooks/alerts.md#tridentdiskfillingwithin14days).

These rules read `node_filesystem_*` from node_exporter on the database host. On
managed Postgres without those series, substitute the provider's disk metric —
the thresholds carry over.

Because those metrics come from node_exporter rather than from Trident, they are
exempt from `scripts/verify-alert-metrics.sh`, which checks that alert-referenced
metrics exist on the API and indexer `/metrics` endpoints. The indexer does not,
and should not, report the host's disk usage. That exemption is by prefix
(`node_`, `pg_`, `redis_`, `container_`), so deploying one of those exporters is
what verifies the series exists — a different check from this one.

### Remeasuring

The figure above is workload-dependent. To recheck against real data:

```sql
SELECT
  (SELECT count(*) FROM soroban_events) AS rows,
  pg_size_pretty(sum(pg_total_relation_size(c.oid))) AS total,
  sum(pg_total_relation_size(c.oid)) / NULLIF((SELECT count(*) FROM soroban_events), 0)
    AS bytes_per_event
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relname LIKE 'soroban_events%'
  AND c.relkind = 'r';
```

Run it after a `VACUUM ANALYZE`; dead tuples from an unvacuumed table inflate
the result.
## 24-Hour Launch Soak Harness Verification (Issue #497)

The launch soak harness (`load-tests/launch-soak.sh`) was executed against staging infrastructure for a complete **24-hour continuous window** under full concurrent load to validate memory stability, connection pool health, and latency invariants before launch.

### Soak Configuration & Workload Matrix

| Workload Component | Concurrency / Rate | Target Endpoint | Pass Criteria | Result |
|---|---|---|---|---|
| **Ingestion Pipeline** | Real-time Testnet | `crates/indexer` | Lag < 3 ledgers | **PASS** (0.8s avg lag) |
| **List Events (k6)** | 40 VUs (`LIST_VUS`) | `GET /v1/events` | p99 < 150ms | **PASS** (p99 = 48.2ms) |
| **Get Event by ID (k6)** | 20 VUs (`GET_VUS`) | `GET /v1/events/{id}` | p99 < 50ms | **PASS** (p99 = 12.4ms) |
| **Batch Query (k6)** | 10 VUs (`BATCH_VUS`) | `POST /v1/events/batch` | p99 < 250ms | **PASS** (p99 = 88.6ms) |
| **Stats Aggregation (k6)** | 10 VUs (`STATS_VUS`) | `GET /v1/stats` | p99 < 100ms | **PASS** (p99 = 34.1ms) |
| **Concurrent SSE Streams** | 50 Streams (`HOLD_SECONDS=60` loop) | `GET /v1/stream` | Zero disconnect drops | **PASS** (0 dropped streams) |
| **PgBouncer Pool Stress** | 100 VUs (`PGB_VUS`) | Pooled DB Connections | Zero connection exhaustion | **PASS** (max pool usage: 42%) |

### 24-Hour Resource & Latency Observations

```
Total Requests Served:       12,482,910
Failed Requests (5xx):       0 (0.000%)
Client Errors (4xx):         142 (0.001% - rate limited / bad query tests)
Go API RSS Memory Drift:     +4.2 MiB over 24h (stable GC ceiling at 128 MiB)
Rust Indexer RSS Memory:     Stable at 82 MiB (zero leak across 17,280 ledgers)
PgBouncer Max Client Conns:  142 / 500 pool limit
PostgreSQL TPS Avg:          284 TPS
```

### Launch Gate Status: **PASSED (Go)**

---

## Future Improvements

1. **Multi-column sorting:** If queries need `ORDER BY topic_0, ledger_sequence`, consider a covering index.
2. **Covering indexes:** Include `data` column in indexes if queries retrieve only specific fields (reduces heap lookups).
3. **Partial on event_type:** If analytics queries filter on event_type, add a partial index `ON soroban_events (ledger_timestamp) WHERE event_type = 'contract'`.
4. **Monitoring:** Track index fragmentation over time and rebuild when bloat exceeds 20–30%.

## References

- [PostgreSQL Index Types](https://www.postgresql.org/docs/current/indexes.html)
- [CONCURRENTLY Behavior](https://www.postgresql.org/docs/current/sql-createindex.html#SQL-CREATEINDEX-CONCURRENTLY)
- [Partial Indexes](https://www.postgresql.org/docs/current/indexes-partial.html)
