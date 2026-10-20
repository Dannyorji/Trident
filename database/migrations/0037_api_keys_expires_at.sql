-- Migration 0037: api_keys.expires_at (issue #752)
--
-- Nullable: NULL means the key never expires. Enforced at lookup time by the
-- REST, WebSocket and GraphQL auth paths, and the auth cache TTL is capped at
-- the time remaining so a cached key cannot outlive it.

ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
