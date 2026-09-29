-- Down migration for 0034_partition_default_guard.sql.
-- Drops the monitoring view and restores create_soroban_partition to its
-- pre-0034 body (0017's original: name-collision check only, no
-- DEFAULT-occupancy guard).
--
-- lint:allow-destructive Restoring the prior function body necessarily
--   removes the DEFAULT-occupancy check this migration added; that is the
--   entire point of a rollback here, not incidental data loss -- no rows
--   are dropped, only the guard logic reverts to what 0017 shipped.

DROP VIEW IF EXISTS soroban_events_default_partition_status;

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
