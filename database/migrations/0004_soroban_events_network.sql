-- lint:allow-long-lock Historical migration, already applied everywhere. These
-- indexes were built when soroban_events was empty or near-empty, so the
-- non-CONCURRENT build blocked nothing. Editing an applied migration would
-- change its checksum and break `sqlx migrate run` on existing databases.
--
-- Add network column to soroban_events for per-network data isolation.
-- Existing rows default to 'testnet' to match the initial deployment target;
-- adjust via a backfill if mainnet data was already ingested.

ALTER TABLE soroban_events
    ADD COLUMN IF NOT EXISTS network TEXT NOT NULL DEFAULT 'testnet';

CREATE INDEX IF NOT EXISTS idx_soroban_events_network ON soroban_events (network);

-- Composite index for the most common authenticated query pattern:
-- events filtered by network + contract.
CREATE INDEX IF NOT EXISTS idx_soroban_events_network_contract
    ON soroban_events (network, contract_id);
