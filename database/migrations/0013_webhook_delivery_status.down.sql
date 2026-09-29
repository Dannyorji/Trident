-- Down migration for 0013_webhook_delivery_status.sql.
-- Drops the status/attempts columns and the dead-lettered partial index.
--
-- lint:allow-destructive dropping columns added by this migration's own
--   forward half.

DROP INDEX IF EXISTS idx_webhook_deliveries_dead_lettered;

ALTER TABLE webhook_deliveries
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS attempts;
