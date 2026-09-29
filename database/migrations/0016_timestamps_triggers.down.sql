-- Down migration for 0016_timestamps_triggers.sql.
-- Drops the triggers and shared trigger function, and the updated_at columns
-- this migration added (system_state's updated_at predates 0016 and is left
-- alone).
--
-- lint:allow-destructive dropping columns/objects added by this migration's
--   own forward half.

DROP TRIGGER IF EXISTS trg_webhook_subscriptions_updated_at ON webhook_subscriptions;
DROP TRIGGER IF EXISTS trg_api_keys_updated_at ON api_keys;
DROP TRIGGER IF EXISTS trg_indexed_contracts_updated_at ON indexed_contracts;
DROP TRIGGER IF EXISTS trg_system_state_updated_at ON system_state;

ALTER TABLE webhook_subscriptions DROP COLUMN IF EXISTS updated_at;
ALTER TABLE api_keys DROP COLUMN IF EXISTS updated_at;
ALTER TABLE indexed_contracts DROP COLUMN IF EXISTS updated_at;
-- system_state.updated_at existed before 0016; only the trigger is this
-- migration's addition, already dropped above.

DROP FUNCTION IF EXISTS set_updated_at();
