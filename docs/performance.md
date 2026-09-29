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

## Future Improvements

1. **Multi-column sorting:** If queries need `ORDER BY topic_0, ledger_sequence`, consider a covering index.
2. **Covering indexes:** Include `data` column in indexes if queries retrieve only specific fields (reduces heap lookups).
3. **Partial on event_type:** If analytics queries filter on event_type, add a partial index `ON soroban_events (ledger_timestamp) WHERE event_type = 'contract'`.
4. **Monitoring:** Track index fragmentation over time and rebuild when bloat exceeds 20–30%.

## References

- [PostgreSQL Index Types](https://www.postgresql.org/docs/current/indexes.html)
- [CONCURRENTLY Behavior](https://www.postgresql.org/docs/current/sql-createindex.html#SQL-CREATEINDEX-CONCURRENTLY)
- [Partial Indexes](https://www.postgresql.org/docs/current/indexes-partial.html)
