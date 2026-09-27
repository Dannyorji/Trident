-- Down migration for 0027_webhook_secret_rotation.sql.
-- Drops the partial index and the two columns this migration added.
--
-- lint:allow-destructive dropping secondary_secret discards any in-flight
--   rotation's overlap-window secret. That is the correct behaviour for a
--   rollback: the column did not exist before this migration, so there is
--   no prior state to preserve, and a rotation in progress at rollback time
--   must be re-initiated after the column is re-added.

DROP INDEX IF EXISTS idx_webhook_subscriptions_secondary_secret;

ALTER TABLE webhook_subscriptions
    DROP COLUMN IF EXISTS secondary_secret,
    DROP COLUMN IF EXISTS updated_at;
