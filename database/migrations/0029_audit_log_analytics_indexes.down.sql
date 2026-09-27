-- Down migration for 0029_audit_log_analytics_indexes.sql.
-- Restores audit_log's index set to what it was immediately before 0029:
-- recreates the plain (api_key_id, ts DESC) index that 0029 dropped in favor
-- of the covering index, then drops the two indexes 0029 added.
--
-- lint:allow-long-lock  Same constraint as 0029 and 0009: CREATE INDEX
--   CONCURRENTLY cannot run inside sqlx's per-migration transaction. Run in a
--   maintenance window, or build idx_audit_log_key_ts with CONCURRENTLY by
--   hand first, in which case the IF NOT EXISTS guard below makes this
--   statement a no-op.
--
-- Ordering matters: the replacement index is created before the covering
-- index is dropped, so audit_log is never left, even briefly, without an
-- (api_key_id, ts DESC) index for the admin-analytics queries (Q1-Q3 in
-- 0029's header) to use.
CREATE INDEX IF NOT EXISTS idx_audit_log_key_ts
    ON audit_log (api_key_id, ts DESC);

DROP INDEX IF EXISTS idx_audit_log_key_ts_covering;
DROP INDEX IF EXISTS idx_audit_log_rollup;
