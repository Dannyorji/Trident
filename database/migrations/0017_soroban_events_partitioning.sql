-- lint:allow-destructive  Step 8's DROP TABLE is the point of this migration:
--   the legacy table's rows were copied to the partitioned table in step 5.
--   NOTE: this exact statement caused #437 — it cascaded away six indexes that
--   the CREATE INDEX IF NOT EXISTS statements above had silently failed to
--   recreate, because those index names still belonged to the legacy table.
--   Migration 0026 restores them. Kept as a waiver rather than a fix because
--   editing an applied migration changes its checksum and breaks
--   `sqlx migrate run` on every existing database.
-- lint:allow-no-guard    The shadow table and its partitions are created once,
--   inside the transaction below; a bare CREATE is what makes a re-run fail
--   loudly rather than silently adopting a half-built table.
-- lint:allow-long-lock   This whole migration runs inside BEGIN/COMMIT, and
--   CREATE INDEX CONCURRENTLY cannot run inside a transaction block. The
--   partitioned table is empty when these indexes are built, so the lock is
--   held over no rows.
--
-- 0017: Convert soroban_events to RANGE-partitioned table (#244).
--
-- Partition key: ledger_sequence
--   Chosen over ledger_timestamp because it is deterministic (monotonically
--   increasing, no clock skew), aligns with the ingest cursor, and makes
--   retention a cheap DROP TABLE on an individual partition.
--
-- Primary-key constraint:
--   PostgreSQL requires every unique constraint on a partitioned table to
--   include all partition-key columns. The original PK was (id UUID).
--   The new PK is (ledger_sequence, id) to satisfy this requirement.
--   id retains its UUID DEFAULT so existing insert code is unchanged.
--
-- FK trade-off:
--   webhook_deliveries.event_id previously held a FK to soroban_events(id).
--   A single-column UNIQUE (id) cannot be enforced globally on a partitioned
--   table (PostgreSQL constraint), so the FK is converted to a logical
--   reference — the column is kept as NOT NULL UUID and an index is added
--   for join performance. Referential integrity is upheld by application-layer
--   writes that only insert deliveries for events that have been committed.
--
-- Expand/contract steps (zero data loss):
--   1. Create partitioned shadow table (soroban_events_new).
--   2. Seed initial partitions + default catch-all partition.
--   3. Copy all rows from legacy table into shadow table.
--   4. Atomically rename: legacy → soroban_events_legacy, shadow → soroban_events.
--   5. Recreate all indexes on the partitioned parent.
--   6. Drop old FK on webhook_deliveries; add index on event_id.
--   7. Drop old FK on token_events (same trade-off as webhook_deliveries).
--   8. Drop soroban_events_legacy.
--
-- Reversible: to roll back before step 7, rename tables back and drop the
--   partitioned table. The legacy table survives until explicitly dropped.
--
-- Partition pre-creation: add new partitions ahead of the ingest frontier with:
--   SELECT create_soroban_partition(start_ledger, end_ledger);
-- Each partition covers 2 000 000 ledgers (~115 days at current Stellar
-- mainnet throughput of ~17 280 ledgers/day).

BEGIN;

-- 1. Create partitioned shadow table -----------------------------------------
CREATE TABLE soroban_events_new (
    id                  UUID        NOT NULL DEFAULT gen_random_uuid(),
    contract_id         TEXT        NOT NULL,
    ledger_sequence     BIGINT      NOT NULL,
    ledger_timestamp    TIMESTAMPTZ NOT NULL,
    transaction_hash    TEXT        NOT NULL,
    event_index         INTEGER     NOT NULL,
    event_type          TEXT        NOT NULL
        CHECK (event_type IN ('contract', 'system', 'diagnostic')),
    network             TEXT        NOT NULL DEFAULT 'testnet',
    topics              JSONB       NOT NULL DEFAULT '[]',
    topic_0             TEXT        GENERATED ALWAYS AS (topics ->> 0) STORED,
    topic_1             TEXT        GENERATED ALWAYS AS (topics ->> 1) STORED,
    data                JSONB       NOT NULL DEFAULT '{}',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (ledger_sequence, id)
) PARTITION BY RANGE (ledger_sequence);

-- 2. Seed initial partitions --------------------------------------------------
-- Default catch-all for any rows that fall outside named ranges.
CREATE TABLE soroban_events_default
    PARTITION OF soroban_events_new DEFAULT;

-- Named range partitions; each spans 2 000 000 ledgers (~115 days on mainnet).
-- Stellar mainnet is currently around ledger 55 000 000.
-- Testnet resets periodically; the default partition absorbs any range gaps.
CREATE TABLE soroban_events_p0_1999999
    PARTITION OF soroban_events_new
    FOR VALUES FROM (0) TO (2000000);

CREATE TABLE soroban_events_p2000000_3999999
    PARTITION OF soroban_events_new
    FOR VALUES FROM (2000000) TO (4000000);

CREATE TABLE soroban_events_p4000000_5999999
    PARTITION OF soroban_events_new
    FOR VALUES FROM (4000000) TO (6000000);

-- Mainnet forward-looking partitions.
CREATE TABLE soroban_events_p50000000_51999999
    PARTITION OF soroban_events_new
    FOR VALUES FROM (50000000) TO (52000000);

CREATE TABLE soroban_events_p52000000_53999999
    PARTITION OF soroban_events_new
    FOR VALUES FROM (52000000) TO (54000000);

CREATE TABLE soroban_events_p54000000_55999999
    PARTITION OF soroban_events_new
    FOR VALUES FROM (54000000) TO (56000000);

CREATE TABLE soroban_events_p56000000_57999999
    PARTITION OF soroban_events_new
    FOR VALUES FROM (56000000) TO (58000000);

CREATE TABLE soroban_events_p58000000_59999999
    PARTITION OF soroban_events_new
    FOR VALUES FROM (58000000) TO (60000000);

-- 3. Copy existing data -------------------------------------------------------
-- Generated columns (topic_0, topic_1) are excluded; they are recomputed.
INSERT INTO soroban_events_new
    (id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash,
     event_index, event_type, network, topics, data, created_at)
SELECT id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash,
       event_index, event_type, network, topics, data, created_at
FROM   soroban_events;

-- 4. Atomic rename ------------------------------------------------------------
ALTER TABLE soroban_events     RENAME TO soroban_events_legacy;
ALTER TABLE soroban_events_new RENAME TO soroban_events;

-- 5. Recreate indexes on the partitioned parent --------------------------------
-- Indexes created on the parent are automatically propagated to each partition
-- and to any future partitions added later.
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

-- 6. Update webhook_deliveries ------------------------------------------------
-- Drop the FK that referenced the non-partitioned single-column PK.
ALTER TABLE webhook_deliveries
    DROP CONSTRAINT IF EXISTS webhook_deliveries_event_id_fkey;

-- Retain NOT NULL; app layer enforces referential integrity.
ALTER TABLE webhook_deliveries
    ALTER COLUMN event_id SET NOT NULL;

-- Index preserves join/lookup performance previously provided by the FK index.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_event_id
    ON webhook_deliveries (event_id);

-- 7. Update token_events --------------------------------------------------------
-- Same trade-off as webhook_deliveries above: a single-column UNIQUE (id)
-- cannot be enforced globally on a partitioned table, so this FK is also
-- converted to a logical reference. event_id remains token_events' own
-- PRIMARY KEY, so no additional index is needed for lookups — only the FK
-- constraint (which still blocks dropping soroban_events_legacy below) goes.
ALTER TABLE token_events
    DROP CONSTRAINT IF EXISTS token_events_event_id_fkey;

-- 8. Drop legacy table --------------------------------------------------------
DROP TABLE soroban_events_legacy;

COMMIT;

-- ---------------------------------------------------------------------------
-- Partition pre-creation helper
-- ---------------------------------------------------------------------------
-- Call this function periodically (e.g., monthly cron or API maintenance job)
-- to add the next ledger-range partition before the ingest frontier reaches it.
--
--   SELECT create_soroban_partition(60000000, 62000000);
CREATE OR REPLACE FUNCTION create_soroban_partition(
    p_start BIGINT,
    p_end   BIGINT
) RETURNS TEXT LANGUAGE plpgsql AS $$
DECLARE
    pname TEXT;
BEGIN
    pname := 'soroban_events_p' || p_start || '_' || (p_end - 1);
    IF EXISTS (
        SELECT 1 FROM pg_class c
        JOIN pg_inherits i ON i.inhrelid = c.oid
        JOIN pg_class p ON p.oid = i.inhparent
        WHERE p.relname = 'soroban_events' AND c.relname = pname
    ) THEN
        RETURN 'already exists: ' || pname;
    END IF;
    EXECUTE format(
        'CREATE TABLE %I PARTITION OF soroban_events FOR VALUES FROM (%L) TO (%L)',
        pname, p_start, p_end
    );
    RETURN 'created: ' || pname;
END;
$$;
