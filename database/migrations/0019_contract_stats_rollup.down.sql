-- Down migration for 0019_contract_stats_rollup.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created.

DROP TABLE IF EXISTS contract_stats_rollup;
