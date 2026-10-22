DROP INDEX IF EXISTS uq_failed_events_pending;
CREATE UNIQUE INDEX IF NOT EXISTS uq_failed_events_pending
    ON failed_events (contract_id, ledger_sequence, event_index)
    WHERE replayed_at IS NULL;

DROP INDEX IF EXISTS idx_failed_events_network_pending;
DROP INDEX IF EXISTS idx_parse_errors_network_ledger;

ALTER TABLE failed_events DROP CONSTRAINT IF EXISTS chk_failed_events_network;
ALTER TABLE parse_errors DROP CONSTRAINT IF EXISTS chk_parse_errors_network;

ALTER TABLE failed_events DROP COLUMN IF EXISTS network;
ALTER TABLE parse_errors DROP COLUMN IF EXISTS network;
