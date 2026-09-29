-- 0027: Add secondary_secret for webhook secret rotation overlap window (issue #452).
--
-- During a rotation, the current primary secret is demoted to secondary_secret
-- and a new secret becomes primary. Deliveries are signed with the new primary
-- but the old secondary signature is also included in X-Trident-Signature
-- (space-separated) so receivers can verify against either key until they have
-- swapped. Once the overlap window expires the secondary is cleared.
--
-- updated_at tracks when the secret was last rotated, which drives the
-- overlap-window expiry cleanup (WEBHOOK_SECRET_OVERLAP_HOURS, default 24h).

ALTER TABLE webhook_subscriptions
    ADD COLUMN IF NOT EXISTS secondary_secret TEXT,
    ADD COLUMN IF NOT EXISTS updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Partial index: only rows that have an active secondary secret, since the
-- cleanup job queries this column to find subscriptions whose overlap window
-- has expired.
CREATE INDEX IF NOT EXISTS idx_webhook_subscriptions_secondary_secret
    ON webhook_subscriptions (updated_at)
    WHERE secondary_secret IS NOT NULL;
