# Rollback runbook

**Status: automated, not yet rehearsed end-to-end on staging.** Issue #460
asks for this procedure to be executed end-to-end on staging, with
wall-clock time measured, before launch. That rehearsal needs actual
staging access and still hasn't been performed, since this repo does not
have a live staging environment as of this pass (see issue #623). What
follows is the documented procedure, the automation now wired around it
(issue #621), and a real finding about migration reversibility that should
inform the eventual rehearsal.

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

## Finding: migrations here are forward-only

`database/migrations/` has 25 numbered `.sql` files and **zero**
corresponding `.down.sql` (or equivalent reverse) files. That means there
is currently no automated way to reverse a schema migration — "which
migrations are reversible" is, today, **none of them** in an automated
sense. Any rollback that needs to undo a schema change requires either:

- Rolling back to the previous **application** version only, leaving the
  new schema in place (safe only if the new schema is backward-compatible
  with the old app version — this is the expand/contract pattern the
  issue asks about, and it isn't consistently followed today since there's
  no tooling enforcing it), or
- A hand-written reverse SQL script, written and reviewed under incident
  pressure — the worst time to write one for the first time.

**Recommendation** (not implemented in this pass): require a `.down.sql`
alongside every new migration going forward, and treat any migration that
can't be cleanly reversed (e.g. a dropped column) as needing an
expand/contract split — add the new shape, migrate data, only drop the
old shape in a later, separate release once nothing depends on it.

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
   is. This is the only rollback path this repo currently supports
   cleanly.
2. **If the migration is not backward-compatible** (a dropped/renamed
   column, a changed constraint the old app version doesn't expect): there
   is currently no automated reverse path. This must be handled by a
   manually-written, reviewed reverse script — which is exactly the
   scenario the "migrations are forward-only" finding above means to flag
   as a gap to close before relying on this runbook under real pressure.

## What this rehearsal still needs (not done here)

- [ ] Actually run the above against staging, end to end.
- [ ] Record wall-clock time from "rollback called" to "verified serving."
- [ ] Attempt a migration-boundary rollback against a real backward-
      incompatible migration (staging only) to see what actually breaks.
- [ ] Update this runbook with the exact commands/output from the
      rehearsal, not the generic commands above.
