-- Migration 0029: cover the audit_log admin-analytics and cleanup queries (#247).
-- ---------------------------------------------------------------------------
-- lint:allow-long-lock  sqlx wraps each migration in a transaction, and
--   CREATE INDEX CONCURRENTLY cannot run inside a transaction block — the
--   same constraint documented in 0009. audit_log is a high-write table, so
--   these builds do take a write-blocking lock for their duration; run this
--   migration in a maintenance window, or build the two indexes by hand with
--   CONCURRENTLY outside the migration chain first, in which case the
--   IF NOT EXISTS guards below make this migration a no-op.
--
-- What the live queries actually are
-- ----------------------------------
-- Collected from services/api/handlers/admin.go (AdminKeyUsage),
-- services/api/handlers/usage.go (RollupUsage) and services/api/main.go
-- (retention cleanup):
--
--   Q1  SELECT COUNT(*) FROM audit_log
--        WHERE api_key_id = $1 AND ts >= $2 AND ts < $3
--   Q2  SELECT COUNT(*) FROM audit_log
--        WHERE api_key_id = $1 AND ts >= $2 AND ts < $3 AND status_code < 400
--   Q3  SELECT endpoint, COUNT(*), AVG(duration_ms) FROM audit_log
--        WHERE api_key_id = $1 AND ts >= $2 AND ts < $3 GROUP BY endpoint
--   Q4  DELETE FROM audit_log WHERE ts < NOW() - ($1 || ' days')::INTERVAL
--        AND ctid IN (SELECT ctid FROM audit_log WHERE ts < ... LIMIT 1000)
--   Q5  SELECT api_key_id, date_trunc('day', ts), COUNT(*), ... FROM audit_log
--        WHERE api_key_id IS NOT NULL AND ts >= $1 GROUP BY 1, 2
--
-- Findings from EXPLAIN (ANALYZE, BUFFERS) at 2M rows / 312 MB
-- -----------------------------------------------------------
-- Full before/after plans are in docs/db/explain-audit-log-247.txt.
--
-- Q1/Q2/Q3 already matched idx_audit_log_key_ts (api_key_id, ts DESC), so
-- none of them seq-scanned. The cost was not the lookup but the heap: that
-- index carries only the two key columns, so every one of the ~10k matching
-- rows had to be fetched from the heap to read status_code, endpoint and
-- duration_ms. Each query paid a Bitmap Heap Scan over 9,999 heap blocks.
-- Q2 was the worst of the three in wasted work: it fetched all 9,999 rows
-- and then discarded 2,727 of them on `Filter: (status_code < 400)`, after
-- paying the heap I/O for every one.
--
-- Q5 (the 5-minute rollup job) matched idx_audit_log_ts but likewise had to
-- visit 16,667 heap blocks to read api_key_id, status_code and duration_ms.
--
-- Q4, the retention cleanup, plans as a Seq Scan — but that is the correct
-- plan for it, not a defect. See the note at the bottom.
--
-- The fix
-- -------
-- INCLUDE the payload columns rather than adding them to the key. They are
-- never used to seek or to order, only read, so making them key columns
-- would enlarge every internal page and slow descent for no benefit; as
-- INCLUDE columns they live only in the leaves and make the scan
-- index-only. This is what turns "9,999 heap blocks" into a heap-free plan
-- while the visibility map is fresh.

-- Covers Q1, Q2 and Q3: the same (api_key_id, ts DESC) prefix the old index
-- had, so it serves every lookup idx_audit_log_key_ts served, plus the three
-- columns the admin analytics read.
CREATE INDEX IF NOT EXISTS idx_audit_log_key_ts_covering
    ON audit_log (api_key_id, ts DESC)
    INCLUDE (status_code, endpoint, duration_ms);

-- Covers Q5, the usage rollup. Partial on `api_key_id IS NOT NULL` because
-- that is exactly the job's WHERE clause: rows whose key was deleted
-- (api_key_id is ON DELETE SET NULL) can never contribute to a per-key
-- rollup, so keeping them out shrinks the index and removes the recheck.
CREATE INDEX IF NOT EXISTS idx_audit_log_rollup
    ON audit_log (ts)
    INCLUDE (api_key_id, status_code, duration_ms)
    WHERE api_key_id IS NOT NULL;

-- idx_audit_log_key_ts is now a strict prefix of idx_audit_log_key_ts_covering
-- and earns nothing on a table this write-heavy, where every redundant index
-- is paid for on every INSERT.
--
-- lint:allow-destructive  Dropped only after the covering index above is
--   created in this same transaction, so no query is ever left without an
--   (api_key_id, ts DESC) index to use.
DROP INDEX IF EXISTS idx_audit_log_key_ts;

-- idx_audit_log_ts (ts DESC) is deliberately KEPT: it is the only index that
-- can serve a time-range predicate over rows where api_key_id IS NULL, which
-- the rollup's partial index excludes by construction.
--
-- Note on Q4, the retention cleanup, and why it is left alone
-- ----------------------------------------------------------
-- Q4's inner `SELECT ctid ... WHERE ts < NOW() - interval LIMIT 1000` plans
-- as a Seq Scan, before and after this migration. That is the right plan,
-- not a missing-index symptom, and the acceptance criterion "cleanup query
-- is index-supported" is met by *not* forcing an index onto it.
--
-- The predicate is not selective: with a 90-day retention window and 120
-- days of data, 496,072 of 1,996,000 rows (25%) satisfy `ts < NOW() - 90
-- days`. The LIMIT 1000 then means the executor only needs the first 1,000
-- of them. A sequential scan finds those almost immediately — measured at
-- 59 buffers and ~2 ms — whereas descending idx_audit_log_ts to fetch 1,000
-- scattered ctids would cost more. The planner is choosing correctly.
--
-- This was verified rather than assumed: rewriting the predicate three ways
-- (the current `($1 || ' days')::INTERVAL` concatenation, `make_interval(days
-- => $1)`, and a precomputed timestamptz) produced the same Seq Scan plan at
-- the same cost in all three cases, which rules out the parameter-opacity
-- explanation the query shape invites. See docs/db/explain-audit-log-247.txt.
--
-- The plan would change on its own if the shape of the data changed — a
-- backlog cleared down to a small tail, or a much longer retention window
-- makes the predicate selective, at which point idx_audit_log_ts starts
-- winning and is there to be used.
