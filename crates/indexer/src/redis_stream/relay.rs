//! Outbox relay: drains `event_outbox` onto the Redis Stream (issue #200).
//!
//! The poll loop commits events with an unpublished outbox row; this task
//! publishes them. It runs on its own interval and takes a bounded batch per
//! pass so it can never starve the poll loop or monopolise the database.
//!
//! Failure handling is deliberately conservative: a batch stops at the first
//! publish failure and only the rows published *before* it are marked, so
//! ordering is preserved and nothing is lost. The next pass resumes at the
//! failed row. That makes delivery at-least-once — consumers dedupe by event
//! id (see `db::outbox`).
//!
//! The one exception is a payload that cannot be decoded: no retry can fix it,
//! so instead of blocking the queue forever the row is dead-lettered (issue
//! #739). It stays `published = FALSE` — nothing reached Redis — and is
//! excluded from the backlog once marked.

use std::time::Duration;

use sqlx::PgPool;
use tokio_util::sync::CancellationToken;
use trident_common::TridentError;

use crate::db::outbox;
use crate::metrics;
use crate::redis_stream;

/// Tuning for [`OutboxRelay`].
#[derive(Debug, Clone)]
pub struct RelayConfig {
    /// How often the relay scans for unpublished rows.
    pub interval: Duration,
    /// Maximum rows published per pass.
    pub batch_size: i64,
    /// Backlog size above which the relay logs an alert-worthy warning.
    pub backlog_alert_threshold: i64,
    /// Redis stream trim length, mirrors the direct-publish path.
    pub stream_maxlen: u64,
}

pub struct OutboxRelay {
    db: PgPool,
    redis: redis::aio::MultiplexedConnection,
    config: RelayConfig,
}

impl OutboxRelay {
    pub fn new(db: PgPool, redis: redis::aio::MultiplexedConnection, config: RelayConfig) -> Self {
        Self { db, redis, config }
    }

    /// Run until `shutdown` is cancelled, publishing pending events every
    /// `interval`. A failing pass is logged and retried on the next tick rather
    /// than taking the process down: the rows stay unpublished, so nothing is
    /// dropped.
    pub async fn run(&mut self, shutdown: CancellationToken) {
        tracing::info!(
            interval_ms = self.config.interval.as_millis() as u64,
            batch_size = self.config.batch_size,
            "Outbox relay started"
        );

        loop {
            if shutdown.is_cancelled() {
                break;
            }

            match self.publish_pending().await {
                Ok(published) if published > 0 => {
                    tracing::debug!(published, "Outbox batch published");
                }
                Ok(_) => {}
                Err(e) => {
                    tracing::warn!(error = %e, "Outbox relay pass failed, retrying next interval");
                }
            }

            tokio::select! {
                _ = tokio::time::sleep(self.config.interval) => {}
                _ = shutdown.cancelled() => break,
            }
        }

        tracing::info!("Outbox relay stopped");
    }

    /// Publish one bounded batch. Returns how many events reached Redis.
    pub async fn publish_pending(&mut self) -> Result<usize, TridentError> {
        let records = outbox::fetch_unpublished(&self.db, self.config.batch_size).await?;
        if records.is_empty() {
            self.report_backlog(0).await;
            return Ok(0);
        }

        let mut published_seqs: Vec<i64> = Vec::with_capacity(records.len());
        let mut failure: Option<TridentError> = None;

        for record in &records {
            let event = match record.event() {
                Ok(event) => event,
                Err(e) => {
                    // A payload we cannot decode will never publish. Dead-letter
                    // it so it stops blocking the queue without pretending it
                    // reached Redis; the row is retained for audit (issue #739).
                    tracing::error!(
                        seq = record.seq,
                        event_id = %record.event_id,
                        error = %e,
                        "Undecodable outbox payload, dead-lettering"
                    );
                    if let Err(mark_err) =
                        outbox::mark_dead_lettered(&self.db, record.seq, &e.to_string()).await
                    {
                        // Could not record the dead letter: stop the batch so the
                        // next pass retries it, but still mark what was already
                        // delivered (as with a publish failure).
                        failure = Some(mark_err);
                        break;
                    }
                    metrics::record_outbox_undecodable();
                    continue;
                }
            };

            match redis_stream::publish_event(
                &mut self.redis,
                &event,
                self.config.stream_maxlen,
                Some(&record.event_id.to_string()),
            )
            .await
            {
                Ok(()) => {
                    published_seqs.push(record.seq);
                    metrics::record_outbox_published();
                }
                Err(e) => {
                    metrics::record_outbox_publish_failure();
                    failure = Some(e);
                    break;
                }
            }
        }

        // Mark what actually reached Redis, even when the batch aborted early.
        outbox::mark_published(&self.db, &published_seqs).await?;
        self.report_backlog(published_seqs.len() as i64).await;

        match failure {
            Some(e) => Err(e),
            None => Ok(published_seqs.len()),
        }
    }

    /// Publish the current backlog gauge, warning when it grows past the
    /// configured threshold (the alert signal for a stuck relay).
    async fn report_backlog(&self, _published: i64) {
        match outbox::backlog(&self.db).await {
            Ok(backlog) => {
                metrics::set_outbox_backlog(backlog);
                if backlog >= self.config.backlog_alert_threshold {
                    tracing::warn!(
                        backlog,
                        threshold = self.config.backlog_alert_threshold,
                        "Outbox backlog above alert threshold — live subscribers are falling behind"
                    );
                }
            }
            Err(e) => tracing::warn!(error = %e, "Failed to read outbox backlog"),
        }
    }
}
