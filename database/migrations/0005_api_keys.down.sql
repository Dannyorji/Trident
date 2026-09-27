-- Down migration for 0005_api_keys.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created; api_keys rows created after this migration applied are lost,
--   which is the expected cost of reverting the migration that created the
--   table in the first place.

DROP TABLE IF EXISTS api_keys;
