-- Down migration for 0028_failed_events.sql.
-- Drops the failed_events table (and its indexes, via CASCADE-by-default on
-- DROP TABLE) introduced by this migration.
--
-- lint:allow-destructive failed_events did not exist before this migration;
--   dropping it discards any dead-lettered events recorded since. Operators
--   rolling this back should replay or export pending rows first if any
--   exist (`SELECT * FROM failed_events WHERE replayed_at IS NULL`).

DROP TABLE IF EXISTS failed_events;
