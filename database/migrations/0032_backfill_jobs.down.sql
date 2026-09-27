-- Down migration for 0032_backfill_jobs.sql.
-- Drops the backfill_jobs table (and its indexes) introduced by this
-- migration.
--
-- lint:allow-destructive backfill_jobs did not exist before this migration;
--   dropping it discards the queue's history (pending/running/done/failed
--   rows). Operators rolling this back should confirm no worker has a job
--   claimed (status = 'running') and no jobs are pending that still need to
--   run, since the queue itself, not just its schema, disappears.

DROP TABLE IF EXISTS backfill_jobs;
