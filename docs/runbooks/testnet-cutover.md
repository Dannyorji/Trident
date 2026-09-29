# Testnet cutover runbook

**Status: documented procedure, not yet walked through by someone who
didn't write it.** Issue #502's "done when" is a real dry-run against
staging with a second person driving from this document alone. That walkthrough
needs staging access and a second engineer, neither available in this pass.
What follows is the ordered procedure, grounded in what this repo's
deployment tooling, CI gates, and prior runbooks actually do — not generic
advice.

## Preconditions

All of the following must be true before cutover starts. None of these are
new checks invented for this runbook — they're the existing gates this repo
already has, gathered into one list.

### CI / code

- [ ] The commit being deployed passed CI's `docker`, `e2e`, and
      `e2e-contract-events` jobs (see [`docs/CI.md`](../CI.md)) on `dev`.
- [ ] Coverage floors are met (`docs/CI.md`'s "Coverage: collection and
      enforced floors" section) — a coverage regression here is a signal
      the change wasn't tested as thoroughly as the rest of the codebase.
- [ ] `scripts/lint-migrations.sh` and `scripts/check-schema-drift.sh` are
      green if the release includes a migration. **Known gap, found while
      writing this runbook**: `check-schema-drift.sh`'s own documentation
      states it compares tables/columns/indexes/constraints — it does
      **not** compare functions. `create_soroban_partition` (added in
      migration `0017`) is missing from `database/schema.sql` today and
      this check does not catch it (see
      [`RESTORE_DRILL_RUNBOOK.md`](../RESTORE_DRILL_RUNBOOK.md)'s
      "What was actually done" step 4). Don't rely on this check alone to
      certify a migration that adds or changes a function.

### Which issues must be closed

- [ ] [#431](https://github.com/Telocel-Labs/Trident/issues/431) —
      automated backups with a real restore performed. **Currently open.**
      Cutting over to testnet without this means the first real incident
      that needs a restore will be the first time anyone has ever
      performed one. See
      [`RESTORE_DRILL_RUNBOOK.md`](../RESTORE_DRILL_RUNBOOK.md) for what's
      been verified so far (schema/partition round-trip, real dump/restore
      timing) and what's still missing (a real backup to restore, at
      production-shaped scale).
- [ ] [#460](https://github.com/Telocel-Labs/Trident/issues/460) — rollback
      rehearsed. **Closed**, but its own runbook
      ([`ROLLBACK_RUNBOOK.md`](../ROLLBACK_RUNBOOK.md)) is explicitly
      marked "template — not yet rehearsed" and documents a real, load-bearing
      finding: **there are zero `.down.sql` files for any of the 25
      migrations** — schema rollback is not automated today. Read that
      runbook's "Finding: migrations here are forward-only" section before
      cutover, especially if the release being cut over includes a
      migration.
- [ ] [#445](https://github.com/Telocel-Labs/Trident/issues/445) —
      incident response process with a named on-call owner. **Closed** —
      [`incident-response.md`](incident-response.md) exists with severity
      levels, escalation path, and a communication channel. Its "On-call
      owner — launch week" section is a `[FILL IN: ...]` placeholder as of
      this writing — confirm it has real names/contacts filled in before
      cutover, not just that the section exists.
- [ ] Row 1 of [`LAUNCH_CHECKLIST.md`](../LAUNCH_CHECKLIST.md) (alerts
      verified firing) and row 9 (rollback rehearsed within the last 30
      days) are checked off with evidence, per that checklist's own no-go
      criteria.

## Ordered cutover steps

Each step names an owner role (not a person — fill in the actual name in
the walkthrough) and a verification to run immediately after, not deferred
to the end.

### 1. Freeze `dev` and cut the release branch — *release owner*

```bash
git checkout dev && git pull
git checkout -b release/testnet-cutover-<date>
```

**Verify:** CI is green on the release branch (same jobs as the preconditions
above, re-run on the exact commit being cut over — not assumed still-green
from when it merged to `dev`).

### 2. Confirm environment configuration for testnet — *deploy owner*

Per [`docs/ENVIRONMENT.md`](../ENVIRONMENT.md), `NETWORK` defaults to
`testnet` and needs no override for a testnet deployment; confirm the
target `.env` does not have a stale `NETWORK=mainnet` or
`NETWORK=futurenet` left over from a previous environment's config.

**Verify:**
```bash
grep -E '^NETWORK=' .env   # expect: NETWORK=testnet, or absent (defaults to testnet)
```

### 3. Take a pre-cutover database snapshot — *database owner*

Even without #431's automated backups yet, take a manual one immediately
before cutover so there is at least one restore point:

```bash
pg_dump -Fc -h <host> -U trident -d trident -f pre-cutover-$(date -u +%Y%m%dT%H%M%SZ).dump
```

**Verify:** the dump file exists and is non-empty; spot-check its size is in
the expected range for the current database (compare against the previous
manual snapshot, if any — a dump an order of magnitude smaller than
expected usually means a connection/permission problem, not an empty
database).

### 4. Run migrations — *database owner*

Per [`docs/deployment.md`](../deployment.md#5-start-postgresql-and-run-database-migrations)'s
existing migration procedure.

**Verify:** `sqlx migrate info` (or the project's equivalent) shows every
migration applied, none pending. Re-run `scripts/check-schema-drift.sh`
against the now-migrated database to confirm no drift was introduced by
this release — subject to the function-comparison gap noted in
Preconditions above.

### 5. Deploy the indexer and API — *deploy owner*

Per [`docs/deployment.md`](../deployment.md#6-start-all-services)'s existing
deploy steps.

**Verify:** immediately after, per
[`docs/deployment.md`](../deployment.md#7-verify-health):
```bash
curl -sf https://<host>/v1/health
```
Confirm the indexer's ingest cursor is advancing (not stuck at whatever it
was pre-cutover) — a health check passing does not by itself confirm the
indexer resumed consuming testnet ledgers.

### 6. Watch for the first full ingest cycle — *on-call owner*

Per [`incident-response.md`](incident-response.md)'s alert catalog,
specifically `TridentIndexerHeartbeatStale` and `TridentIndexerLagCritical`
— these are exactly the signals that would fire if cutover left the indexer
unable to reach testnet RPC or resume its cursor correctly.

**Verify:** no SEV-1/SEV-2 alert fires within one full expected ingest
cycle after cutover. Manually query recent `soroban_events` rows and
confirm `ledger_timestamp` values are current, not frozen at the pre-cutover
watermark.

### 7. Announce cutover complete — *release owner*

Per [`incident-response.md`](incident-response.md#user-communication-channel)'s
existing communication channel — post confirmation there, not ad hoc.

## Rollback trigger and procedure

**Trigger**: any SEV-1 per [`incident-response.md`](incident-response.md#sev-1-service-down-or-data-incorrect)'s
definition within the first ingest cycle after cutover, or a health check
that never turns green within 15 minutes of step 5.

**Who calls it**: per [`LAUNCH_CHECKLIST.md`](../LAUNCH_CHECKLIST.md#rollback-decision-procedure) —
the on-call engineer, or the release owner if reachable within 5 minutes.
Do not wait for a quorum.

**Procedure**: follow [`ROLLBACK_RUNBOOK.md`](../ROLLBACK_RUNBOOK.md) in
full. If this release included a migration, read that runbook's
"Rollback across a migration boundary" section first — **a
backward-incompatible migration currently has no automated reverse path**,
which may mean the correct rollback for this specific release is
"roll back the application only, leave the schema" (if the migration was
additive) rather than a full schema rollback. Decide which case applies
*before* cutover, not while already mid-incident.

## What this runbook still needs (not done here)

- [ ] A real walkthrough by someone who did not write this document,
      driving from this file alone against a real staging environment —
      this is #502's literal "done when" criterion and the single most
      important gap.
- [ ] #431 landed, so precondition checkbox 1 above can actually be
      checked rather than flagged as open.
- [ ] `incident-response.md`'s on-call section filled in with real names.
- [ ] The exact commands in each step re-verified against whatever the
      target testnet environment's actual hostnames/credentials are (the
      commands above use placeholders consistent with `deployment.md`'s
      existing style).
