-- Down migration for 0018_contract_specs.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created.

DROP TABLE IF EXISTS contract_specs;
