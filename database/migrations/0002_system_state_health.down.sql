-- Down migration for 0002_system_state_health.sql.
-- Drops the indexer health columns added to system_state. Additive-only
-- forward migration, so the reverse is a clean drop with no data to preserve.

ALTER TABLE system_state
    DROP COLUMN IF EXISTS last_poll_at,
    DROP COLUMN IF EXISTS last_ledger_indexed,
    DROP COLUMN IF EXISTS events_indexed_total,
    DROP COLUMN IF EXISTS events_in_last_poll,
    DROP COLUMN IF EXISTS poll_duration_ms;
