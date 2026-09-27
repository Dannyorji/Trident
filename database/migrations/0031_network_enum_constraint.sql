-- Migration 0031: DB: store network as a typed enum/constraint and validate on write (issue #252)
-- Enforces network scoping across all relevant tables: mainnet, testnet, futurenet, sandbox.
--
-- lint:allow-no-rollback Step 1's UPDATEs overwrite any 'pubnet' row to
--   'mainnet' and any 'standalone'/'local' row to 'sandbox' across 14 tables,
--   with no column recording which rows were touched or what their original
--   value was. Dropping the CHECK constraints added below would undo the
--   *enforcement* but cannot undo that normalisation: a row that entered this
--   migration as 'pubnet' is indistinguishable, after it runs, from a row
--   that was always 'mainnet'. This codebase is not known to have ever
--   written 'pubnet' or 'standalone' (see the migration's own note: only the
--   indexer config mentions 'pubnet', as an accepted alias), so in most real
--   environments step 1 is a no-op, but the waiver is written against what
--   the migration can do to a database that does carry those values (e.g.
--   one seeded by hand, per the same header note), not against the common
--   case.
--
-- The original version of this migration also renamed the vocabulary
-- (mainnet -> pubnet, standalone -> local) across every table. That rename is
-- deliberately dropped: 16 files in services/ and crates/ write 'mainnet', and
-- only one place mentions 'pubnet' at all (indexer config, as an accepted
-- alias). Migrating the data without changing those writers would make every
-- subsequent INSERT violate the very constraints added below.

-- 1. Normalise any legacy values so the constraints below can be validated.
--    'pubnet' and 'standalone' were never written by this codebase, but a
--    database seeded by hand or by the earlier draft of this migration may
--    carry them.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'soroban_events', 'indexed_contracts', 'api_keys', 'audit_log',
        'webhook_subscriptions', 'token_events', 'contract_invocation_metrics',
        'contract_liveness', 'contract_verification', 'contract_specs',
        'contract_stats_rollup', 'contract_event_schemas', 'token_metadata',
        'contract_storage_snapshots'
    ] LOOP
        EXECUTE format('UPDATE %I SET network = ''mainnet'' WHERE network = ''pubnet''', t);
        EXECUTE format('UPDATE %I SET network = ''sandbox'' WHERE network IN (''standalone'', ''local'')', t);
    END LOOP;
END $$;

-- 2. Column default
-- api_keys.network default left as-is; the codebase writes 'mainnet'.

-- 3. Add CHECK constraints across relevant tables
ALTER TABLE soroban_events
    ADD CONSTRAINT chk_soroban_events_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE indexed_contracts
    ADD CONSTRAINT chk_indexed_contracts_network
    CHECK (network IS NULL OR network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE api_keys
    ADD CONSTRAINT chk_api_keys_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE audit_log
    ADD CONSTRAINT chk_audit_log_network
    CHECK (network IS NULL OR network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE webhook_subscriptions
    ADD CONSTRAINT chk_webhook_subscriptions_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE token_events
    ADD CONSTRAINT chk_token_events_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE contract_invocation_metrics
    ADD CONSTRAINT chk_contract_invocation_metrics_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE contract_liveness
    ADD CONSTRAINT chk_contract_liveness_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE contract_verification
    ADD CONSTRAINT chk_contract_verification_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE contract_specs
    ADD CONSTRAINT chk_contract_specs_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE contract_stats_rollup
    ADD CONSTRAINT chk_contract_stats_rollup_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE contract_event_schemas
    ADD CONSTRAINT chk_contract_event_schemas_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE token_metadata
    ADD CONSTRAINT chk_token_metadata_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));

ALTER TABLE contract_storage_snapshots
    ADD CONSTRAINT chk_contract_storage_snapshots_network
    CHECK (network IN ('mainnet', 'testnet', 'futurenet', 'sandbox'));
