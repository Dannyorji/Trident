-- Down migration for 0022_token_metadata.sql.
--
-- lint:allow-destructive dropping the table this migration's forward half
--   created. Note this migration was renumbered from an earlier 0018 (#371);
--   the down file follows the current, live version number (0022).

DROP TABLE IF EXISTS token_metadata;
