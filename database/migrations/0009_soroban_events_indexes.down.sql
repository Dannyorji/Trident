-- Down migration for 0009_soroban_events_indexes.sql.
-- Drops the four composite indexes this migration added to soroban_events.

DROP INDEX IF EXISTS idx_soroban_events_contract_ledger;
DROP INDEX IF EXISTS idx_soroban_events_contract_topic0;
DROP INDEX IF EXISTS idx_soroban_events_id_desc;
DROP INDEX IF EXISTS idx_soroban_events_ledger_timestamp;
