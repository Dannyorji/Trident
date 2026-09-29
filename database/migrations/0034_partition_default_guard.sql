-- Migration 0034: guard create_soroban_partition against DEFAULT-partition
-- occupancy in the target range (issue #605).
-- ---------------------------------------------------------------------------
-- Problem
-- -------
-- soroban_events_default (migration 0017) is a catch-all: any row whose
-- ledger_sequence is not covered by a named range partition lands there
-- instead of failing. Once that happens, PostgreSQL cannot create a named
-- partition whose range overlaps rows already sitting in DEFAULT --
-- `CREATE TABLE ... PARTITION OF ... FOR VALUES FROM (a) TO (b)` requires the
-- parent's DEFAULT partition to hold zero rows in [a, b) and raises
-- "updated partition constraint ... is violated by some row" otherwise.
--
-- create_soroban_partition (0017) only checked for a name collision with an
-- existing partition, not for this condition. An operator hits the real
-- Postgres error with no indication of what to do about it -- exactly the
-- "operator discovers this mid-incident" scenario issue #605 describes, and
-- the crates/indexer streamer's own overflow guard
-- (assert_no_default_partition_overflow, issue #525) only prevents the
-- streamer itself from ever writing to DEFAULT; it is not wired into
-- crates/backfill, which writes soroban_events directly with no equivalent
-- check, so a backfill run against an uncovered range is a live path to
-- exactly this state.
--
-- Fix
-- ---
-- Before creating the new partition, count rows in soroban_events_default
-- whose ledger_sequence falls in [p_start, p_end). If any exist, raise an
-- exception naming the documented recovery procedure
-- (docs/db/default-partition-recovery.md) instead of letting the caller hit
-- Postgres's own less actionable constraint-violation error.

CREATE OR REPLACE FUNCTION create_soroban_partition(
    p_start BIGINT,
    p_end   BIGINT
) RETURNS TEXT LANGUAGE plpgsql AS $$
DECLARE
    pname TEXT;
    conflicting_rows BIGINT;
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

    -- DEFAULT-occupancy check (issue #605): a row already sitting in
    -- soroban_events_default within [p_start, p_end) blocks Postgres from
    -- attaching the new named partition over that range. Surface a clear,
    -- actionable error before hitting Postgres's own constraint-violation
    -- message, which names neither the cause nor the fix.
    EXECUTE format(
        'SELECT count(*) FROM soroban_events_default
         WHERE ledger_sequence >= %L AND ledger_sequence < %L',
        p_start, p_end
    ) INTO conflicting_rows;

    IF conflicting_rows > 0 THEN
        RAISE EXCEPTION
            'cannot create partition %: % row(s) in soroban_events_default '
            'already fall within [%, %). PostgreSQL cannot attach a named '
            'partition over a range that DEFAULT already holds rows for. '
            'Follow the detach-move-reattach procedure in '
            'docs/db/default-partition-recovery.md, then retry '
            'create_soroban_partition(%, %).',
            pname, conflicting_rows, p_start, p_end, p_start, p_end;
    END IF;

    EXECUTE format(
        'CREATE TABLE %I PARTITION OF soroban_events FOR VALUES FROM (%L) TO (%L)',
        pname, p_start, p_end
    );
    RETURN 'created: ' || pname;
END;
$$;

-- ---------------------------------------------------------------------------
-- DEFAULT-partition occupancy monitoring (issue #605)
-- ---------------------------------------------------------------------------
-- A thin view over the one number operators/alerting actually need: how many
-- rows currently sit in soroban_events_default. Zero is healthy; non-zero
-- means the next create_soroban_partition call for an overlapping range will
-- fail with the guard above, and the detach-move-reattach procedure should be
-- run proactively rather than waiting for that call to happen during an
-- incident. crates/indexer's alerting module (issue #75's Alerter) polls this
-- on the same cadence as its existing lag/RPC-degraded checks.
CREATE OR REPLACE VIEW soroban_events_default_partition_status AS
SELECT count(*) AS row_count,
       min(ledger_sequence) AS min_ledger_sequence,
       max(ledger_sequence) AS max_ledger_sequence
FROM soroban_events_default;
