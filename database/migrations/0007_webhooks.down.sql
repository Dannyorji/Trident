-- Down migration for 0007_webhooks.sql.
--
-- lint:allow-destructive dropping the tables this migration's forward half
--   created. webhook_deliveries is dropped first since it has a FK onto
--   webhook_subscriptions.

DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_subscriptions;
