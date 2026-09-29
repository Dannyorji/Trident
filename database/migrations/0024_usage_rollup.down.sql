-- Down migration for 0024_usage_rollup.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created.

DROP TABLE IF EXISTS usage_rollup;
