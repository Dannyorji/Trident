-- Migration 0033: namespace the ledger cursor (and the health/alert-state
-- columns that share its system_state row) per network (issue #600).
--
-- system_state had a single 'latest_ledger_cursor' row with no network
-- dimension, so two indexers against one database (e.g. testnet and
-- mainnet) fought over it: get_cursor/commit_page's monotonic advance guard
-- prevented the lower-sequence network's cursor from ever moving once the
-- other network's cursor pulled ahead, silently stopping that network's
-- indexing entirely. The application now keys this row as
-- 'latest_ledger_cursor:<network>' (crates/indexer/src/db/mod.rs::cursor_key)
-- so each network gets its own row, independent cursor, health stats, and
-- alert state, under the same table and column shape.
--
-- Migrating the existing row: every deployment before this migration could
-- only ever index one network (that was the whole bug), so
-- soroban_events.network - if any events have been indexed yet - tells us
-- which one. Fall back to indexed_contracts.network (populated at startup
-- before any events exist) when soroban_events is still empty, and finally
-- to 'testnet' (the indexer's own config default - see
-- crates/indexer/src/config.rs) for a fresh, never-yet-run database. Either
-- way, no position is lost: the row's value, and its health/alert-state
-- columns, are carried over unchanged onto the new key.
DO $$
DECLARE
    detected_network text;
BEGIN
    -- Nothing to migrate on a fresh database that hasn't run 0001-0032 with
    -- data yet, or one already migrated past this point.
    IF NOT EXISTS (
        SELECT 1 FROM system_state WHERE key = 'latest_ledger_cursor'
    ) THEN
        RETURN;
    END IF;

    SELECT network INTO detected_network
    FROM soroban_events
    GROUP BY network
    ORDER BY COUNT(*) DESC
    LIMIT 1;

    IF detected_network IS NULL THEN
        SELECT network INTO detected_network
        FROM indexed_contracts
        WHERE network IS NOT NULL
        LIMIT 1;
    END IF;

    IF detected_network IS NULL THEN
        detected_network := 'testnet';
    END IF;

    UPDATE system_state
    SET key = 'latest_ledger_cursor:' || detected_network
    WHERE key = 'latest_ledger_cursor';

    RAISE NOTICE 'Migrated cursor row to latest_ledger_cursor:%', detected_network;
END $$;

-- Seed each of the four supported networks (migration 0031's CHECK
-- constraint vocabulary) with a fresh cursor row so a newly-added network on
-- an existing database can start advancing without a manual INSERT, exactly
-- like the original single-row seed did for a brand-new database.
INSERT INTO system_state (key, value)
VALUES
    ('latest_ledger_cursor:mainnet', '0'),
    ('latest_ledger_cursor:testnet', '0'),
    ('latest_ledger_cursor:futurenet', '0'),
    ('latest_ledger_cursor:sandbox', '0')
ON CONFLICT (key) DO NOTHING;
