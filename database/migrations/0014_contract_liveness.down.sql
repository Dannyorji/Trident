-- Down migration for 0014_contract_liveness.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created.

DROP TABLE IF EXISTS contract_liveness;
