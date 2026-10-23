-- Migration 0038: add network to parse_errors and failed_events
--
-- Both dead-letter tables had no network dimension, so in a database shared by
-- several indexers (e.g. testnet and mainnet) the reconciler's per-ledger
-- counts picked up the other network's parse_errors rows as "extra events",
-- the dead-letter backlog gauge mixed networks, and the replay CLI had to
-- guess which network to tag a replayed event with from its own environment.
--
-- Backfill: every deployment before this migration indexed a single network,
-- so soroban_events.network tells us which one. Use it when it holds exactly
-- one distinct value; otherwise (empty table, or already mixed) fall back to
-- 'testnet', the indexer's own config default.

ALTER TABLE parse_errors
    ADD COLUMN IF NOT EXISTS network TEXT;
ALTER TABLE failed_events
    ADD COLUMN IF NOT EXISTS network TEXT;

DO $$
DECLARE
    detected text;
BEGIN
    SELECT CASE WHEN COUNT(DISTINCT network) = 1 THEN MIN(network) END
      INTO detected
      FROM soroban_events;
    detected := COALESCE(detected, 'testnet');

    UPDATE parse_errors SET network = detected WHERE network IS NULL;
    UPDATE failed_events SET network = detected WHERE network IS NULL;
END $$;

ALTER TABLE parse_errors
    ALTER COLUMN network SET DEFAULT 'testnet',
    ALTER COLUMN network SET NOT NULL;
ALTER TABLE failed_events
    ALTER COLUMN network SET DEFAULT 'testnet',
    ALTER COLUMN network SET NOT NULL;

ALTER TABLE parse_errors
    ADD CONSTRAINT chk_parse_errors_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));
ALTER TABLE failed_events
    ADD CONSTRAINT chk_failed_events_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

-- Reconciler counts per (network, ledger window); backlog/replay filter pending
-- rows by network.
CREATE INDEX IF NOT EXISTS idx_parse_errors_network_ledger
    ON parse_errors (network, ledger_sequence);
CREATE INDEX IF NOT EXISTS idx_failed_events_network_pending
    ON failed_events (network, occurred_at) WHERE replayed_at IS NULL;

-- Pending rows are unique per event *per network*: the same contract id and
-- ledger sequence can legitimately occur on two networks.
DROP INDEX IF EXISTS uq_failed_events_pending;
CREATE UNIQUE INDEX IF NOT EXISTS uq_failed_events_pending
    ON failed_events (network, contract_id, ledger_sequence, event_index)
    WHERE replayed_at IS NULL;
