-- Down migration for 0001_init.sql.
-- Drops the four tables created by the initial schema, in reverse dependency
-- order. None of these tables have foreign keys pointing at them from within
-- this migration, but later migrations (0007 webhooks, 0010 token_events,
-- etc.) add tables that reference soroban_events; this down migration only
-- undoes 0001 itself; reverting a later migration first is the caller's
-- responsibility, same as with any other migration in this set.

DROP TABLE IF EXISTS ledger_metadata;
DROP TABLE IF EXISTS indexed_contracts;
DROP TABLE IF EXISTS system_state;
DROP TABLE IF EXISTS soroban_events;

-- pgcrypto is left installed: other extensions/objects in the database may
-- depend on it, and dropping an extension is outside the blast radius of
-- reverting this migration's own table creations.
