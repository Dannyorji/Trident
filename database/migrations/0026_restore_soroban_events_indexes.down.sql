-- Down migration for 0026_restore_soroban_events_indexes.sql.
-- Drops the six indexes this migration (re-)created, restoring soroban_events
-- to the state it was actually in immediately before 0026 ran: carrying only
-- its primary key (ledger_sequence, id) and, if 0025 has also been applied,
-- the uq_soroban_events_tx_index_network natural-key constraint from 0025.
-- That prior state was the accidental one described in 0026's header (the
-- six indexes silently missing since 0017's CREATE INDEX IF NOT EXISTS
-- statements no-op'd against the about-to-be-dropped legacy table) rather
-- than a deliberately designed one, but a rollback's job is to undo this
-- migration's own change, not to also re-fix 0017 — reverting further back
-- than that is out of scope for this file, same as any other down migration
-- in this chain only undoes its own forward half.
--
-- This down migration is independent of whether 0025 is applied or reverted:
-- 0025 added a UNIQUE constraint, a separate object from the six plain
-- indexes below, so dropping these six never needs to touch it either way.

DROP INDEX IF EXISTS idx_soroban_events_network;
DROP INDEX IF EXISTS idx_soroban_events_network_contract;
DROP INDEX IF EXISTS idx_soroban_events_contract_ledger;
DROP INDEX IF EXISTS idx_soroban_events_contract_topic0;
DROP INDEX IF EXISTS idx_soroban_events_id_desc;
DROP INDEX IF EXISTS idx_soroban_events_ledger_timestamp;
