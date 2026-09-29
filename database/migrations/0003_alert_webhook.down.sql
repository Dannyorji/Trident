-- Down migration for 0003_alert_webhook.sql.
-- Drops the alerting state columns added to system_state.

ALTER TABLE system_state
    DROP COLUMN IF EXISTS last_alert_at,
    DROP COLUMN IF EXISTS alert_fired;
