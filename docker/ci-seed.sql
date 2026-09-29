-- CI smoke-test seed data.
-- Inserts a pre-computed test API key so the smoke test can authenticate
-- against GET /v1/events without needing to call POST /v1/api-keys first.
--
-- Raw key    : ci-test-api-key-do-not-use-in-production
-- Key prefix : ci-test-api-ke
-- key_hash is the SHA-256 hex of the raw key (NOT HMAC — the DB stores a
-- plain SHA-256 digest used by the admin handler for lookup display only;
-- the auth middleware uses HMAC-SHA256 keyed on API_KEY_SALT, which is
-- pre-computed and stored in .env.ci as API_KEY_HASHES).
--
-- DO NOT use this key or this salt in any environment other than CI.

INSERT INTO api_keys (id, key_hash, key_prefix, label, network, rate_limit_tier)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    encode(sha256('ci-test-api-key-do-not-use-in-production'::bytea), 'hex'),
    'ci-test-api-ke',
    'CI smoke-test key',
    'testnet',
    'standard'
)
ON CONFLICT DO NOTHING;

-- Stable fixtures used by the live OpenAPI contract suite. Keeping these in
-- the same initdb seed as the API key means CI exercises the real HTTP ->
-- gRPC -> Postgres path instead of replacing dependencies with mocks.
INSERT INTO soroban_events (
    id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash,
    event_index, event_type, network, topics, data, created_at
) VALUES (
    '550e8400-e29b-41d4-a716-446655440000',
    'CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ',
    1000,
    '2026-01-01T00:00:00Z',
    'ci-contract-test-transaction',
    0,
    'contract',
    'testnet',
    '["transfer"]'::jsonb,
    '"ci-contract-test-data"'::jsonb,
    '2026-01-01T00:00:00Z'
)
ON CONFLICT DO NOTHING;

INSERT INTO contract_specs (
    contract_id, network, code_hash, has_spec, functions,
    contract_type, interfaces
) VALUES (
    'CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ',
    'testnet',
    'ci-contract-test-code-hash',
    TRUE,
    '[{"name":"transfer"}]'::jsonb,
    'token',
    '["SEP-41"]'::jsonb
)
ON CONFLICT (contract_id, network) DO NOTHING;

INSERT INTO contract_storage_snapshots (
    contract_id, network, storage_key, key_json, value_json, ledger_sequence,
    created_at
) VALUES (
    'CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ',
    'testnet',
    'ci-storage-key',
    '"balance"'::jsonb,
    '100'::jsonb,
    1000,
    '2026-01-01T00:00:00Z'
)
ON CONFLICT (contract_id, network, storage_key, ledger_sequence) DO NOTHING;

-- Per-network cursor key (issue #600); CI runs the indexer against testnet.
UPDATE system_state
SET last_ledger_indexed = 1000,
    events_indexed_total = 1,
    events_in_last_poll = 1,
    poll_duration_ms = 1,
    last_poll_at = NOW(),
    updated_at = NOW()
WHERE key = 'latest_ledger_cursor:testnet';
