use sqlx::PgPool;
use trident_common::{SorobanEvent, TridentError};
use uuid::Uuid;

const EVENT_NS: Uuid = Uuid::NAMESPACE_DNS;

/// Must stay identical to `crates/indexer/src/db/mod.rs`'s `event_uuid`: a
/// backfilled event and a live-indexed event for the same
/// (transaction_hash, raw_event_index) must produce the same id, or
/// `ON CONFLICT DO NOTHING` cannot tell a backfill replay from a genuinely
/// new row (issue #599).
fn event_uuid(transaction_hash: &str, raw_event_index: u32) -> Uuid {
    let key = format!("{transaction_hash}:{raw_event_index}");
    Uuid::new_v5(&EVENT_NS, key.as_bytes())
}

/// One ledger range to backfill, claimed from `backfill_jobs` (issue #216).
/// `backfill_jobs` is created by `crates/indexer`'s gap scan (migration
/// 0029); this crate is the worker that consumes it.
#[derive(Debug, Clone)]
pub struct BackfillJob {
    pub id: Uuid,
    pub from_ledger: u64,
    pub to_ledger: u64,
    pub network: String,
}

/// Atomically claim the oldest pending job and mark it `running`
/// (issue #216). `FOR UPDATE SKIP LOCKED` lets multiple `--from-queue`
/// workers run concurrently against the same table without two workers
/// claiming the same job or blocking on each other's row lock.
pub async fn claim_next_job(pool: &PgPool) -> Result<Option<BackfillJob>, TridentError> {
    let mut tx = pool.begin().await.map_err(|e| {
        TridentError::storage(anyhow::Error::new(e).context("claim_next_job begin"))
    })?;

    let row: Option<(Uuid, i64, i64, String)> = sqlx::query_as(
        r#"
        SELECT id, from_ledger, to_ledger, network
        FROM backfill_jobs
        WHERE status = 'pending'
        ORDER BY created_at
        LIMIT 1
        FOR UPDATE SKIP LOCKED
        "#,
    )
    .fetch_optional(&mut *tx)
    .await
    .map_err(|e| TridentError::storage(anyhow::Error::new(e).context("claim_next_job select")))?;

    let Some((id, from_ledger, to_ledger, network)) = row else {
        tx.rollback().await.ok();
        return Ok(None);
    };

    sqlx::query("UPDATE backfill_jobs SET status = 'running', claimed_at = NOW() WHERE id = $1")
        .bind(id)
        .execute(&mut *tx)
        .await
        .map_err(|e| {
            TridentError::storage(anyhow::Error::new(e).context("claim_next_job update"))
        })?;

    tx.commit().await.map_err(|e| {
        TridentError::storage(anyhow::Error::new(e).context("claim_next_job commit"))
    })?;

    Ok(Some(BackfillJob {
        id,
        from_ledger: from_ledger as u64,
        to_ledger: to_ledger as u64,
        network,
    }))
}

/// Mark a claimed job `done` (issue #216).
pub async fn complete_job(pool: &PgPool, id: Uuid) -> Result<(), TridentError> {
    sqlx::query("UPDATE backfill_jobs SET status = 'done', completed_at = NOW() WHERE id = $1")
        .bind(id)
        .execute(pool)
        .await
        .map_err(|e| TridentError::storage(anyhow::Error::new(e).context("complete_job")))?;
    Ok(())
}

/// Mark a claimed job `failed` with a reason (issue #216). Left for an
/// operator to inspect and re-enqueue rather than auto-retried: a partially
/// applied range is not necessarily safe to blindly re-run (see the
/// `idx_backfill_jobs_stale` comment in migration 0032).
pub async fn fail_job(pool: &PgPool, id: Uuid, error: &str) -> Result<(), TridentError> {
    sqlx::query(
        "UPDATE backfill_jobs SET status = 'failed', completed_at = NOW(), error = $2 WHERE id = $1",
    )
    .bind(id)
    .bind(error)
    .execute(pool)
    .await
    .map_err(|e| TridentError::storage(anyhow::Error::new(e).context("fail_job")))?;
    Ok(())
}

/// Insert a backfilled event.
///
/// Duplicate handling uses two complementary strategies:
/// - **Primary**: `ON CONFLICT (ledger_sequence, id) DO NOTHING` deduplicates
///   replays, because `id` is a deterministic UUIDv5 of
///   `(contract_id, ledger_sequence, event_index)`. The conflict target must
///   include `ledger_sequence`: `soroban_events` is RANGE-partitioned on it
///   (migration 0017), so the partition key is part of every unique index.
/// - **Safety net**: `UNIQUE (transaction_hash, event_index, network)` at the
///   DB layer (migration 0025) catches any case where the same protocol event
///   would be inserted under a different derived `id`.
///
/// `network` must match the value used in `indexed_contracts` for this
/// deployment (e.g. `"mainnet"` or `"testnet"`); the natural-key constraint is
/// network-scoped because the same transaction hash can legitimately appear on
/// more than one network.
pub async fn insert_event(
    pool: &PgPool,
    event: &SorobanEvent,
    network: &str,
) -> Result<(), TridentError> {
    let id = event_uuid(&event.transaction_hash, event.raw_event_index);
    let event_type = match event.event_type {
        trident_common::EventType::Contract => "contract",
        trident_common::EventType::System => "system",
        trident_common::EventType::Diagnostic => "diagnostic",
    };
    let topics = serde_json::to_value(&event.topics)
        .map_err(|e| TridentError::storage(anyhow::Error::new(e).context("topics serialise")))?;
    let ledger_ts: chrono::DateTime<chrono::Utc> = event.ledger_timestamp.parse().map_err(|e| {
        TridentError::storage(anyhow::Error::new(e).context("ledger timestamp parse"))
    })?;

    sqlx::query(
        r#"
        INSERT INTO soroban_events
            (id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash,
             event_index, event_type, topics, data, network)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
        ON CONFLICT (ledger_sequence, id) DO NOTHING
        "#,
    )
    .bind(id)
    .bind(&event.contract_id)
    .bind(event.ledger_sequence as i64)
    .bind(ledger_ts)
    .bind(&event.transaction_hash)
    .bind(event.event_index as i32)
    .bind(event_type)
    .bind(&topics)
    .bind(&event.data)
    .bind(network)
    .execute(pool)
    .await
    .map_err(|e| TridentError::storage(anyhow::Error::new(e).context("insert_event")))?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn connect_test_db() -> Option<PgPool> {
        let url = match std::env::var("TEST_DATABASE_URL") {
            Ok(url) => url,
            Err(_) if std::env::var("REQUIRE_TEST_SERVICES").is_ok() => {
                panic!("TEST_DATABASE_URL must be set when REQUIRE_TEST_SERVICES is set");
            }
            Err(_) => {
                eprintln!("SKIP: TEST_DATABASE_URL not set");
                return None;
            }
        };
        Some(PgPool::connect(&url).await.unwrap())
    }

    /// A claimed job must move pending -> running, and a second claim call
    /// must not see it again (issue #216).
    #[tokio::test]
    async fn claim_next_job_claims_oldest_pending_and_marks_running() {
        let Some(pool) = connect_test_db().await else {
            return;
        };

        let network = format!("backfilltest-{}", Uuid::new_v4());
        sqlx::query(
            "INSERT INTO backfill_jobs (from_ledger, to_ledger, network) VALUES ($1, $2, $3)",
        )
        .bind(1_000_i64)
        .bind(1_010_i64)
        .bind(&network)
        .execute(&pool)
        .await
        .unwrap();

        let job = claim_next_job(&pool)
            .await
            .unwrap()
            .expect("a pending job must be claimable");
        assert_eq!(job.from_ledger, 1_000);
        assert_eq!(job.to_ledger, 1_010);
        assert_eq!(job.network, network);

        let status: String = sqlx::query_scalar("SELECT status FROM backfill_jobs WHERE id = $1")
            .bind(job.id)
            .fetch_one(&pool)
            .await
            .unwrap();
        assert_eq!(status, "running");

        // The same job must not be claimable again while running.
        let next = claim_next_job(&pool).await.unwrap();
        assert!(next.is_none(), "a running job must not be claimed twice");

        sqlx::query("DELETE FROM backfill_jobs WHERE network = $1")
            .bind(&network)
            .execute(&pool)
            .await
            .unwrap();
    }

    /// complete_job and fail_job must set the expected terminal status
    /// (issue #216).
    #[tokio::test]
    async fn complete_and_fail_job_set_terminal_status() {
        let Some(pool) = connect_test_db().await else {
            return;
        };

        let network = format!("backfilltest-{}", Uuid::new_v4());
        sqlx::query(
            "INSERT INTO backfill_jobs (from_ledger, to_ledger, network) VALUES ($1, $2, $3)",
        )
        .bind(2_000_i64)
        .bind(2_010_i64)
        .bind(&network)
        .execute(&pool)
        .await
        .unwrap();
        let done_id: Uuid = sqlx::query_scalar(
            "SELECT id FROM backfill_jobs WHERE network = $1 AND from_ledger = 2000",
        )
        .bind(&network)
        .fetch_one(&pool)
        .await
        .unwrap();

        sqlx::query(
            "INSERT INTO backfill_jobs (from_ledger, to_ledger, network) VALUES ($1, $2, $3)",
        )
        .bind(3_000_i64)
        .bind(3_010_i64)
        .bind(&network)
        .execute(&pool)
        .await
        .unwrap();
        let failed_id: Uuid = sqlx::query_scalar(
            "SELECT id FROM backfill_jobs WHERE network = $1 AND from_ledger = 3000",
        )
        .bind(&network)
        .fetch_one(&pool)
        .await
        .unwrap();

        complete_job(&pool, done_id).await.unwrap();
        fail_job(&pool, failed_id, "rpc timeout").await.unwrap();

        let (done_status, done_completed): (String, Option<chrono::DateTime<chrono::Utc>>) =
            sqlx::query_as("SELECT status, completed_at FROM backfill_jobs WHERE id = $1")
                .bind(done_id)
                .fetch_one(&pool)
                .await
                .unwrap();
        assert_eq!(done_status, "done");
        assert!(done_completed.is_some());

        let (failed_status, failed_error): (String, Option<String>) =
            sqlx::query_as("SELECT status, error FROM backfill_jobs WHERE id = $1")
                .bind(failed_id)
                .fetch_one(&pool)
                .await
                .unwrap();
        assert_eq!(failed_status, "failed");
        assert_eq!(failed_error.as_deref(), Some("rpc timeout"));

        sqlx::query("DELETE FROM backfill_jobs WHERE network = $1")
            .bind(&network)
            .execute(&pool)
            .await
            .unwrap();
    }
}
