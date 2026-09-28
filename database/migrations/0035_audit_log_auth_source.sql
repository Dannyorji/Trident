-- Migration 0035: audit_log gains auth_source (issue #616)
--
-- A request authenticated through the legacy API_KEY_HASHES env-var path has
-- no api_keys row, so api_key_id is legitimately NULL for it (the FK to
-- api_keys rules out a fabricated id). Before this migration that request's
-- audit_log row carried no attribution at all: api_key_id NULL, network
-- silently empty, indistinguishable from a bug swallowing the field. This
-- column lets the legacy path record *why* api_key_id is NULL ("legacy-env")
-- so every authenticated request has a non-null audit attribution somewhere
-- on the row, matching migration 0033's attempted_key_prefix/failure_reason
-- precedent for the equivalent problem on the 401 path.

ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS auth_source TEXT;
