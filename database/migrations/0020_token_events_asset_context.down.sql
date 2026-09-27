-- Down migration for 0020_token_events_asset_context.sql.
--
-- lint:allow-destructive dropping columns added by this migration's own
--   forward half.

DROP INDEX IF EXISTS idx_token_events_asset_code;

ALTER TABLE token_events
    DROP COLUMN IF EXISTS asset_code,
    DROP COLUMN IF EXISTS asset_issuer;
