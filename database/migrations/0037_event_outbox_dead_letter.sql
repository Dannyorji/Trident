-- Dead-letter undecodable event_outbox payloads (issue #739).
--
-- The relay used to mark a row it could not decode as published: it pushed the
-- row's seq into `published_seqs`, so `published` became TRUE even though
-- nothing had reached the Redis stream. That hid the failure from the backlog
-- gauge (the row stopped counting as pending), made an undecodable row
-- indistinguishable from a delivered one, and gave operators no way to find
-- out which rows had been dropped.
--
-- Such a payload can never be published, so mark it explicitly instead:
-- `dead_lettered_at` removes it from `fetch_unpublished()` and `backlog()`,
-- `dead_letter_reason` keeps the decode error for audit, and `published`
-- stays FALSE because no delivery happened.
ALTER TABLE event_outbox
    ADD COLUMN IF NOT EXISTS dead_lettered_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dead_letter_reason TEXT;

-- No new index: the relay scans `published = FALSE AND dead_lettered_at IS
-- NULL`, and the existing partial index on `(seq) WHERE published = FALSE`
-- still bounds that scan. Dead letters are expected to be vanishingly rare,
-- so filtering them out of that index's result is cheaper than maintaining a
-- second partial index.
