# Runbook: rotating the mainnet RPC provider without indexer downtime

**Status: documented procedure, not yet walked through against a real
mainnet indexer.** Issue #684's "done when" is a tested procedure; what
follows is grounded in reading the indexer's actual failover, cursor, and
reorg-handling code (cited throughout), not generic RPC-rotation advice. A
real walkthrough against staging, with someone other than the author driving
from this document alone, is still owed — see "What this runbook still
needs" at the end.

Related: [`../deployment.md`](../deployment.md#indexer-rpc-transport-and-failover),
[`../ENVIRONMENT.md`](../ENVIRONMENT.md), [`mainnet-event-volume-spike.md`](mainnet-event-volume-spike.md)
(RPC-saturation detection table), [`../metrics-catalog.md`](../metrics-catalog.md).

## Why this needs a runbook at all

A naive RPC URL swap can silently corrupt indexed data rather than just fail
loudly, because the indexer's reorg-detection logic cannot tell "the chain
actually reorged" from "the new provider disagrees with the old one." Three
real hazards, all confirmed by reading the code paths below, not assumed:

1. **The new provider's tip is behind the stored cursor.** `check_and_handle_reorg`
   (`crates/indexer/src/streamer/mod.rs`) treats `latest_ledger < cursor` as a
   reorg. If the apparent depth is within `MAX_REORG_DEPTH` (default 128) it
   **deletes** rows at or above that ledger from `soroban_events`,
   `token_events`, `contract_invocation_metrics`, and
   `contract_storage_snapshots`, then rewinds the cursor
   (`db::handle_reorg_rollback`). Past that depth it halts with a fatal
   error instead. A lagging new provider is indistinguishable from a real
   reorg to this code.
2. **Wrong network, or a provider whose ledger hashes disagree with the
   current one for reasons other than a real reorg.** The same hash
   comparison against `ledger_metadata` triggers the identical rollback or
   halt.
3. **Shorter history retention than the outgoing provider.** If the stored
   cursor is older than the new provider's oldest retained ledger,
   `recover_retained_floor` moves the cursor forward to what the new
   provider can actually serve, enqueues a `backfill_jobs` row for the gap,
   and emits a skipped-ledgers metric — but live indexing does not fill that
   gap on its own; a backfill run is needed afterward (see
   [`gap-detection-backfill-drill.md`](gap-detection-backfill-drill.md)).

All three failure modes are avoidable with the pre-checks below, run against
the *new* endpoint *before* it is put into service.

## What actually happens when you change the config

**A full process restart is required.** The indexer handles only SIGTERM and
SIGINT, both of which trigger the same graceful shutdown
(`crates/indexer/src/main.rs`): there is no SIGHUP or live-reload path, so
`STELLAR_RPC_URLS` can only take effect by restarting the process. Shutdown
finishes whatever page is already mid-poll (it never stops mid-batch), logs
"Streamer stopped cleanly; cursor persisted," and the outbox relay then gets
`SHUTDOWN_GRACE_SECS` (default 30) to drain. Because the cursor and the
events for a page are committed together in one transaction, a restart loses
no committed data regardless of exactly when it lands.

On start, the cursor is read back from `system_state` and logged as
"Resuming from ledger cursor" — the new provider is handed exactly that
cursor, which is why the pre-checks below matter: nothing on the startup
path separately re-validates that the new endpoint can actually serve from
there before the normal poll loop (and its reorg logic) takes over.

**The indexer has no horizontal replicas to roll one at a time against**: the
Helm chart pins it to `strategy: Recreate` and `replicaCount: 1`
(`helm/trident/templates/indexer-deployment.yaml`) — see
["Indexer replica count — single-writer by design"](../deployment.md) for
why. Indexing briefly and visibly stops for the duration of the restart;
this is expected, not a bug to work around mid-rotation.

**Current failover behaviour, so you know what you are actually
configuring**: `RpcClient::with_endpoints` (`crates/indexer/src/rpc/mod.rs`)
scores every configured endpoint in memory (0-100, via
`crates/indexer/src/rpc/health.rs`) and sends calls to the highest-scoring
endpoint, with a sticky selection (a challenger must lead by a hysteresis
margin) and a per-endpoint circuit breaker. An endpoint that fails
`RPC_BREAKER_FAILURE_THRESHOLD` consecutive times is parked for
`RPC_BREAKER_COOLDOWN_MS` and then probed again. Scores reset to 100 on every
restart. `RPC_FAILOVER_THRESHOLD` and `RPC_ENDPOINT_COOLDOWN_MS` were removed
(issue #737) because nothing ever read them — setting either now fails
startup, so plan a rotation around the `RPC_BREAKER_*` pair instead. What a multi-URL
`STELLAR_RPC_URLS` list actually buys you today is per-call best-of-N
routing by live health score, not threshold-and-cooldown failover.

## Preconditions

- [ ] The new endpoint's `getNetwork` passphrase matches the target network
      exactly (`Public Global Stellar Network ; September 2015` for
      mainnet), using the same check already documented in
      [`../deployment.md`](../deployment.md#stellar-rpc-url):
      ```bash
      curl -s -X POST "$NEW_RPC_URL" -H 'Content-Type: application/json' \
        -d '{"jsonrpc":"2.0","id":1,"method":"getNetwork"}'
      ```
- [ ] The new endpoint's current tip is at or ahead of the indexer's stored
      cursor:
      ```sql
      SELECT value FROM system_state WHERE key = 'latest_ledger_cursor:mainnet';
      ```
      ```bash
      curl -s -X POST "$NEW_RPC_URL" -H 'Content-Type: application/json' \
        -d '{"jsonrpc":"2.0","id":1,"method":"getLatestLedger"}'
      ```
      If the new endpoint's `sequence` is **behind** the stored cursor, do
      not switch yet — this is exactly the condition `check_and_handle_reorg`
      cannot distinguish from a real reorg (hazard 1 above).
- [ ] The new endpoint's oldest retained ledger is at or before the stored
      cursor:
      ```bash
      curl -s -X POST "$NEW_RPC_URL" -H 'Content-Type: application/json' \
        -d '{"jsonrpc":"2.0","id":1,"method":"getHealth"}'
      ```
      Compare the response's `oldestLedger` against the cursor from the
      previous step. If `oldestLedger` is *greater* than the cursor, the new
      provider's retention window does not reach back far enough — expect
      `recover_retained_floor` to move the cursor forward and leave a gap for
      a manual backfill (hazard 3 above); decide whether that is acceptable
      before switching, not after.
- [ ] `RPC_MAX_CALLS_PER_SEC` and the provider's own published rate limit are
      compatible — a tighter unannounced provider limit shows up as the
      saturation signature in
      [`mainnet-event-volume-spike.md`](mainnet-event-volume-spike.md)'s
      detection table, not as an obvious hard failure.
- [ ] A rollback endpoint (the current, working RPC URL) is on hand and
      proven reachable, in case the new provider misbehaves after cutover.

## Procedure

### 1. Validate the new endpoint in isolation — *operator*

Run every precondition check above against the new URL alone, without
touching the running indexer's configuration yet. Treat any failed
precondition as a stop, not a warning.

### 2. Update the endpoint list — *operator*

Decide the shape of the change:

- **Add as a second endpoint** (recommended for a first rotation): set
  `STELLAR_RPC_URLS` to a comma-separated list with the new endpoint added
  alongside the current one, current endpoint first. This lets the health
  scorer route calls to whichever actually responds best, rather than
  betting the whole rotation on the new endpoint from the first call.
- **Replace outright**: set `STELLAR_RPC_URLS` (or the single-value
  `STELLAR_RPC_URL` alias) to only the new endpoint.

Apply the change through your deployment target's own secret/config path
(Helm `values.yaml`'s `indexer.env`, or `fly secrets set` — see
[`../deployment.md`](../deployment.md) for the deploy target this
environment actually uses). **Caution**: the Helm chart only wires
`STELLAR_RPC_URL` (singular) from the `ExternalSecret`-backed source; a
multi-URL `STELLAR_RPC_URLS` only reaches the pod through `indexer.env`,
which is plaintext in `values.yaml` — do not put a token-bearing URL there
without first checking whether your deployment already has a secret path
for it.

### 3. Restart the indexer — *operator*

A full restart is required (see "What actually happens" above). Use your
normal deploy mechanism; there is no in-place reload to trigger instead.

### 4. Verify recovery — *operator*

Prometheus metrics are the real signal here; `/readyz` only checks Postgres
and Redis and never queries RPC, so a green `/readyz` does not confirm the
new endpoint is actually working.

- [ ] `trident_indexer_last_poll_timestamp_seconds` is advancing (the
      indexer's heartbeat).
- [ ] `trident_indexer_ledger_lag` is not climbing.
- [ ] `trident_rpc_health_score{endpoint="<new URL>"}` is present and
      recovering toward 100 (starts at 100 on restart, so watch that it
      does not immediately start dropping from repeated failures instead).
      **Caution**: this label carries the full endpoint URL, which can
      include a provider token — treat Prometheus/Grafana access
      accordingly if your `STELLAR_RPC_URLS` entries are not bare hostnames.
- [ ] `trident_indexer_rpc_breaker_state` stays `0` (closed) and
      `trident_indexer_rpc_consecutive_failures` stays low.
- [ ] The startup log line "RPC endpoint pool configured with health
      scoring" shows the expected endpoint list.
- [ ] No `check_and_handle_reorg`/rollback log lines appear immediately
      after restart. If one does, treat it as real: stop and read
      [`incident-response.md`](incident-response.md) rather than assuming
      it is a rotation artifact, since the code cannot tell the difference
      itself (hazard 1/2 above) — which is precisely why step 1's
      pre-checks exist.
- [ ] If retention was a concern in the preconditions, confirm whether
      `recover_retained_floor` fired by checking for a new
      `backfill_jobs` row, and if so run the backfill per
      [`gap-detection-backfill-drill.md`](gap-detection-backfill-drill.md).

### 5. Remove the old endpoint (if it was kept as a second entry) — *operator*

Once step 4's checks have held for a reasonable observation window (at
least one full poll cycle under normal load, longer if the old endpoint is
being decommissioned on the provider's side), repeat steps 2-4 to drop the
old URL from the list, if it is no longer wanted.

## Rollback

If step 4 shows a problem, the fastest rollback is config, not code: set
`STELLAR_RPC_URLS`/`STELLAR_RPC_URL` back to the known-good endpoint from the
precondition checklist and restart again. If a reorg rollback already fired
against bad data from a lagging new endpoint, the cursor has already been
rewound and the affected rows already deleted by `check_and_handle_reorg`
itself — reverting the RPC config does not undo that. Restarting against the
old, correct endpoint lets the indexer re-fetch and re-insert the
rolled-back range on its own; this is the same self-healing path a genuine
reorg relies on, so no separate manual replay is needed, only verification
afterward that the gap closed.

## What this runbook still needs (not done here)

- [ ] A real walkthrough against staging by someone who did not write this
      document — issue #684's own "done when" criterion.
- [x] `RPC_FAILOVER_THRESHOLD`/`RPC_ENDPOINT_COOLDOWN_MS` removed and the
      docs corrected to describe the real health-scoring behaviour (issue
      #737): the effective failover knobs are `RPC_BREAKER_FAILURE_THRESHOLD`
      and `RPC_BREAKER_COOLDOWN_MS`.
- [ ] A reusable script wrapping the precondition `curl` calls above, rather
      than operators retyping them per rotation.
