-- Down migration for 0011_event_outbox.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created; any unpublished rows are lost, so this should only be run once
--   the relay has drained the outbox (or the loss is acceptable).

DROP TABLE IF EXISTS event_outbox;
