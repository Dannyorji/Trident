-- Down migration for 0006_audit_log.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created; audit_log rows recorded after this migration applied are lost.

DROP TABLE IF EXISTS audit_log;
