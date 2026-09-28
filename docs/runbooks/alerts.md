# Alert runbook

One section per alert in [`monitoring/alerts.yml`](../../monitoring/alerts.yml).
Each section covers what the alert means, why its threshold was picked, and
the first steps to take when it fires. See
[`docs/metrics-catalog.md`](../metrics-catalog.md) for what every metric
referenced here actually measures. Routing (which severity/service pages
whom) is configured in [`monitoring/alertmanager.yml`](../../monitoring/alertmanager.yml) —
"page on-call" below means whatever's wired into that file's
`on-call-critical`/`on-call-warning` receivers.

**Related runbooks:**
- [`incident-response.md`](incident-response.md) — severity classification (SEV-1/2/3), on-call owner, escalation path, and user communication channel.
- [`alert-routing.md`](alert-routing.md) — Alertmanager configuration, pre-launch routing test checklist, and maintenance silences.
- [`post-incident-review.md`](post-incident-review.md) — PIR template; complete within 72 hours of resolving any SEV-1 or SEV-2 incident.

## TridentIndexerLagWarning

**Means:** the indexer is more than 200 ledgers behind the Stellar chain tip,
sustained for 10 minutes.

**Why this threshold:** 200 ledgers is the same default as
`ALERT_LAG_THRESHOLD`, the indexer's own app-level webhook alert (issue #75)
— reusing it keeps the two alerting paths (webhook + Prometheus) agreeing on
what "behind" means. The 10-minute `for` absorbs normal RPC jitter and brief
upstream slowdowns without paging.

**First steps:**
1. Check `trident_indexer_rpc_call_duration_seconds` and
   `trident_indexer_rpc_errors_total` — is the Stellar RPC node slow or
   erroring? (see `TridentIndexerRPCErrorRateHigh` below)
2. Check `trident_indexer_db_pool_size`/`_idle_connections` — is the
   indexer's own Postgres write path the bottleneck?
3. If both look healthy, check indexer logs for retries/backoff — the RPC
   node may be silently rate-limiting.

## TridentIndexerLagCritical

**Means:** the indexer is more than 1000 ledgers behind, sustained for 5
minutes — API consumers are now reading meaningfully stale data.

**Why this threshold:** 1000 ledgers (~1.5 hours of Stellar ledger time) is
well past "temporarily slow" and into "effectively stalled." The shorter
5-minute window reflects the higher severity — page sooner once lag is this
large.

**First steps:** same as `TridentIndexerLagWarning`, but treat as
page-worthy immediately; also check `TridentIndexerHeartbeatStale` — a lag
this large with a stale heartbeat means the poll loop itself is hung, not
just slow.

## TridentIndexerHeartbeatStale

**Means:** `trident_indexer_last_poll_timestamp_seconds` — updated once per
poll-loop iteration regardless of outcome — hasn't advanced in over 5
minutes.

**Why this threshold:** the heartbeat ticks once per loop iteration
(typically every `POLL_INTERVAL_MS`, default 1s, capped at 60s). 5 minutes is
a large multiple of even the slowest configured poll interval, so staleness
past that point means the loop itself is hung — deadlocked, panicked past a
supervisor's catch, or blocked on I/O that will never return — not just
running slowly.

**First steps:**
1. Check indexer process status / restart count (`kubectl get pods` or
   `docker compose ps`) — has it actually crashed and is failing to restart?
2. If the process is running but heartbeat is stale, capture a stack
   dump/profile if possible before restarting — this is the "hung but alive"
   case the dead-man's-switch exists to catch.
3. Restart the indexer; the cursor is persisted in `system_state`, so a
   restart resumes safely without reprocessing or data loss.

## TridentIndexerMetricsMissing

**Means:** no `trident_indexer_last_poll_timestamp_seconds` series exists at
all — Prometheus can't find the metric, as opposed to finding it stale.

**Why this threshold:** distinguishes "the indexer is emitting metrics but
hung" (`TridentIndexerHeartbeatStale`) from "the indexer's `/metrics` isn't
scrapeable at all" (crashed, network partition, misconfigured scrape target).
3 minutes gives one scrape-interval's worth of margin before treating it as
real.

**First steps:** check the indexer process is running and `/metrics` is
reachable from Prometheus's network (`curl http://indexer:9090/metrics`);
check Prometheus's Targets page for scrape errors on the `trident-indexer`
job.

## TridentIndexerProcessDown

**Means:** Prometheus's own `up{job="trident-indexer"}` is 0 — the scrape
itself is failing (connection refused/timeout), for 2 minutes.

**Why this threshold:** `up` is the standard, library-level signal for "this
target isn't reachable at all." 2 minutes covers one or two missed scrapes
without paging on a single transient network blip.

**First steps:** same as `TridentIndexerMetricsMissing` — this is usually the
same underlying problem (process down or network partition) observed from
Prometheus's scrape health instead of the metric's own staleness.

## TridentIndexerParseErrorRateHigh

**Means:** over 1% of events in the last 10 minutes failed XDR decoding and
were written to `parse_errors` (parse-error isolation) instead of being
indexed.

**Why this threshold:** a small, steady trickle of parse errors is expected
(unusual contract event shapes, XDR edge cases) and shouldn't page. 1%
sustained for 15 minutes indicates something systemic — e.g. an RPC/XDR
schema change the parser doesn't handle — rather than a handful of one-off
malformed events.

**First steps:**
1. Query `parse_errors` for the most recent rows and inspect `raw_payload` /
   `error_message` for a common pattern.
2. Check whether a Stellar protocol upgrade or RPC node version bump
   coincides with the spike.
3. If it's a new, valid event shape, this is a parser bug — file/fix rather
   than treating it as transient.

## TridentIndexerDeadLetteredEventsDetected

**Means:** one or more events failed XDR decoding and were durably written to
`parse_errors` (issue #414). `trident_indexer_dead_lettered_total` only
increments once the dead-letter row is actually committed — after its own
bounded retry — so this never fires for an event that was merely attempted
and lost outright.

**Why this threshold:** unlike the rate-based
`TridentIndexerParseErrorRateHigh` alert, a healthy indexer keeps this
counter flat, so any increase is worth a look. `> 0 over 1h, for 5m` catches
every occurrence without paging on a single scrape.

This is distinct from `TridentIndexerPersistDeadLetterBacklog`: that alert
covers events which decoded fine but failed to *persist* into
`soroban_events` (issue #208) and are captured in `failed_events` instead.
This alert covers events that never decoded at all.

**First steps:**
1. Query `parse_errors` for the most recent rows and inspect `raw_payload` /
   `error_message` for a common pattern.
2. Check whether a Stellar protocol upgrade or RPC node version bump
   coincides with the spike.
3. If it's a new, valid event shape, this is a parser bug — file/fix rather
   than treating it as transient.

## TridentIndexerUnexpectedScValVariant

**Means:** an event payload contained an ScVal variant that no well-behaved
contract emits — `ContractInstance`, `LedgerKeyContractInstance`, or
`LedgerKeyNonce`. The decoder (issue #506) stored the value faithfully as a
tagged JSON object; nothing was lost or coerced, but the traffic is
anomalous.

**Why this threshold:** any occurrence at all is worth a look — these
variants exist for ledger entries, not event payloads, so a contract putting
them into events is at best confused and at worst probing the decoder. `> 0
over 1h, for 5m` surfaces every occurrence without flapping on a single
scrape.

Note this alert cannot fire for *unknown* variants: the decoder matches the
`ScVal` enum exhaustively with no fallback arm, so a variant added by an XDR
upgrade fails compilation instead of reaching production.

**First steps:**
1. Find the warn-level `decoded an ScVal variant that should not appear in
   event payloads` log lines — they name the variant and decode context.
2. Identify the emitting contract from the surrounding event logs and review
   what it publishes.
3. If a legitimate new use appears for one of these variants in event
   payloads, decide its first-class rendering and demote it from the
   anomalous set.
## TridentIndexerReconciliationMismatch

**Means:** the reconciliation loop (issue #511) re-fetched a settled ledger
window from `getEvents` - applying the ingest pipeline's own filter and skip
rules - and the per-ledger event counts disagree with what the database
holds (`soroban_events` plus `parse_errors`). Some ledgers are
under-indexed (missing events) or over-indexed (extra events).

**Why this threshold:** any disagreement at all means the indexed data is
wrong for those ledgers; the gauge is refreshed every pass (default 10
minutes), so `for: 15m` means at least two consecutive passes agreed on the
disagreement. Warning rather than critical while the detector is new;
ratchet to critical once it has run clean on testnet for a while.

**First steps:**
1. Find the `Reconciliation discrepancy` warn logs - they name each ledger
   range with the RPC and database counts
   (`trident_indexer_reconcile_missing_events_total` vs
   `_extra_events_total` says which direction).
2. For missing events, re-ingest the reported ranges:
   `trident-backfill --from-ledger <from> --to-ledger <to>` (idempotent).
   Use `--dry-run` first to preview counts for any range on demand.
3. If the discrepancy reappears on later passes for NEW ranges, the ingest
   pipeline is dropping events right now - check parse-error rates, RPC
   health, and recent deploys before backfilling further.
4. Extra events (indexed rows the chain does not report) usually mean a
   backfill wrote rows outside the allowlist rules or a duplicate-index bug
   - inspect the rows in the reported range before deleting anything.

## TridentIndexerReconciliationFailing

**Means:** the reconciliation loop keeps aborting before producing a report
- `getLatestLedger`/`getEvents` failures, or database errors during the
count queries.

**Why this threshold:** a single failed pass self-heals next interval; more
than two failures inside 30 minutes, sustained for 30 minutes, means the
loop has effectively stopped verifying. While this fires, the mismatch
alert's silence is unknown, not clean.

**First steps:**
1. Check the `Reconciliation pass failed` warn logs for the error.
2. If RPC-related, see `TridentIndexerRPCErrorRateHigh` - the reconciler
   shares the endpoint pool and fails alongside it.
3. If the indexer cursor has not yet reached the settled window (fresh
   deploy, deep backfill), passes fail with "nothing to reconcile yet" -
   expected until the indexer catches up.

## TridentIndexerRetainedFloorLedgersSkipped

**Means:** the indexer's cursor was behind the RPC's retention window - the
RPC rejected `getEvents` with "startLedger must be within the ledger range"
(issue #388) - and the live poll loop recovered by jumping the cursor
forward to the oldest ledger the RPC still retains. Every ledger strictly
between the old cursor and that floor is permanently unreachable through
live polling; the indexer then reports healthy (lag returns to 0, heartbeat
fresh) with no other signal that history was lost.

**Why this threshold:** any occurrence at all is real, permanent data loss
for that range - there is no self-healing pass that would ever notice it on
its own. `for: 0m` fires immediately; the 15-minute `increase()` window just
avoids double-counting the same jump across scrape intervals.

**First steps:**
1. Find the "startLedger predates the RPC's retained history" warn log -
   it names the `retained_floor` and the new `cursor`.
2. Confirm a `backfill_jobs` row was enqueued for the skipped range: `SELECT
   * FROM backfill_jobs WHERE network = '<network>' AND status IN
   ('pending', 'running') ORDER BY created_at DESC LIMIT 5;`
3. Run `crates/backfill --from-queue` against a source that still retains
   the range (an archival RPC endpoint, if configured) before it prunes
   further.
4. If the indexer was down long enough to hit this repeatedly, check
   `TridentIndexerProcessDown`/deploy history for why it fell behind by more
   than one retention window in the first place.

## TridentIndexerRPCErrorRateHigh

**Means:** over 5% of Stellar RPC calls (`getEvents`/`getLedgers`) errored in
the last 5 minutes (issue #297).

**Why this threshold:** RPC nodes occasionally return isolated errors under
load; 5% sustained for 10 minutes is well above normal noise and usually
means the upstream node is degraded or rate-limiting.

**First steps:**
1. Check `trident_indexer_rpc_call_duration_seconds` for the same method
   — is latency also elevated (overload) or normal (outright rejections)?
2. Check the RPC provider's status page / try a manual `getHealth` call
   against `STELLAR_RPC_URL`.
3. If a fallback/alternate RPC endpoint is configured, consider failing over.

## TridentIndexerRPCErrorRateCritical

**Means:** over 25% of Stellar RPC calls errored in the last 5 minutes.

**Why this threshold:** at this error rate the indexer is very likely
effectively stalled (most poll cycles failing outright). Page immediately
rather than waiting the full 10-minute window used for the warning tier.

**First steps:** same as `TridentIndexerRPCErrorRateHigh`, treated as
immediate: fail over to an alternate RPC endpoint if one exists, or escalate
to the RPC provider.

## TridentAPIHTTP5xxRateHigh

**Means:** over 5% of Go API requests returned a 5xx in the last 5 minutes,
sustained for 10 minutes.

**Why this threshold:** individual 5xx responses happen (a single bad
request, a momentary DB blip); 5% sustained is a real, ongoing failure mode
rather than noise.

**First steps:**
1. Check `trident_api_db_pool_acquired_connections` /
   `trident_api_db_pool_max_connections` — is the pool saturated?
2. Check `TridentAPIDependencyUnhealthy` — is Postgres/Redis/gRPC down?
3. Break down by `route` (`trident_api_http_requests_total{status=~"5..",...}`)
   to see if it's isolated to one endpoint or API-wide.

## TridentAPIHTTP5xxRateCritical

**Means:** over 25% of Go API requests are failing with a 5xx, sustained for
5 minutes — the API is largely unusable.

**Why this threshold:** at 25%+ the service is failing for a meaningful
fraction of all callers; the shorter window pages faster than the warning
tier.

**First steps:** same as `TridentAPIHTTP5xxRateHigh`, treated as immediate —
check dependency health first since a downed Postgres/Redis is the most
common cause of an API-wide 5xx spike this large.

## TridentAPIProcessDown

**Means:** `up{job="trident-api"}` is 0 for 2 minutes — Prometheus can't
scrape the Go API at all.

**Why this threshold:** same reasoning as `TridentIndexerProcessDown` — `up`
is the standard scrape-health signal, 2 minutes covers a missed scrape or
two without paging on a blip.

**First steps:** check process status and container/pod logs; check
`readinessProbe`/`livenessProbe` results if running under Kubernetes (see
`helm/trident/values.yaml`).

## TridentAPIDependencyUnhealthy

**Means:** `GET /v1/health` has been returning non-200 for 5 minutes. That
endpoint checks Postgres, Redis, and the gRPC backend concurrently and fails
if any one of them does.

**Why this threshold:** relies on the Helm chart's liveness/readiness probes
(or an equivalent external health check) polling `/v1/health` every ~10s to
keep `trident_api_http_requests_total{route="GET /v1/health"}` moving; 5
minutes gives ample margin over that polling interval before treating a
failure as sustained rather than a single flaky check. If you deploy without
those probes (e.g. bare `docker compose`), add an equivalent periodic health
check — otherwise this alert has no samples to evaluate.

**First steps:**
1. `curl` `/v1/health` directly and read the `checks` field
   (`postgres`/`redis`/`grpc_api`) to see which dependency is failing.
2. If Postgres: check `TridentAPIDBPoolSaturated` and the database's own
   health/connection count.
3. If Redis: check Redis process health and `trident_api_redis_stream_length`
   for a consumer that's stopped reading.

## TridentAPIDBPoolSaturated

**Means:** over 90% of the Go API's Postgres connection pool has been
checked out for 10 minutes.

**Why this threshold:** 90% is a leading indicator — the pool isn't
exhausted yet, but requests will start queuing (and eventually timing out on
acquire) once it is. 10 minutes filters out brief bursts that a healthy pool
absorbs on its own.

**First steps:**
1. Check for slow queries or a query holding connections open longer than
   expected (`pg_stat_activity` on the database).
2. Check request volume — is this organic traffic growth that needs a larger
   `GO_API_DB_POOL_SIZE`, or a leak/regression?
3. As a mitigation, `GO_API_DB_POOL_SIZE` can be raised without a code
   change, but treat it as a stopgap if the root cause is a query regression.


---

## TridentDiskSpaceLow

**Means:** less than 15% of the Postgres data volume remains, regardless of trend.

**Why this threshold:** the two predictive alerts above extrapolate a 6-hour trend, which cannot see a step change — a large backfill, a WAL pileup behind a stalled replication slot, or a runaway temp file. This is the backstop for those, so it fires on the level rather than the slope.

**First steps:**
1. Identify the consumer: `pg_ls_waldir()` for WAL,
   `pg_stat_replication` / `pg_replication_slots` for a stalled slot, and the
   table-size query above for ordinary growth.
2. A stalled replication slot is the most common non-obvious cause — an
   inactive slot pins WAL indefinitely. Drop it if the replica is genuinely
   gone: `SELECT pg_drop_replication_slot('<name>');`
3. If it is ordinary growth, treat it as
   `TridentDiskFillingWithin48Hours` above.

---

## TridentPartitionExhaustionWarning

**Means:** `trident_indexer_partition_lookahead_ledgers` — the distance in
ledgers between the current ingest cursor and the upper bound of the last named
`soroban_events` partition — has been below 5,000,000 for 30 minutes.

**Why this threshold:** at Stellar's current rate of ~17,280 ledgers/day,
5,000,000 ledgers is ~289 days of runway. That is intentionally generous: adding
a partition is a single SQL call but requires operator attention, and the
indexer will halt entirely (Fatal error, see below) if the boundary is actually
reached. This warning fires while there is still months of time to act, not
days.

**First steps:**
1. Confirm the current boundary — query the DB directly:
   ```sql
   SELECT MAX(
       (regexp_match(
           pg_get_partition_constraintdef(child.oid),
           'ledger_sequence < (\d+)'
       ))[1]::bigint
   )
   FROM pg_inherits
   JOIN pg_class parent ON parent.oid = inhparent
   JOIN pg_class child  ON child.oid  = inhrelid
   WHERE parent.relname = 'soroban_events';
   ```
2. Add the next partition. Each partition covers 2,000,000 ledgers; extend
   from the current upper bound:
   ```sql
   SELECT create_soroban_partition(60000000, 62000000);
   -- then the next one:
   SELECT create_soroban_partition(62000000, 64000000);
   ```
   The `create_soroban_partition` function (defined in migration
   `0017_soroban_events_partitioning.sql`) is idempotent — calling it for
   a range that already exists returns `'already exists: ...'` without error.
3. Verify the metric drops back to a safe value on the next poll cycle
   (within the indexer's configured poll interval after the partition is
   created).

**Why it fires as a warning, not a page:** there is months of runway at the
5M threshold. Create a ticket, action it during business hours, and monitor
the gauge. Escalate to `TridentPartitionExhausted` (critical) if the lookahead
continues to fall without action.

## TridentPartitionExhausted

**Means:** `trident_indexer_partition_lookahead_ledgers` is at or below 0 —
the ingest cursor has reached or passed the upper bound of the last named
`soroban_events` partition. The indexer will refuse to commit any further pages
and will halt with a `Fatal` error on the next poll cycle that produces events.

**Why this threshold:** a `<= 0` lookahead means the very next INSERT would
land in `soroban_events_default` — the unindexed, unmanaged DEFAULT catch-all
partition. Silently writing there would make data invisible to API queries and
unrecoverable by normal partition retention tools. The indexer halts rather
than corrupt the event stream (see `assert_no_default_partition_overflow` in
`crates/indexer/src/db/mod.rs`).

**This is a total ingest outage.** Treat as SEV-1. Resolve before resuming.

**First steps:**
1. **Add the next partition immediately.** Get the exact boundary value:
   ```sql
   SELECT MAX(
       (regexp_match(
           pg_get_partition_constraintdef(child.oid),
           'ledger_sequence < (\d+)'
       ))[1]::bigint
   ) AS last_upper
   FROM pg_inherits
   JOIN pg_class parent ON parent.oid = inhparent
   JOIN pg_class child  ON child.oid  = inhrelid
   WHERE parent.relname = 'soroban_events';
   ```
   Then create the next two partitions (add a buffer, not just one):
   ```sql
   SELECT create_soroban_partition(<last_upper>, <last_upper + 2000000>);
   SELECT create_soroban_partition(<last_upper + 2000000>, <last_upper + 4000000>);
   ```
2. **Verify the partition was created:**
   ```sql
   SELECT relname FROM pg_class
   WHERE relname LIKE 'soroban_events_p%'
   ORDER BY relname DESC LIMIT 5;
   ```
3. **Restart the indexer** (it halted with a Fatal error; it will not recover
   on its own). The cursor is persisted in `system_state` so restart resumes
   from exactly where it stopped — no data loss, no re-index required.
4. **Confirm** `trident_indexer_partition_lookahead_ledgers` is positive and
   rising after the restart.
5. Check whether any events landed in `soroban_events_default` during any
   window when the guard was not in effect (pre-#525). If rows exist there,
   they must be migrated into the correct named partition:
   ```sql
   -- Inspect what is in the default partition
   SELECT MIN(ledger_sequence), MAX(ledger_sequence), COUNT(*)
   FROM soroban_events_default;
   -- If rows exist and the correct named partition now covers the range,
   -- move them:
   INSERT INTO soroban_events
   SELECT * FROM soroban_events_default
   ON CONFLICT DO NOTHING;
   DELETE FROM soroban_events_default;
   ```

---

## Observability RPC alerts (observability/rpc-alerts.yml)

The following alerts monitor Stellar RPC provider health — latency, error
rate, and failover state — so ops can see "RPC is degraded" before it turns
into ingest lag.

## TridentRPCHighErrorRate

**Means:** over 10% of Stellar RPC calls have failed over the last 5 minutes,
sustained for 5 minutes.

**Why this threshold:** 10% sustained error rate indicates the upstream RPC
node is degraded, rate-limiting, or unreachable — not just isolated
transient failures. This is a leading indicator that will turn into ingest
lag if not addressed.

**First steps:**
1. Check `trident_indexer_rpc_errors_total` and break down by `error_type`
   to distinguish rate-limited vs timing-out vs bad request shape.
2. Check `trident_indexer_rpc_active_endpoint` to see if failover has
   already kicked in to a secondary RPC provider.
3. Check the RPC provider's status page or try a manual health check against
   `STELLAR_RPC_URL`.

**Known causes:**
- RPC provider under load or rate-limiting
- Network partition between indexer and RPC endpoint
- Invalid cursor/pagination state (check for `invalid_cursor` error_type)

**Mitigation:** configure a fallback RPC endpoint if one exists; consider
raising rate limits with the provider.

**Escalation:** if sustained for >15 minutes and no fallback is available,
escalate to the RPC provider or switch endpoints.

## TridentRPCHighLatency

**Means:** p95 latency for a specific RPC method (e.g., `getEvents`) has
exceeded 5 seconds, sustained for 10 minutes.

**Why this threshold:** 5s p95 is a degraded-but-still-responding provider,
distinct from outright timeouts. Left unaddressed, high latency typically
turns into ingest lag as the indexer's poll loop spends most of its time
waiting on slow RPC responses.

**First steps:**
1. Check which method is slow: break down
   `trident_indexer_rpc_call_duration_seconds` by `method` label.
2. Check if this correlates with elevated error rate
   (`TridentRPCHighErrorRate`) — often both fire together when the provider
   is overloaded.
3. Check `trident_indexer_rpc_timeouts_total` — are requests timing out
   entirely, or just responding slowly?

**Known causes:**
- RPC provider under load
- Large response payloads (many events per ledger)
- Network congestion between indexer and RPC endpoint

**Mitigation:** if a secondary RPC endpoint is available, consider manual
failover or allowing the automatic failover logic to switch.

**Escalation:** if sustained for >30 minutes, escalate to the RPC provider
or investigate network path.

## TridentRPCFailoverActive

**Means:** `trident_indexer_rpc_active_endpoint` has been non-zero (not the
primary) for at least 5 minutes — the indexer is running on a fallback RPC
endpoint.

**Why this threshold:** failover is working as designed to keep the indexer
running when the primary is down. This alert is informational ("you're on
backup power") rather than urgent, but should be investigated before the
backup fails too.

**First steps:**
1. Check `trident_indexer_rpc_failovers_total` to see how often failover has
   occurred — frequent flapping suggests both endpoints are unstable.
2. Check whether the primary RPC endpoint has recovered — try a manual health
   check or `getHealth` call.
3. Check `trident_indexer_rpc_errors_total` for the primary endpoint to see
   why failover triggered.

**Known causes:**
- Primary RPC provider outage or maintenance window
- Primary endpoint rate-limiting or rejecting requests
- Network partition to primary endpoint

**Mitigation:** if the primary has recovered, the indexer will automatically
fail back on the next poll cycle (no manual intervention needed). If the
primary is still down, ensure the fallback endpoint has sufficient capacity
for sustained traffic.

**Escalation:** if both primary and fallback are degraded, page on-call to
add a third endpoint or escalate to RPC provider(s).

## TridentRPCRateLimited

**Means:** `trident_indexer_rpc_errors_total{error_type="rate_limited"}` has
been climbing for 5+ minutes — the Stellar RPC provider is actively
rate-limiting the indexer.

**Why this threshold:** sustained rate-limiting degrades ingest freshness the
same way an outage does, but is a distinct root cause (quota exhausted rather
than provider down) that requires a different mitigation (raise quota vs fail
over).

**First steps:**
1. Check the indexer's configured poll interval (`POLL_INTERVAL_MS`) — if
   it's very aggressive (e.g., <1s), consider backing off slightly.
2. Check `trident_indexer_rpc_call_duration_seconds_count` to estimate
   request rate — are we exceeding the provider's documented limits?
3. Check the RPC provider's dashboard/billing page to see current quota usage
   and limits.

**Known causes:**
- Indexer poll rate exceeds RPC provider's quota
- Other consumers sharing the same RPC quota
- Provider has reduced quota limits (check provider changelog/announcements)

**Mitigation:** raise the provider's quota if possible; add a secondary RPC
endpoint to the pool to distribute load; back off poll interval slightly if
latency tolerance allows.

**Escalation:** if quota cannot be raised and no secondary endpoint is
available, escalate to product/eng to prioritize RPC provider migration or
multi-provider setup.

---

## SLO burn-rate alerts (observability/burn-rate-alerts.yml)

The following alerts implement multi-window, multi-burn-rate monitoring for
the SLOs defined in docs/slo.md (issue #296). They follow the Google SRE
workbook pattern: a short window confirms the burn is happening *now*, a long
window confirms it's sustained (not a blip) — both must breach before paging.

## IngestFreshnessFastBurn

**Means:** ledger lag has exceeded the 30s target (docs/slo.md SLO 1) for a
large share of both the last 5 minutes and the last 1 hour, consuming the
28-day error budget at 14.4x — exhausts the whole monthly budget in ~2 days
if sustained.

**Why this threshold:** fast burn (14.4x) is high enough to be page-worthy
immediately — it's not a transient blip if both the 5m and 1h windows agree
— but not so high that it triggers on every momentary spike.

**First steps:**
1. Check `trident_indexer_ledger_lag` current value — how far behind is the
   indexer right now?
2. Check `trident_indexer_rpc_retries_total` and
   `trident_indexer_rpc_failovers_total` — is the RPC layer struggling?
3. Check `trident_indexer_last_poll_timestamp_seconds` — is the poll loop
   stalled entirely, or just slow?

**Known causes:**
- RPC provider degradation (see `TridentRPCHighErrorRate`,
  `TridentRPCHighLatency`)
- Database write path bottleneck (check `trident_indexer_db_pool_size` and
  Postgres slow query log)
- Indexer restart/deploy during high ledger activity

**Mitigation:** if RPC is the bottleneck, fail over to a secondary endpoint
or back off poll interval slightly; if DB is the bottleneck, scale the DB or
increase the indexer's connection pool.

**Escalation:** page on-call immediately — fast burn exhausts the monthly
budget in under 2 days.

## IngestFreshnessSlowBurn

**Means:** ledger lag has exceeded 30s for a sustained share of both the last
30 minutes and the last 6 hours, consuming the error budget at 6x — exhausts
the budget in ~5 days if sustained.

**Why this threshold:** slow burn (6x) is not urgent enough to page
immediately, but indicates a sustained problem that needs investigation before
it becomes a fast burn. The longer windows (30m/6h) filter out transient
issues the fast-burn rule would already catch.

**First steps:**
1. Check the same diagnostic metrics as `IngestFreshnessFastBurn` but with
   lower urgency — this is a leading indicator, not an active outage.
2. Check whether this correlates with any recent deploys, config changes, or
   upstream Stellar protocol upgrades.
3. Review `trident_indexer_rpc_errors_total` and
   `trident_indexer_parse_errors_total` for elevated rates.

**Known causes:**
- Slightly degraded RPC latency not yet crossing the `TridentRPCHighLatency`
  threshold
- Gradual increase in ledger activity (more events per ledger) without a
  corresponding indexer capacity increase
- Small config regression (e.g., poll interval accidentally increased)

**Mitigation:** address the root cause before it becomes a fast burn —
optimize indexer throughput, scale DB, or add RPC capacity.

**Escalation:** create a ticket rather than paging — investigate during
business hours before it escalates to fast burn.

## IndexerHeartbeatStalled

**Means:** `trident_indexer_last_poll_timestamp_seconds` has not advanced in
over 2 minutes — the indexer poll loop has not completed a cycle.

**Why this threshold:** this is a dead-man's-switch for the SLO: if the
indexer dies outright or the metric stops being scraped, the lag-ratio alerts
above can miss it (flat line looks healthy). 2 minutes is well above any
reasonable poll interval, so staleness means the indexer is hung, crashed, or
stuck retrying RPC.

**First steps:**
1. Check indexer process status (`kubectl get pods` or `docker compose ps`) —
   is it running, restarting, or crashed?
2. Check `trident_indexer_rpc_errors_total` — is it stuck retrying RPC
   failures?
3. If the process is alive but stalled, capture a stack dump/profile before
   restarting.

**Known causes:**
- Indexer process crashed or killed (OOM, segfault, panic)
- Poll loop deadlocked or blocked on I/O
- RPC provider completely unreachable (not just slow or erroring, but
  connection refused / timeout on every request)

**Mitigation:** restart the indexer — the cursor is persisted in
`system_state`, so restart is safe.

**Escalation:** page on-call immediately — a stalled indexer violates the
ingest-freshness SLO directly.

## TridentIngestLagSustainedHigh

**Means:** `trident_indexer_ledger_lag_seconds_estimated` (lag expressed in
estimated wall-clock seconds, assuming ~5s per ledger) has been above 500s
(~100 ledgers) for 10 minutes.

**Why this threshold:** this is a direct, human-readable threshold alert
independent of the error-budget/burn-rate math above. 100 ledgers (~8 minutes
of lag) sustained for 10 minutes is well past "transient slowdown" and into
"the indexer is falling behind." Mirrors the fields exposed by
`GET /v1/stats/indexer` (docs/observability/data-freshness.md).

**First steps:**
1. Check `trident_indexer_ledger_lag` (the raw ledger-count lag) and
   `trident_indexer_rpc_active_endpoint` to see if RPC failover has occurred.
2. Check `trident_indexer_rpc_errors_total` — is the RPC provider the cause?
3. Same diagnostic steps as `IngestFreshnessFastBurn` — this is an alternate
   view of the same underlying problem.

**Known causes:** same as `IngestFreshnessFastBurn` (RPC degradation, DB
bottleneck, indexer restart during high activity).

**Mitigation:** same as `IngestFreshnessFastBurn`.

**Escalation:** page on-call — this crosses the "API consumers are reading
meaningfully stale data" threshold.

## TridentDiskFillingWithin14Days

**Means:** extrapolating the last 6 hours of growth, the Postgres data volume
runs out of space within 14 days.

**Why this threshold:** it is a provisioning signal, not an incident. Disk
growth is measured, not guessed — 890 bytes per event including indexes, over
500k rows on the full migration chain
(docs/performance.md#storage-capacity-and-disk-growth). At the 10x testnet
rate that is ~17 GiB/month, so a volume can go from comfortable to full inside
a quarter. 14 days is chosen to leave room to provision, migrate, and verify
rather than to react.

**First steps:**
1. Confirm the trend is real and not a one-off:
   `node_filesystem_avail_bytes{mountpoint="/var/lib/postgresql"}` over 7d.
2. Check what is actually growing — `soroban_events` is the expected answer:
   ```sql
   SELECT relname, pg_size_pretty(pg_total_relation_size(c.oid))
     FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public'
    ORDER BY pg_total_relation_size(c.oid) DESC LIMIT 10;
   ```
3. Decide between resizing the volume and enabling partition retention. Because
   `soroban_events` is RANGE-partitioned by `ledger_sequence` (migration 0017),
   dropping the oldest partition is a fast metadata operation, not a bulk
   DELETE:
   ```sql
   DROP TABLE soroban_events_p0_1999999;
   ```
   Confirm the retention policy before dropping — those events are gone.

## TridentDiskFillingWithin48Hours

**Means:** the same projection, now inside 48 hours.

**Why this threshold:** at this point provisioning lead time is mostly gone, so
this pages rather than warns. A full volume does not degrade gracefully — the
indexer stops committing and the API fails writes.

**First steps:**
1. Resize the volume now if the platform supports online resize. This is the
   only action that does not lose data.
2. If a resize is not immediately available, drop the oldest
   `soroban_events` partition (see the query above) to buy time.
3. Check for a non-obvious consumer before assuming it is event growth: an
   unrotated WAL (`SELECT pg_size_pretty(sum(size)) FROM pg_ls_waldir();`) or a
   stalled replication slot holds space that no partition drop will release.

## TridentDiskSpaceLow

**Means:** less than 15% of the Postgres data volume remains, regardless of
trend.

**Why this threshold:** the two predictive alerts above extrapolate a 6-hour
trend, which cannot see a step change — a large backfill, a WAL pileup behind a
stalled replication slot, or a runaway temp file. This is the backstop for
those, so it fires on the level rather than the slope.

**First steps:**
1. Identify the consumer: `pg_ls_waldir()` for WAL,
   `pg_stat_replication` / `pg_replication_slots` for a stalled slot, and the
   table-size query above for ordinary growth.
2. A stalled replication slot is the most common non-obvious cause — an
   inactive slot pins WAL indefinitely. Drop it if the replica is genuinely
   gone: `SELECT pg_drop_replication_slot('<name>');`
3. If it is ordinary growth, treat it as
   `TridentDiskFillingWithin48Hours` above.

## TridentIndexerPersistDeadLetterBacklog

**Means:** `failed_events` has pending rows — at least one event decoded
fine but its database commit kept failing through the whole-page retry, the
per-event isolation retry, and its backoff budget, so the streamer captured
the failing event (full payload + error message), counted it on
`trident_indexer_persist_dead_lettered_total`, and advanced the cursor past
it (issues #208/#508). The data is safe but missing from `soroban_events`
until replayed.

**Why this threshold:** any pending row at all means indexed data is
incomplete, and rows leave the pending state only through the replay
procedure below — so the alert stays up until the gap is actually closed. A
silent DLQ is the same as data loss. `for: 5m` only absorbs scrape jitter.
Pending rows are unique per event (migration 0030): the gauge counts
distinct poisoned events, not retry bursts.

**First steps:**

1. Inspect the queue:

   ```sql
   SELECT contract_id, ledger_sequence, error_message, attempts, occurred_at
   FROM failed_events WHERE replayed_at IS NULL ORDER BY occurred_at;
   ```

   `error_message` names the exact commit error, updated to the most recent
   failure; `attempts` accumulates across redeliveries.
2. Fix the underlying cause (a malformed field failing column conversion, a
   constraint interaction, …). The failure survived the whole retry budget,
   so replaying before the fix will just fail again.
3. **Replay** once the fix is deployed: re-ingest the affected range with
   the backfill CLI (idempotent — `ON CONFLICT DO NOTHING` absorbs the
   events that did commit). Note its scope: backfill restores rows in
   `soroban_events` only — it does not write outbox rows or token
   projections, so replayed events are not delivered to Redis/webhook
   subscribers and do not appear in `token_events`. Acceptable for
   historical repair, but know what you are and are not restoring:

   ```
   trident-backfill --from-ledger <min> --to-ledger <max> [--contract <id>]
   ```

   using `SELECT MIN(ledger_sequence), MAX(ledger_sequence) FROM
   failed_events WHERE replayed_at IS NULL;` for the range.
4. Verify the events landed, then mark the rows replayed — this is what
   resolves the alert (rows are kept as history, never deleted):

   ```sql
   UPDATE failed_events d
   SET replayed_at = NOW()
   FROM soroban_events e
   WHERE d.replayed_at IS NULL
     AND e.ledger_sequence = d.ledger_sequence
     AND e.transaction_hash = d.transaction_hash
     AND e.event_index = d.event_index;
   ```

   The gauge refreshes on the next active poll cycle; the alert clears once
   no pending rows remain.

---

## TridentOutboxBacklogWarning

**Means:** `trident_indexer_outbox_backlog` has been at or above 10,000 rows
for 10 minutes. The outbox is the sole delivery guarantee for live
subscribers (`crates/indexer/src/db/outbox.rs`); a growing backlog means the
relay is publishing to Redis slower than events are landing.

**Why this threshold:** 10,000 is the relay's own
`OUTBOX_BACKLOG_ALERT_THRESHOLD` default (`crates/indexer/src/config.rs`),
already used to emit a `tracing::warn!` log line — this alert simply pages on
the same signal instead of leaving it as a log line nothing watches. 10
minutes filters a brief publish stall that clears on its own.

**First steps:**
1. Check `trident_indexer_outbox_published_total`'s rate — is it still
   advancing (a slow relay) or flat (a stopped one)?
2. Check Redis connectivity/health from the indexer's perspective; a Redis
   outage is the most common cause.
3. Check `trident_indexer_outbox_publish_failures_total` for a climbing
   failure count alongside the backlog (see
   `TridentOutboxPublishFailuresHigh` below).

---

## TridentOutboxBacklogCritical

**Means:** `trident_indexer_outbox_backlog` has been at or above 50,000 for
5 minutes — five times the warning threshold.

**Why this threshold:** at this level the relay is not merely slow, it has
very likely stopped entirely (Redis down, or the relay loop crashed/hung).
Live subscribers have stopped receiving events; this is a page, not a
ticket.

**First steps:**
1. Confirm the relay process is actually running and not crash-looping.
2. Confirm Redis is reachable from the indexer.
3. If both check out, `trident_indexer_outbox_publish_failures_total`'s
   error detail (indexer logs) should show the specific publish failure
   mode.
4. Once the underlying cause is fixed, the relay drains the accumulated
   backlog automatically — no manual replay step, since the events remain
   in `soroban_events`/the outbox table the whole time.

---

## TridentOutboxPublishFailuresHigh

**Means:** `trident_indexer_outbox_publish_failures_total` has been
increasing for 10 minutes.

**Why this threshold:** every failed publish leaves its row in the outbox
for the next attempt, directly feeding `trident_indexer_outbox_backlog` —
this alert exists to fire *before* the backlog itself crosses a threshold,
giving earlier warning than `TridentOutboxBacklogWarning` alone.

**First steps:**
1. Check the indexer logs for the specific publish failure (Redis
   connection error, XADD rejection, serialization failure).
2. If failures are climbing but the backlog is still low, the relay may be
   compensating via retry — check whether `trident_indexer_outbox_backlog`
   is still flat before treating this as urgent.
3. See `TridentOutboxBacklogWarning`/`TridentOutboxBacklogCritical` above
   for what to do once the backlog itself starts climbing.

---

## TridentDeadLetteredEventsHigh

**Means:** `trident_indexer_dead_lettered_total` increased in the last
hour. This counts events that were undecodable (a poison message where
retry never helps, issue #414) and were written to `parse_errors` so the
poll could advance past them — distinct from
`trident_indexer_persist_dead_lettered_total` (well-formed events whose
database *commit* failed, see `TridentIndexerPersistDeadLetterBacklog`
above).

**Why this threshold:** `TridentIndexerParseErrorRateHigh` already alerts
on the *rate* of all parse failures, including ones that later succeed on
retry. This counter only moves when an event is actually abandoned — a
single occurrence is worth knowing about even if the overall parse-error
rate stays well under that alert's 1% threshold.

**First steps:**
1. Query `parse_errors` for the newly dead-lettered rows and inspect
   `raw_payload`/`error_message`.
2. Check whether this coincides with an RPC/XDR schema change (same first
   step as `TridentIndexerParseErrorRateHigh`).
3. Dead-lettered events are not replayed automatically — the poison
   message reasoning is that retry never helps for these specifically, so
   fixing the parser is the only path back, followed by a manual backfill
   of the affected ledger range if the events are needed.

---

## TridentIndexerDBPoolSaturated

**Means:** over 90% of the indexer's Postgres connection pool has been
checked out for 10 minutes.

**Why this threshold:** identical reasoning to `TridentAPIDBPoolSaturated`
above — 90% is a leading indicator before the pool is actually exhausted,
and 10 minutes filters brief bursts. Before this alert, only the API's own
pool was watched (`trident_api_db_pool_acquired_connections` /
`trident_api_db_pool_max_connections`); the indexer's pool
(`trident_indexer_db_pool_size` / `trident_indexer_db_pool_idle_connections`)
had the gauges emitted but nothing alerting on them.

**First steps:**
1. Check for a slow or stuck query holding indexer connections open
   (`pg_stat_activity`).
2. Check whether this coincides with a backfill or reconciliation pass
   running concurrently with normal ingest — both draw from the same pool.
3. The indexer's pool size is configured separately from the API's; raising
   it is a stopgap if the root cause is a query regression rather than
   organic load growth.

## Container resource alerts (monitoring/alerts.yml, `trident.container.resources`)

The three sections below (issue #444) were added to `monitoring/alerts.yml`
with `runbook_url` already pointing at these anchors, but the sections
themselves were never written — `scripts/check-runbook-urls.sh` (issue #618)
now catches that class of drift going forward.

## TridentContainerOOMKilled

**Means:** a container (`api`, `grpc-api`, or `indexer`) was killed by the
kernel OOM killer within the last 10 minutes
(`kube_pod_container_status_terminated_reason{reason="OOMKilled"}`).

**Why this threshold:** `for: 0m` — this fires immediately rather than after
a sustained window, because an OOM kill is already a completed event by the
time the metric exists; waiting to confirm it "persists" makes no sense for
something that already happened once.

**First steps:**
1. Identify which container and pod from `{{ $labels.container }}` /
   `{{ $labels.pod }}` in the alert.
2. Check `container_memory_working_set_bytes` for that container leading up
   to the kill, and correlate with `TridentContainerMemoryHigh` firing
   beforehand if it did.
3. Check whether the container's Go/Rust runtime has a memory limit
   configured (`GOMEMLIMIT` for Go services) consistent with the pod's
   `container_spec_memory_limit_bytes` — a runtime unaware of the container
   limit will keep allocating past it instead of triggering its own GC
   pressure response first.
4. If this coincides with a traffic spike or backfill, treat it as a
   capacity question (raise the limit) rather than a leak; if it recurs
   under steady load, treat it as a leak and profile heap usage.

**Escalation:** `severity: critical` — the container was already killed and
restarted, which is itself a brief availability gap for whatever it served.

## TridentContainerMemoryHigh

**Means:** `container_memory_working_set_bytes` for `api`, `grpc-api`, or
`indexer` has exceeded 85% of `container_spec_memory_limit_bytes` for 5
minutes.

**Why this threshold:** 85% sustained for 5 minutes is a leading indicator
ahead of `TridentContainerOOMKilled` — the goal is to page on the trend
before the kernel has to intervene, not only after.

**First steps:**
1. Check `{{ $labels.container }}` in `{{ $labels.pod }}` against recent
   deploys or traffic changes.
2. If this is trending up gradually rather than holding steady, treat it as
   a candidate leak and prioritize investigation before it reaches
   `TridentContainerOOMKilled`.
3. If it's a step change correlated with load, raising the memory limit is a
   reasonable stopgap while the root cause (or a horizontal-scaling fix) is
   worked.

**Escalation:** `severity: warning` — ticket, not page; this is the
early-warning alert for the critical one above.

## TridentContainerCPUThrottling

**Means:** more than 25% of CPU periods were throttled under the container's
CFS quota for `api`, `grpc-api`, or `indexer`, sustained for 10 minutes
(`container_cpu_cfs_throttled_periods_total` /
`container_cpu_cfs_periods_total`).

**Why this threshold:** CFS throttling at this level means the container is
routinely hitting its CPU quota and being paused mid-timeslice, which shows
up as latency and jitter even though the process never crashes or restarts —
distinct from and often less visible than the memory alerts above.

**First steps:**
1. Check `{{ $labels.container }}` in `{{ $labels.pod }}` against
   `TridentAPIHTTP5xxRateHigh` / `TridentRPCHighLatency` for whether the
   throttling is visibly degrading response times.
2. Check whether this correlates with a request-rate increase (organic
   growth, worth raising CPU limits or scaling out replicas) or a regression
   in a specific code path (worth profiling before just raising the limit).
3. Raising the CPU limit is the direct mitigation; scaling out replicas is
   the alternative when the workload parallelizes well across pods.

**Escalation:** `severity: warning` — degraded latency, not an outage.

## TridentRPCHealthScoreLow

**Means:** `trident_rpc_health_score` for an RPC endpoint has been below 50
(the midpoint of its 0-100 scale, `crates/indexer/src/rpc/health.rs`) for 10
minutes.

**Why this threshold:** the health scorer penalizes an endpoint faster than
it recovers once it starts erroring or slowing down, ahead of the connection
pool actually failing over to another endpoint — this is the leading
indicator, `TridentRPCHealthScoreCritical` below is the trailing one.

**First steps:**
1. Check `trident_indexer_rpc_errors_total` and
   `trident_indexer_rpc_call_duration_seconds` for `{{ $labels.endpoint }}`
   to see which is driving the penalty.
2. Compare against `TridentRPCHighErrorRate` / `TridentRPCHighLatency` above
   — this may already be firing alongside one of them for the same
   endpoint.
3. If only one endpoint is configured, there's no failover target; treat
   this as an early warning to add a secondary endpoint (issue #213) rather
   than waiting for `TridentRPCHealthScoreCritical`.

**Escalation:** `severity: ticket` — not urgent enough to page on its own.

## TridentRPCHealthScoreCritical

**Means:** `trident_rpc_health_score` for an RPC endpoint has been below 20
for 5 minutes — near the scorer's floor of 0.

**Why this threshold:** 20/100 sustained for 5 minutes means the scorer
considers this endpoint close to unusable, not merely degraded.

**First steps:**
1. Check `trident_indexer_rpc_active_endpoint` for `{{ $labels.endpoint }}`
   to confirm whether failover to another configured endpoint has already
   occurred.
2. If this is the only configured endpoint, ingest is likely already
   degraded or stalled — treat as the same incident as
   `IngestFreshnessFastBurn` / `IndexerHeartbeatStalled` if either of those
   is also firing.
3. Same diagnostic steps as `TridentRPCHealthScoreLow` above for isolating
   the cause on this endpoint.

**Escalation:** `severity: page` — this endpoint is near-unusable by the
scorer's own floor.
