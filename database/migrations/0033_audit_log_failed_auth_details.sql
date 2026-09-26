-- Migration 0033: audit_log gains attempted_key_prefix and failure_reason (issue #609)
--
-- A failed authentication attempt (401) has no api_key_id to record - the
-- key never resolved to a row - so before this migration a rejected request
-- audited nothing beyond "some request got a 401". The two new columns let
-- an audited failure be attributed to *which* key prefix was tried and *why*
-- it failed, without ever storing the full attempted key.

ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS attempted_key_prefix TEXT,
    ADD COLUMN IF NOT EXISTS failure_reason TEXT;
