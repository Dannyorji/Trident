-- Down migration for 0025_soroban_events_natural_key.sql.
-- Drops the natural-key uniqueness constraint this migration added. The
-- pre-flight duplicate check and the constraint's ledger_sequence-scoping
-- rationale are both documentation, not data changes; nothing else in this
-- migration mutates rows, so dropping the constraint fully restores the
-- pre-migration state.

ALTER TABLE soroban_events
    DROP CONSTRAINT IF EXISTS uq_soroban_events_tx_index_network;
