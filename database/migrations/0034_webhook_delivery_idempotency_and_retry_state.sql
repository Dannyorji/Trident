-- Migration 0034: webhook_deliveries idempotency key + persisted retry state
-- (issues #649, #651)
--
-- #649: delivery and its database record are two independent steps with no
-- idempotency key, so a crash between the HTTP call and the INSERT leaves
-- bookkeeping out of sync, and nothing stops the same event/subscription/
-- attempt from being recorded twice on replay. A unique constraint on
-- (subscription_id, event_id, attempt) rejects a duplicate insert at the
-- database level instead of silently accepting it.
--
-- #651: retry backoff lives only in an in-process goroutine sleep. A restart
-- mid-backoff drops the retry with no record of it ever having been pending.
-- next_attempt_at persists when the next attempt is due so a restarted
-- worker can find and resume deliveries that were mid-backoff.

ALTER TABLE webhook_deliveries
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;

CREATE UNIQUE INDEX IF NOT EXISTS uq_webhook_deliveries_idempotency
    ON webhook_deliveries (subscription_id, event_id, attempt);

-- Query path for the restart-resume worker: find deliveries still owed a
-- retry (status = 'failed', not yet dead-lettered) whose backoff has
-- elapsed.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_pending_retry
    ON webhook_deliveries (next_attempt_at)
    WHERE status = 'failed' AND next_attempt_at IS NOT NULL;
