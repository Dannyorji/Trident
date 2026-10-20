-- Down migration for 0037_event_outbox_dead_letter.sql.
--
-- Dropping the marker columns discards the record of which payloads were
-- undecodable and why. The rows themselves are untouched — they were never
-- published, and without the marker they simply become pending again, which is
-- the pre-0037 behaviour.
ALTER TABLE IF EXISTS event_outbox
    DROP COLUMN IF EXISTS dead_letter_reason,
    DROP COLUMN IF EXISTS dead_lettered_at;
