-- Down migration for 0008_parse_errors.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created.

DROP TABLE IF EXISTS parse_errors;
