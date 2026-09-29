-- Migration 0026: restore soroban_events performance indexes lost by 0017 (#437).
-- ---------------------------------------------------------------------------
-- Root cause
-- ----------
-- Migration 0017 (partitioning) renamed the original table to
-- `soroban_events_legacy` and only then ran, on the new partitioned table:
--
--   CREATE INDEX IF NOT EXISTS idx_soroban_events_network ...
--   CREATE INDEX IF NOT EXISTS idx_soroban_events_network_contract ...
--   CREATE INDEX IF NOT EXISTS idx_soroban_events_contract_ledger ...
--   CREATE INDEX IF NOT EXISTS idx_soroban_events_contract_topic0 ...
--   CREATE INDEX IF NOT EXISTS idx_soroban_events_id_desc ...
--   CREATE INDEX IF NOT EXISTS idx_soroban_events_ledger_timestamp ...
--
-- Index names are unique per-schema in Postgres, not per-table. At that
-- point in 0017, indexes with those exact names still existed — attached to
-- `soroban_events_legacy` (renaming a table does not rename its indexes) —
-- so every one of those `IF NOT EXISTS` statements silently no-op'd instead
-- of creating a new index on the partitioned table. Step 8 of 0017 then
-- dropped `soroban_events_legacy`, which cascaded to drop those old indexes
-- for good. Net effect: since 0017, `soroban_events` has carried only its
-- primary key `(ledger_sequence, id)` and (as of 0025) the natural-key
-- unique constraint — none of the six indexes migrations 0004/0009 intended.
--
-- `database/schema.sql` was never wrong — it already lists all six indexes;
-- the live migration chain just silently stopped producing them since 0017.
-- This is exactly the drift #436 adds CI protection against going forward.
-- Verified with `SELECT indexname FROM pg_indexes WHERE tablename =
-- 'soroban_events'` against a database built from the full 25-migration
-- chain: only the PK and the 0025 unique constraint were present.
--
-- Impact (see docs/db/explain-before.txt / explain-after.txt for real
-- `EXPLAIN (ANALYZE, BUFFERS)` evidence at 1.5M-row scale): `get_event`, the
-- `list_events` cursor lookup, and `list_events`' main contract-scoped query
-- could not use a selective index and instead did a near-full scan of the
-- primary key over every partition.
--
-- Fix: recreate the six indexes. The original names are safe to reuse here —
-- the legacy table that held them was dropped in the same transaction 0017
-- ran in, so in every environment where 0017 applied successfully, these
-- names are guaranteed free.
--
-- lint:allow-long-lock  sqlx wraps each migration in a transaction, and
--   CREATE INDEX CONCURRENTLY cannot run inside a transaction block (#642,
--   same constraint documented in 0009/0029). soroban_events is the
--   highest-volume, partitioned table, so these six non-concurrent builds do
--   take a write-blocking lock across every partition for their duration;
--   run this migration in a maintenance window, or build the indexes by hand
--   with CONCURRENTLY outside the migration chain first, in which case the
--   IF NOT EXISTS guards below make this migration a no-op.

CREATE INDEX IF NOT EXISTS idx_soroban_events_network
    ON soroban_events (network);

CREATE INDEX IF NOT EXISTS idx_soroban_events_network_contract
    ON soroban_events (network, contract_id);

CREATE INDEX IF NOT EXISTS idx_soroban_events_contract_ledger
    ON soroban_events (contract_id, ledger_sequence DESC);

CREATE INDEX IF NOT EXISTS idx_soroban_events_contract_topic0
    ON soroban_events (contract_id, topic_0)
    WHERE topic_0 IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_soroban_events_id_desc
    ON soroban_events (id DESC);

CREATE INDEX IF NOT EXISTS idx_soroban_events_ledger_timestamp
    ON soroban_events (ledger_timestamp DESC);
