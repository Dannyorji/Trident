-- Down migration for 0033_audit_log_failed_auth_details.sql.
-- Drops the two columns this migration added.
--
-- lint:allow-destructive attempted_key_prefix and failure_reason did not
--   exist before this migration; dropping them discards any failed-auth
--   detail recorded since. audit_log's other columns (including whether a
--   row represents a 401 at all) are untouched.

ALTER TABLE audit_log
    DROP COLUMN IF EXISTS attempted_key_prefix,
    DROP COLUMN IF EXISTS failure_reason;
