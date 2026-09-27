-- Down migration for 0012_contract_invocation_metrics.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created.

DROP TABLE IF EXISTS contract_invocation_metrics;
