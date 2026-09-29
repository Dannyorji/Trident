-- backfill_jobs: a queue of ledger-range backfills to close gaps found in
-- ledger_metadata (issue #216).
--
-- Nothing previously scanned for holes in the processed range: a transient
-- skip (an aborted poll before the transactional commit_page fix, or a
-- bounded reorg rewind edge from #196) can leave a permanent gap that no
-- code would ever notice or repair. The indexer's periodic gap scan writes
-- rows here rather than invoking crates/backfill in-process, since the two
-- are separate deployables today; a `backfill --from-queue` worker (added in
-- the same change) polls this table, claims a row, and runs the existing
-- range-backfill path against it.
CREATE TABLE IF NOT EXISTS backfill_jobs (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    from_ledger      BIGINT      NOT NULL,
    to_ledger        BIGINT      NOT NULL,
    network          TEXT        NOT NULL,
    -- pending: enqueued, not yet claimed.
    -- running: claimed by a worker (claimed_at set); a worker that dies
    --          mid-job leaves this stuck deliberately -- see
    --          idx_backfill_jobs_stale below -- rather than silently retrying
    --          and potentially double-processing.
    -- done:    the range was backfilled successfully.
    -- failed:  the worker gave up; error holds the reason.
    status           TEXT        NOT NULL DEFAULT 'pending'
                                  CHECK (status IN ('pending', 'running', 'done', 'failed')),
    claimed_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    error            TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_backfill_jobs_range CHECK (to_ledger >= from_ledger)
);

-- lint:allow-long-lock small operational table, not a hot ingest path
CREATE UNIQUE INDEX IF NOT EXISTS uq_backfill_jobs_pending_range
    ON backfill_jobs (network, from_ledger, to_ledger)
    WHERE status IN ('pending', 'running');

-- Worker's claim query: oldest pending job first.
CREATE INDEX IF NOT EXISTS idx_backfill_jobs_pending
    ON backfill_jobs (created_at)
    WHERE status = 'pending';

-- Operator/alerting query: jobs claimed but never completed, i.e. a worker
-- that died mid-run. Left for a human to decide whether to re-enqueue rather
-- than auto-retried, since re-running an interrupted backfill blind could
-- double-count if the interruption happened mid-insert of a page that
-- itself wasn't atomic (crates/backfill is not transactional per-page the
-- way the indexer's commit_page is).
CREATE INDEX IF NOT EXISTS idx_backfill_jobs_stale
    ON backfill_jobs (claimed_at)
    WHERE status = 'running';
