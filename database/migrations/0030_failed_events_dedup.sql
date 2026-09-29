-- failed_events dedup + pending-uniqueness (issue #508, completing #208).
--
-- lint:allow-no-rollback The DELETE below permanently removes every
--   pending duplicate row except the newest one per (contract_id,
--   ledger_sequence, event_index): each deleted row's own occurred_at,
--   error_message and individual attempts count is gone (the survivor's
--   attempts is overwritten with the summed total, which is a merge, not a
--   record of the originals). There is no reverse migration that can
--   recreate rows whose contents were never preserved anywhere. A
--   .down.sql could still drop uq_failed_events_pending and stop there, but
--   that would silently misrepresent "rollback" as restoring pre-migration
--   state when the data loss from the DELETE is unconditional and already
--   done by the time any revert could run.
--
-- 0028 introduced the persist dead-letter queue but every exhausted retry
-- burst INSERTs a fresh row, so one poison event re-encountered across polls
-- (an RPC redelivery, a backfill overlap) accumulates duplicates and the
-- queue's row count stops meaning "number of distinct poisoned events" —
-- which is exactly the number the backlog gauge and its alert report.
--
-- A row's natural key is (contract_id, ledger_sequence, event_index) — the
-- same triple 0025 established as soroban_events' natural key and
-- event_uuid derives the deterministic id from. Uniqueness is
-- enforced only over PENDING rows (replayed_at IS NULL): a row an operator
-- has already replayed is history and must not block recording a fresh
-- failure of the same event if it ever fails again.

-- Collapse existing pending duplicates before the index can exist: keep the
-- newest row per key and fold the attempt counts into it, so no evidence of
-- how often the event failed is lost.
WITH ranked AS (
    SELECT id,
           SUM(attempts) OVER (PARTITION BY contract_id, ledger_sequence, event_index) AS total_attempts,
           ROW_NUMBER() OVER (
               PARTITION BY contract_id, ledger_sequence, event_index
               ORDER BY occurred_at DESC, id
           ) AS rn
    FROM failed_events
    WHERE replayed_at IS NULL
)
UPDATE failed_events f
SET attempts = ranked.total_attempts
FROM ranked
WHERE f.id = ranked.id AND ranked.rn = 1;

DELETE FROM failed_events f
USING (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY contract_id, ledger_sequence, event_index
               ORDER BY occurred_at DESC, id
           ) AS rn
    FROM failed_events
    WHERE replayed_at IS NULL
) ranked
WHERE f.id = ranked.id AND ranked.rn > 1;

CREATE UNIQUE INDEX IF NOT EXISTS uq_failed_events_pending
    ON failed_events (contract_id, ledger_sequence, event_index)
    WHERE replayed_at IS NULL;
