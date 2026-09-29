-- Down migration for 0010_token_events.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created.

DROP TABLE IF EXISTS token_events;
