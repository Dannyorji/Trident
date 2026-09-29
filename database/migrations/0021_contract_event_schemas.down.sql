-- Down migration for 0021_contract_event_schemas.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created.

DROP TABLE IF EXISTS contract_event_schemas;
