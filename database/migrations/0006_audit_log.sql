-- lint:allow-long-lock Historical migration, already applied. audit_log is
-- created empty in this same file, so these index builds lock nothing.
--
-- Audit log for API key usage (issue #139 / #162)
-- Records every /v1/* request with metadata for security auditing and billing.

CREATE TABLE IF NOT EXISTS audit_log (
    id           BIGSERIAL   PRIMARY KEY,
    api_key_id   UUID        REFERENCES api_keys(id) ON DELETE SET NULL,
    endpoint     TEXT        NOT NULL,
    method       TEXT        NOT NULL,
    ip           INET,
    user_agent   TEXT,
    status_code  INT         NOT NULL,
    duration_ms  INT         NOT NULL,
    result_count INT,                    -- for list endpoints: number of items returned
    request_id   TEXT        NOT NULL,
    network      TEXT,
    ts           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index for key-centric queries (billing, per-key analytics)
CREATE INDEX IF NOT EXISTS idx_audit_log_key_ts ON audit_log (api_key_id, ts DESC);

-- Index for time-range queries (cleanup jobs, global analytics)
CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON audit_log (ts DESC);