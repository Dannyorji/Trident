-- Down migration for 0004_soroban_events_network.sql.
-- Drops the network column and its two indexes from soroban_events.
--
-- lint:allow-destructive dropping a column added by this migration's own
--   forward half; no other migration in the chain depends on it existing.

DROP INDEX IF EXISTS idx_soroban_events_network_contract;
DROP INDEX IF EXISTS idx_soroban_events_network;

ALTER TABLE soroban_events
    DROP COLUMN IF EXISTS network;
