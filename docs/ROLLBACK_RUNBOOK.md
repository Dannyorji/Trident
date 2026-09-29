# Rollback runbook

**Status: automated, not yet rehearsed end-to-end on staging.** Issue #460
asks for this procedure to be executed end-to-end on staging, with
wall-clock time measured, before launch. That rehearsal needs actual
staging access and still hasn't been performed, since this repo does not
have a live staging environment as of this pass (see issue #623). What
follows is the documented procedure, the automation now wired around it
(issue #621), and a real finding about migration reversibility (issue
#602) that should inform the eventual rehearsal.

## What is now automated (issue #621)

`.github/workflows/staging-deploy.yml` now runs a `rollback-staging` job
automatically whenever `deploy-staging` or `smoke-test-staging` fails. It
runs `helm rollback trident-staging 0` (roll back to the immediately
previous revision) in the staging namespace, then waits for the three app
deployments to report ready. If there is no previous revision to roll back
to (a release's first-ever install), it logs a warning and stops rather
than performing a no-op rollback that would mask the failure.

This closes the "no automated rollback exists" half of issue #621. It does
**not** by itself satisfy the "rehearsed" half: the job has never actually
run, because it only triggers on a real deploy/smoke-test failure against a
real staging cluster, and no staging cluster is configured yet. The measured
wall-clock time below is still a placeholder, not a real observation, until
someone deliberately breaks a staging deploy (or runs the steps by hand)
after staging exists and records what actually happened.

Automatic rollback also only covers the "schema change is backward
compatible" path described below: rolling back the Helm release to its
previous image tags and values. It has no opinion on whether the schema
left behind by the failed release is safe for the older app version. That
judgment, and any migration-boundary rollback, is still manual per the
sections below.

## Migration reversibility (issue #602)

`database/migrations/` has 33 numbered forward `.sql` files. As of #602,
**30 of them have a real `<version>_<name>.down.sql` rollback file**, and
the remaining **3 carry a documented `-- lint:allow-no-rollback` waiver**
in their own file header explaining exactly why an automated reverse is
not possible. `scripts/lint-migrations.sh` (rule 5) enforces this for every
future migration too: a new forward migration with neither a `.down.sql`
nor a waiver fails CI.

Reverse migrations run via `sqlx migrate revert` / `Migrator::undo` — sqlx
classifies each file by filename suffix (`.sql` = forward, `.down.sql` =
reverse) and never applies a `.down.sql` as part of the normal forward
chain (`sqlx migrate run` / `Migrator::run`). CI's own migration-application
steps (the raw `psql -f` loops in `.github/workflows/ci.yml`, used because
those jobs don't go through the sqlx CLI) explicitly skip `*.down.sql` for
the same reason.

**The 3 migrations without an automated rollback, and why:**

| Migration | Why it cannot be automatically reversed |
| --- | --- |
| `0017_soroban_events_partitioning.sql` | Converts `soroban_events` to a partitioned table and, in its final step, drops the pre-partitioning table (`soroban_events_legacy`) that a reverse migration would need to recreate the old (non-partitioned) shape from. The migration's own header documents a narrower *manual* rollback window that only exists before that step's COMMIT — not something a `.down.sql` can express, since it can only run after the forward migration has already finished. |
| `0030_failed_events_dedup.sql` | Deletes duplicate `failed_events` rows (keeping the newest per natural key and summing attempt counts into it). Each deleted row's own `occurred_at`/`error_message`/individual `attempts` value is gone; nothing preserves them, so there is no data to restore a rollback could read back. |
| `0031_network_enum_constraint.sql` | Normalises legacy `network` values (`pubnet` → `mainnet`, `standalone`/`local` → `sandbox`) across 14 tables via `UPDATE`, with no column recording which rows were changed or their original value. A row that entered as `pubnet` is indistinguishable from one that was always `mainnet` once this migration has run. |

**Everything else — including the two migrations most likely to look
risky at a glance — is fully reversible:**

- `0025_soroban_events_natural_key.sql` (adds a uniqueness constraint) and
  `0026_restore_soroban_events_indexes.sql` (re-creates six indexes that
  0017 silently failed to create) both have real down migrations. Because
  `sqlx migrate revert` walks the chain backwards, `0026`'s down (dropping
  those six indexes) always runs *before* `0025`'s down (dropping the
  natural-key constraint) during a revert — the two are independent
  objects, so this ordering is safe regardless of which one a revert stops
  at.
- `0001_init.sql`, the foundational schema, has a down migration that
  drops its four tables in reverse dependency order. Reverting it only
  makes sense once every later migration that touches those tables has
  also been reverted first, same as reverting any earlier link in the
  chain while later ones still depend on it — `sqlx migrate revert` only
  reverts one step at a time by design, so this is the same expectation
  bearing on every migration below the top of the chain, not a special
  case for 0001.

**CI now proves the reversible set actually works**, not just that the
files exist: `scripts/check-migration-rollback.sh` (wired into the
"Schema guard" job) applies the full forward chain against a scratch
database, then reverts every available `.down.sql` in reverse order — the
same order and direction `sqlx migrate revert` uses — and fails if any of
them errors. This runs on every PR, so a rollback file with a typo, a
wrong column/table name, or a wrong drop order relative to its neighbours
is caught at review time, not the first time someone needs it during an
incident. It does not (yet) assert byte-for-byte schema equality before
vs. after a forward-then-revert round trip — see that script's header for
what it does and does not prove.

## Application rollback procedure (image + chart)

1. Identify the previous known-good image tag and Helm chart revision:
   ```bash
   helm history trident -n <namespace>
   ```
2. Roll back the release:
   ```bash
   helm rollback trident <PREVIOUS_REVISION> -n <namespace>
   ```
3. Verify the rolled-back pods are serving:
   ```bash
   kubectl rollout status deployment/go-api -n <namespace>
   kubectl rollout status deployment/grpc-api -n <namespace>
   kubectl rollout status deployment/indexer -n <namespace>
   curl -sf https://<host>/v1/health
   ```
4. Confirm the indexer resumed from the correct cursor (no double-processed
   or skipped events) — check `soroban_events` for a gap or duplicate
   around the rollback timestamp.

## Rollback across a migration boundary

If the incident requires undoing a release that included a schema
migration:

1. **If the new schema is backward-compatible** with the previous app
   version (additive only — new nullable column, new table, new index):
   roll back the application per the steps above and leave the schema as
   is. This avoids running a reverse migration at all, which is generally
   preferable when it's an option.
2. **If the migration needs to be reversed** and it is one of the 30 with
   a `.down.sql` (see the table above for the 3 exceptions): run
   ```bash
   sqlx migrate revert --database-url "$DATABASE_URL"
   ```
   which reverts exactly the most recently applied migration. Reverting
   more than one step means running this command repeatedly — check
   `SELECT version, description FROM _sqlx_migrations ORDER BY version DESC`
   between each call to confirm which migration is being undone next, since
   `sqlx migrate revert` does not take a target version and stepping past
   the wrong stop point should not become part of an incident.
3. **If the migration is one of the 3 without an automated rollback**
   (`0017`, `0030`, `0031`): there is no `sqlx migrate revert` path past
   that point. This must be handled by a manually-written, reviewed reverse
   script informed by exactly what that migration's own header says is
   unrecoverable (see the table above), or by restoring from backup per
   `scripts/backup.sh`/`scripts/restore.sh` if the data loss is
   unacceptable to hand-patch around.

## What this rehearsal still needs (not done here)

- [ ] Actually run the above against staging, end to end.
- [ ] Record wall-clock time from "rollback called" to "verified serving."
- [ ] Attempt a migration-boundary rollback against a real backward-
      incompatible migration (staging only) to see what actually breaks.
- [ ] Run an actual `sqlx migrate revert` against staging (not just CI's
      scratch-database check) to confirm the app tolerates the reverted
      schema, not only that the SQL applies.
- [ ] Update this runbook with the exact commands/output from the
      rehearsal, not the generic commands above.
