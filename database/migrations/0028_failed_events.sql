-- failed_events: dead-letter queue for well-formed events that repeatedly
-- failed to persist (issue #208).
--
-- Distinct from `parse_errors` (migration 0008): a `parse_errors` row is an
-- event that failed to *decode* (malformed XDR — a poison message, retrying
-- never helps). A `failed_events` row is an event that decoded successfully
-- but whose INSERT into `soroban_events` kept failing after bounded retries
-- (e.g. an unexpected constraint violation or a transient failure that
-- outlasted the retry budget). The full normalised event is stored as JSONB
-- so it can be inspected and replayed once the underlying cause is fixed,
-- without needing to re-fetch it from Stellar RPC.
CREATE TABLE IF NOT EXISTS failed_events (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    ledger_sequence  BIGINT      NOT NULL,
    contract_id      TEXT        NOT NULL,
    transaction_hash TEXT        NOT NULL,
    event_index      INT         NOT NULL,
    event_payload    JSONB       NOT NULL,
    error_message    TEXT        NOT NULL,
    attempts         INT         NOT NULL DEFAULT 1,
    occurred_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Set once an operator has replayed this row back into soroban_events.
    -- NULL means "still pending" — the common case a replay tool queries for.
    replayed_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_failed_events_occurred_at ON failed_events (occurred_at DESC);

-- Replay tooling's primary query: "every row nobody has replayed yet",
-- oldest first. Partial so the index stays small as replayed rows accumulate.
CREATE INDEX IF NOT EXISTS idx_failed_events_pending
    ON failed_events (occurred_at)
    WHERE replayed_at IS NULL;
