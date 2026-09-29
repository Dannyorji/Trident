-- Down migration for 0023_contract_storage_snapshots.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created. Note this migration was renumbered from an earlier 0019 (#371);
--   the down file follows the current, live version number (0023).

DROP TABLE IF EXISTS contract_storage_snapshots;
