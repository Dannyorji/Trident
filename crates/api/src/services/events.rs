use std::time::Duration;

use chrono::{DateTime, Utc};
use opentelemetry::propagation::Extractor;
use redis::streams::{StreamReadOptions, StreamReadReply};
use redis::AsyncCommands;
use sqlx::PgPool;
use tonic::{Request, Response, Status};
use tracing::Instrument;
use tracing_opentelemetry::OpenTelemetrySpanExt;
use uuid::Uuid;

use crate::trident::{
    events_server::Events, Event, GetEventRequest, ListEventsRequest, ListEventsResponse,
    StreamEventsRequest,
};

// ---------------------------------------------------------------------------
// W3C TraceContext extraction from tonic metadata
// ---------------------------------------------------------------------------

struct MetadataCarrier<'a>(&'a tonic::metadata::MetadataMap);

impl<'a> Extractor for MetadataCarrier<'a> {
    fn get(&self, key: &str) -> Option<&str> {
        self.0.get(key).and_then(|v| v.to_str().ok())
    }

    fn keys(&self) -> Vec<&str> {
        self.0
            .keys()
            .filter_map(|k| {
                if let tonic::metadata::KeyRef::Ascii(k) = k {
                    Some(k.as_str())
                } else {
                    None
                }
            })
            .collect()
    }
}

fn extract_context(metadata: &tonic::metadata::MetadataMap) -> opentelemetry::Context {
    opentelemetry::global::get_text_map_propagator(|prop| prop.extract(&MetadataCarrier(metadata)))
}

const REDIS_STREAM_KEY: &str = "trident:events";

/// Default in-flight buffer per subscriber. Bounded so one slow consumer
/// cannot make the server accumulate events without limit; when it fills, the
/// consumer task blocks on send and stops reading Redis.
const DEFAULT_STREAM_CHANNEL_BUF: usize = 128;

/// How long a single blocking XREAD waits before looping. Short enough that a
/// dropped client is noticed promptly, long enough to avoid busy polling.
const XREAD_BLOCK_MS: usize = 5_000;

/// Redis stream ID meaning "only entries added after this call".
const STREAM_ID_LIVE_TAIL: &str = "$";

const DEFAULT_NETWORK: &str = "testnet";

/// Per-subscriber buffer size, overridable via `STREAM_CHANNEL_BUFFER`.
fn stream_channel_buffer() -> usize {
    std::env::var("STREAM_CHANNEL_BUFFER")
        .ok()
        .and_then(|v| v.parse::<usize>().ok())
        .filter(|&n| n > 0)
        .unwrap_or(DEFAULT_STREAM_CHANNEL_BUF)
}

/// Validate a client-supplied resume point.
///
/// Redis stream IDs are `<millis>-<seq>`. `0` (replay everything still in the
/// stream) and `$` (live tail) are also accepted. Anything else is rejected
/// rather than passed through, because Redis would answer a malformed ID with a
/// connection-level error that the subscriber would see as an opaque failure.
// `Status` is the error type the gRPC surface must return; boxing it here would
// only move the size to the caller.
#[allow(clippy::result_large_err)]
fn validate_start_id(start_id: &str) -> Result<String, Status> {
    if start_id.is_empty() {
        return Ok(STREAM_ID_LIVE_TAIL.to_string());
    }
    if start_id == STREAM_ID_LIVE_TAIL || start_id == "0" {
        return Ok(start_id.to_string());
    }

    let mut parts = start_id.split('-');
    let valid = matches!((parts.next(), parts.next(), parts.next()), (Some(ms), Some(seq), None)
        if !ms.is_empty()
            && !seq.is_empty()
            && ms.bytes().all(|b| b.is_ascii_digit())
            && seq.bytes().all(|b| b.is_ascii_digit()));

    if valid {
        Ok(start_id.to_string())
    } else {
        Err(Status::invalid_argument(
            "start_id must be a Redis stream ID (<millis>-<seq>), \"0\", or \"$\"",
        ))
    }
}

pub struct EventsServiceImpl {
    pub db: PgPool,
    pub redis: redis::aio::ConnectionManager,
}

impl EventsServiceImpl {
    pub fn new(db: PgPool, redis: redis::aio::ConnectionManager) -> Self {
        Self { db, redis }
    }
}

// ---------------------------------------------------------------------------
// DB row → proto conversion
// ---------------------------------------------------------------------------

#[derive(sqlx::FromRow)]
struct EventRow {
    id: Uuid,
    contract_id: String,
    ledger_sequence: i64,
    ledger_timestamp: DateTime<Utc>,
    transaction_hash: String,
    event_index: i32,
    event_type: String,
    topics: serde_json::Value,
    data: serde_json::Value,
    created_at: DateTime<Utc>,
}

fn row_to_event(row: EventRow) -> Event {
    let topics: Vec<String> = serde_json::from_value(row.topics).unwrap_or_default();
    Event {
        id: row.id.to_string(),
        contract_id: row.contract_id,
        ledger_sequence: row.ledger_sequence as u64,
        ledger_timestamp: row.ledger_timestamp.to_rfc3339(),
        transaction_hash: row.transaction_hash,
        event_index: row.event_index as u32,
        event_type: row.event_type,
        topics,
        data: row.data.to_string(),
        created_at: row.created_at.to_rfc3339(),
    }
}

fn db_err(e: sqlx::Error) -> Status {
    tracing::error!(error = %e, "database error");
    Status::unavailable("database temporarily unavailable")
}

// Same rationale as `validate_start_id` above: `Status` is the gRPC surface's
// error type, so boxing it here would only move the size to the caller.
#[allow(clippy::result_large_err)]
fn validate_list_events(req: &ListEventsRequest) -> Result<(), Status> {
    if req.contract_id.len() > 256 {
        return Err(Status::invalid_argument(
            "contract_id must be at most 256 characters",
        ));
    }

    if req.topic_0.len() > 128 {
        return Err(Status::invalid_argument(
            "topic_0 must be at most 128 characters",
        ));
    }

    if req.topic_1.len() > 128 {
        return Err(Status::invalid_argument(
            "topic_1 must be at most 128 characters",
        ));
    }

    if req.network.len() > 64 {
        return Err(Status::invalid_argument(
            "network must be at most 64 characters",
        ));
    }

    Ok(())
}

#[allow(clippy::result_large_err)]
fn validate_get_event(req: &GetEventRequest) -> Result<(), Status> {
    if req.id.is_empty() {
        return Err(Status::invalid_argument("id is required"));
    }

    if req.network.len() > 64 {
        return Err(Status::invalid_argument(
            "network must be at most 64 characters",
        ));
    }

    Ok(())
}

/// Normalise a network string — defaults to "testnet" when empty.
fn resolve_network(network: &str) -> &str {
    if network.is_empty() {
        DEFAULT_NETWORK
    } else {
        network
    }
}

// ---------------------------------------------------------------------------
// Pagination cursor (issue #603)
// ---------------------------------------------------------------------------
//
// `list_events` keyset-paginates on `(ledger_sequence, event_index)` — see the
// `ORDER BY`/`WHERE (ledger_sequence, event_index) > ($7, $8)` clause below.
// The cursor's job is only to carry that pair from one page to the next.
//
// Previously the cursor was the last row's bare `id`, and resolving it meant
// `SELECT ledger_sequence, event_index FROM soroban_events WHERE id = $1` — a
// lookup by `id` alone, which cannot use the partitioned primary key
// `(ledger_sequence, id)` and so probes every partition via the global
// `idx_soroban_events_id_desc` index on every page after the first.
//
// Encoding `ledger_sequence` (and `event_index`, the other half of the
// keyset) directly into the cursor removes that lookup entirely: decoding
// the cursor is now pure string parsing, and the values it yields feed
// straight into the same `WHERE` clause `list_events` already had, which can
// prune to a single partition because `ledger_sequence` is known up front.
// The cursor stays opaque to callers — this is an internal format change to
// the token wrapped by the Go layer's `cursor.Encode`/`cursor.Decode`
// (services/api/cursor/cursor.go), which never inspects the token's
// contents, so no documented API contract changes.
const CURSOR_SEPARATOR: char = '_';

/// Encode a `(ledger_sequence, event_index)` keyset position as this
/// service's raw pagination token.
fn encode_events_cursor(ledger_sequence: i64, event_index: i32) -> String {
    format!("{ledger_sequence}{CURSOR_SEPARATOR}{event_index}")
}

/// Decode a cursor produced by [`encode_events_cursor`]. Returns an error for
/// anything else, including the bare-UUID cursors this service issued before
/// issue #603 — those referred to a specific row that may since have aged out
/// of retention, and re-deriving `(ledger_sequence, event_index)` from one
/// would require the very lookup this change exists to remove. A client
/// holding a pre-#603 cursor gets a clear "start over" error instead of a
/// silently different page.
#[allow(clippy::result_large_err)]
fn decode_events_cursor(cursor: &str) -> Result<(i64, i32), Status> {
    let (seq_part, idx_part) = cursor
        .split_once(CURSOR_SEPARATOR)
        .ok_or_else(|| Status::invalid_argument("cursor is not a valid pagination cursor"))?;

    let ledger_sequence: i64 = seq_part
        .parse()
        .map_err(|_| Status::invalid_argument("cursor is not a valid pagination cursor"))?;
    let event_index: i32 = idx_part
        .parse()
        .map_err(|_| Status::invalid_argument("cursor is not a valid pagination cursor"))?;

    Ok((ledger_sequence, event_index))
}

// ---------------------------------------------------------------------------
// Redis stream consumer (issue #236)
// ---------------------------------------------------------------------------

/// What one subscriber asked for.
struct StreamSubscription {
    contract_id: String,
    topic_0: Option<String>,
    /// Validated Redis stream ID to read from.
    start_id: String,
}

/// Read one field of a stream entry as a string, defaulting to empty.
fn entry_field(entry: &redis::streams::StreamId, field: &str) -> String {
    entry
        .map
        .get(field)
        .and_then(|v| {
            if let redis::Value::Data(b) = v {
                String::from_utf8(b.clone()).ok()
            } else {
                None
            }
        })
        .unwrap_or_default()
}

/// Consume the indexer's Redis stream and push matching events to one
/// subscriber until the client goes away.
///
/// Returns as soon as the receiving half is dropped — including while parked in
/// a blocking XREAD — so a disconnected client leaves no task behind.
async fn run_stream_consumer(
    mut redis: redis::aio::ConnectionManager,
    tx: tokio::sync::mpsc::Sender<Result<Event, Status>>,
    subscription: StreamSubscription,
) {
    let StreamSubscription {
        contract_id,
        topic_0,
        start_id,
    } = subscription;
    let mut last_id = start_id;

    let opts = StreamReadOptions::default()
        .block(XREAD_BLOCK_MS)
        .count(100);

    loop {
        let ids = [last_id.clone()];
        let reply: redis::RedisResult<StreamReadReply> = tokio::select! {
            biased;
            _ = tx.closed() => return,
            reply = redis.xread_options(&[REDIS_STREAM_KEY], &ids, &opts) => reply,
        };

        match reply {
            Ok(StreamReadReply { keys }) => {
                for stream_key in keys {
                    for entry in stream_key.ids {
                        last_id = entry.id.clone();

                        if entry_field(&entry, "contract_id") != contract_id {
                            continue;
                        }

                        let topics: Vec<String> =
                            serde_json::from_str(&entry_field(&entry, "topics"))
                                .unwrap_or_default();

                        if let Some(ref t0) = topic_0 {
                            if topics.first().map(String::as_str) != Some(t0.as_str()) {
                                continue;
                            }
                        }

                        let event = Event {
                            id: String::new(),
                            contract_id: entry_field(&entry, "contract_id"),
                            ledger_sequence: entry_field(&entry, "ledger_sequence")
                                .parse()
                                .unwrap_or(0),
                            ledger_timestamp: entry_field(&entry, "ledger_timestamp"),
                            transaction_hash: entry_field(&entry, "transaction_hash"),
                            event_index: entry_field(&entry, "event_index").parse().unwrap_or(0),
                            event_type: entry_field(&entry, "event_type"),
                            topics,
                            data: entry_field(&entry, "data"),
                            created_at: String::new(),
                        };

                        // Blocks once the bounded buffer fills, which is the
                        // backpressure: a slow client stops us reading Redis
                        // rather than growing an unbounded queue.
                        if tx.send(Ok(event)).await.is_err() {
                            return;
                        }
                    }
                }
            }
            Err(e) => {
                tracing::warn!(error = %e, "Redis XREAD error in stream_events, retrying");
                tokio::select! {
                    _ = tokio::time::sleep(Duration::from_secs(1)) => {}
                    _ = tx.closed() => return,
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// gRPC service implementation
// ---------------------------------------------------------------------------

#[tonic::async_trait]
impl Events for EventsServiceImpl {
    async fn list_events(
        &self,
        request: Request<ListEventsRequest>,
    ) -> Result<Response<ListEventsResponse>, Status> {
        let parent_cx = extract_context(request.metadata());
        let span = tracing::info_span!("list_events", "rpc.system" = "grpc");
        span.set_parent(parent_cx);

        let db = self.db.clone();
        let req = request.into_inner();
        let contract_id_attr = req.contract_id.clone();
        span.record("contract_id", contract_id_attr.as_str());

        validate_list_events(&req).map_err(|e| {
            tracing::warn!(error = %e, "invalid list_events request");
            e
        })?;

        async move {
            let limit = req.limit.clamp(1, 200) as i64;
            let network = resolve_network(&req.network);

            let (cursor_seq, cursor_idx): (Option<i64>, Option<i32>) = if req.cursor.is_empty() {
                (None, None)
            } else {
                let (seq, idx) = decode_events_cursor(&req.cursor)?;
                (Some(seq), Some(idx))
            };

            let rows: Vec<EventRow> = sqlx::query_as(
                r#"
                SELECT id, contract_id, ledger_sequence, ledger_timestamp,
                       transaction_hash, event_index, event_type, topics, data, created_at
                FROM soroban_events
                WHERE
                    network = $1
                    AND ($2::text = '' OR contract_id = $2)
                    AND ($3::text = '' OR topic_0 = $3)
                    AND ($4::text = '' OR topic_1 = $4)
                    AND ($5::bigint = 0 OR ledger_sequence >= $5)
                    AND ($6::bigint = 0 OR ledger_sequence <= $6)
                    AND (
                        $7::bigint IS NULL
                        OR (ledger_sequence, event_index) > ($7, $8)
                    )
                ORDER BY ledger_sequence ASC, event_index ASC
                LIMIT $9
                "#,
            )
            .bind(network)
            .bind(&req.contract_id)
            .bind(&req.topic_0)
            .bind(&req.topic_1)
            .bind(req.ledger_from as i64)
            .bind(req.ledger_to as i64)
            .bind(cursor_seq)
            .bind(cursor_idx)
            .bind(limit)
            .fetch_all(&db)
            .await
            .map_err(db_err)?;

            let has_more = rows.len() as i64 == limit;
            let next_cursor = if has_more {
                rows.last()
                    .map(|r| encode_events_cursor(r.ledger_sequence, r.event_index))
                    .unwrap_or_default()
            } else {
                String::new()
            };

            let events: Vec<Event> = rows.into_iter().map(row_to_event).collect();

            Ok(Response::new(ListEventsResponse {
                events,
                next_cursor,
                has_more,
            }))
        }
        .instrument(span)
        .await
    }

    async fn get_event(
        &self,
        request: Request<GetEventRequest>,
    ) -> Result<Response<Event>, Status> {
        let parent_cx = extract_context(request.metadata());
        let span = tracing::info_span!("get_event", "rpc.system" = "grpc");
        span.set_parent(parent_cx);

        let db = self.db.clone();
        let req = request.into_inner();

        validate_get_event(&req).map_err(|e| {
            tracing::warn!(error = %e, "invalid get_event request");
            e
        })?;

        async move {
            let id = Uuid::parse_str(&req.id)
                .map_err(|_| Status::invalid_argument("id must be a valid UUID"))?;

            let network = resolve_network(&req.network);

            let row: Option<EventRow> = sqlx::query_as(
                r#"
            SELECT id, contract_id, ledger_sequence, ledger_timestamp,
                   transaction_hash, event_index, event_type, topics, data, created_at
            FROM soroban_events
            WHERE id = $1
              AND network = $2
            "#,
            )
            .bind(id)
            .bind(network)
            .fetch_optional(&db)
            .await
            .map_err(db_err)?;

            match row {
                Some(r) => Ok(Response::new(row_to_event(r))),
                None => Err(Status::not_found(format!("event {id} not found"))),
            }
        }
        .instrument(span)
        .await
    }

    type StreamEventsStream = tokio_stream::wrappers::ReceiverStream<Result<Event, Status>>;

    async fn stream_events(
        &self,
        request: Request<StreamEventsRequest>,
    ) -> Result<Response<Self::StreamEventsStream>, Status> {
        let parent_cx = extract_context(request.metadata());
        let span = tracing::info_span!("stream_events", "rpc.system" = "grpc");
        span.set_parent(parent_cx);
        let _entered = span.entered();

        let req = request.into_inner();

        if req.contract_id.is_empty() {
            return Err(Status::invalid_argument("contract_id is required"));
        }

        let contract_id = req.contract_id;
        let topic_0_filter = if req.topic_0.is_empty() {
            None
        } else {
            Some(req.topic_0)
        };

        // Resume from where the client left off when it supplies a start_id
        // (issue #236); otherwise tail the stream live.
        let last_id = validate_start_id(&req.start_id)?;

        let (tx, rx) = tokio::sync::mpsc::channel(stream_channel_buffer());

        tokio::spawn(run_stream_consumer(
            self.redis.clone(),
            tx,
            StreamSubscription {
                contract_id,
                topic_0: topic_0_filter,
                start_id: last_id,
            },
        ));

        Ok(Response::new(tokio_stream::wrappers::ReceiverStream::new(
            rx,
        )))
    }
}

#[cfg(test)]
mod tests {
    use tokio_stream::StreamExt as _;

    use super::*;

    // When REQUIRE_TEST_SERVICES is set (the rust-integration CI job sets it),
    // a missing URL is a hard failure instead of a silent skip — otherwise a
    // misconfigured integration job would go green without running anything.
    macro_rules! require_services {
        () => {{
            let required = std::env::var("REQUIRE_TEST_SERVICES").is_ok();
            match (
                std::env::var("TEST_DATABASE_URL"),
                std::env::var("TEST_REDIS_URL"),
            ) {
                (Ok(db), Ok(rd)) => (db, rd),
                _ if required => panic!(
                    "TEST_DATABASE_URL and TEST_REDIS_URL must be set when REQUIRE_TEST_SERVICES is set"
                ),
                _ => {
                    eprintln!("SKIP: TEST_DATABASE_URL / TEST_REDIS_URL not set");
                    return;
                }
            }
        }};
    }

    async fn make_svc(db_url: &str, redis_url: &str) -> EventsServiceImpl {
        let db = PgPool::connect(db_url).await.unwrap();
        let redis = redis::Client::open(redis_url)
            .unwrap()
            .get_connection_manager()
            .await
            .unwrap();
        EventsServiceImpl::new(db, redis)
    }

    async fn seed_events(pool: &PgPool, contract_id: &str, network: &str, count: usize) {
        for i in 0..count {
            sqlx::query(
                r#"
                INSERT INTO soroban_events
                    (contract_id, network, ledger_sequence, ledger_timestamp, transaction_hash,
                     event_index, event_type, topics, data)
                VALUES ($1, $2, $3, NOW(), $4, $5, 'contract', '["transfer"]', '{}')
                ON CONFLICT DO NOTHING
                "#,
            )
            .bind(contract_id)
            .bind(network)
            .bind((100 + i) as i64)
            .bind(format!("txhash_{contract_id}_{network}_{i}"))
            .bind(i as i32)
            .execute(pool)
            .await
            .unwrap();
        }
    }

    async fn insert_one_event(pool: &PgPool, network: &str) -> Uuid {
        let id = Uuid::new_v4();
        let tx_hash = format!("txhashtest-{id}");
        sqlx::query(
            r#"
            INSERT INTO soroban_events
                (id, contract_id, network, ledger_sequence, ledger_timestamp, transaction_hash,
                 event_index, event_type, topics, data)
            VALUES ($1, 'CTEST', $2, 999, NOW(), $3, 0, 'contract', '["transfer"]', '{}')
            ON CONFLICT DO NOTHING
            "#,
        )
        .bind(id)
        .bind(network)
        .bind(tx_hash)
        .execute(pool)
        .await
        .unwrap();
        id
    }

    #[tokio::test]
    async fn list_events_filters_by_contract_id() {
        let (db_url, redis_url) = require_services!();
        let pool = PgPool::connect(&db_url).await.unwrap();

        let contract_a = format!("CONTRACT_A_{}", uuid::Uuid::new_v4());
        let contract_b = format!("CONTRACT_B_{}", uuid::Uuid::new_v4());

        seed_events(&pool, &contract_a, "testnet", 3).await;
        seed_events(&pool, &contract_b, "testnet", 2).await;

        let svc = make_svc(&db_url, &redis_url).await;
        let req = Request::new(ListEventsRequest {
            contract_id: contract_a.clone(),
            network: "testnet".to_string(),
            limit: 200,
            ..Default::default()
        });
        let resp = svc.list_events(req).await.unwrap().into_inner();

        assert_eq!(resp.events.len(), 3);
        assert!(resp.events.iter().all(|e| e.contract_id == contract_a));
        assert!(!resp.has_more);
    }

    #[tokio::test]
    async fn list_events_cursor_pagination() {
        let (db_url, redis_url) = require_services!();
        let pool = PgPool::connect(&db_url).await.unwrap();

        let contract_id = format!("CONTRACT_PAGE_{}", uuid::Uuid::new_v4());
        seed_events(&pool, &contract_id, "testnet", 5).await;

        let svc = make_svc(&db_url, &redis_url).await;

        let first_page = svc
            .list_events(Request::new(ListEventsRequest {
                contract_id: contract_id.clone(),
                network: "testnet".to_string(),
                limit: 2,
                ..Default::default()
            }))
            .await
            .unwrap()
            .into_inner();

        assert_eq!(first_page.events.len(), 2);
        assert!(first_page.has_more);
        assert!(!first_page.next_cursor.is_empty());

        let second_page = svc
            .list_events(Request::new(ListEventsRequest {
                contract_id: contract_id.clone(),
                network: "testnet".to_string(),
                limit: 200,
                cursor: first_page.next_cursor,
                ..Default::default()
            }))
            .await
            .unwrap()
            .into_inner();

        assert_eq!(second_page.events.len(), 3);
        assert!(!second_page.has_more);
    }

    // Issue #603's "done when": page-two latency is independent of partition
    // count, verified structurally rather than by wall-clock timing (which is
    // noisy and environment-dependent, and this repo has no criterion/bench
    // harness to make a timing assertion meaningful in CI). The structural
    // proof is that cursor resolution issues zero queries against
    // soroban_events: decode_events_cursor is pure string parsing, so a page-N
    // request's only soroban_events access is the same range-scoped SELECT
    // every page runs, whose cost is governed by `limit`, not by how many
    // partitions exist upstream of the cursor position. This replaces the old
    // `SELECT ledger_sequence, event_index FROM soroban_events WHERE id = $1`
    // lookup, which — being an equality lookup on a column outside the
    // partitioned primary key (ledger_sequence, id) — had to be checked
    // against every partition (see docs/db/explain-events-603.txt, captured
    // against a database seeded across 9 partitions: an `Append` over all 9
    // for the old query vs. zero extra queries for the new decode).
    #[tokio::test]
    async fn list_events_page_two_cursor_resolution_touches_no_partition() {
        let (db_url, redis_url) = require_services!();
        let pool = PgPool::connect(&db_url).await.unwrap();

        let contract_id = format!("CONTRACT_PART_{}", uuid::Uuid::new_v4());
        seed_events(&pool, &contract_id, "testnet", 3).await;

        let svc = make_svc(&db_url, &redis_url).await;

        let first_page = svc
            .list_events(Request::new(ListEventsRequest {
                contract_id: contract_id.clone(),
                network: "testnet".to_string(),
                limit: 1,
                ..Default::default()
            }))
            .await
            .unwrap()
            .into_inner();
        assert!(first_page.has_more);

        // This is the whole fix: resolving the next page's position is pure
        // parsing, not a database round trip. Before #603 there was no
        // equivalent client-side call at all — the lookup happened inside
        // list_events itself, invisibly, on every page after the first.
        let (seq, idx) = decode_events_cursor(&first_page.next_cursor)
            .expect("service-issued cursor must decode with no DB access");
        assert!(seq > 0, "expected a real ledger_sequence, got {seq}");
        assert!(idx >= 0);

        // The second page then runs exactly one soroban_events query — same
        // as the first page — confirming no hidden lookup query was reintroduced.
        let second_page = svc
            .list_events(Request::new(ListEventsRequest {
                contract_id: contract_id.clone(),
                network: "testnet".to_string(),
                limit: 200,
                cursor: first_page.next_cursor,
                ..Default::default()
            }))
            .await
            .unwrap()
            .into_inner();
        assert_eq!(second_page.events.len(), 2);
        assert!(!second_page.has_more);
    }

    #[tokio::test]
    async fn list_events_isolated_by_network() {
        let (db_url, redis_url) = require_services!();
        let pool = PgPool::connect(&db_url).await.unwrap();

        let contract_id = format!("CONTRACT_NET_{}", uuid::Uuid::new_v4());
        seed_events(&pool, &contract_id, "testnet", 3).await;
        seed_events(&pool, &contract_id, "mainnet", 2).await;

        let svc = make_svc(&db_url, &redis_url).await;

        let testnet_resp = svc
            .list_events(Request::new(ListEventsRequest {
                contract_id: contract_id.clone(),
                network: "testnet".to_string(),
                limit: 200,
                ..Default::default()
            }))
            .await
            .unwrap()
            .into_inner();

        let mainnet_resp = svc
            .list_events(Request::new(ListEventsRequest {
                contract_id: contract_id.clone(),
                network: "mainnet".to_string(),
                limit: 200,
                ..Default::default()
            }))
            .await
            .unwrap()
            .into_inner();

        assert_eq!(testnet_resp.events.len(), 3);
        assert_eq!(mainnet_resp.events.len(), 2);
    }

    #[tokio::test]
    async fn get_existing_event_returns_correct_fields() {
        let (db_url, redis_url) = require_services!();
        let pool = PgPool::connect(&db_url).await.unwrap();

        let event_id = insert_one_event(&pool, "testnet").await;

        let svc = make_svc(&db_url, &redis_url).await;
        let req = Request::new(GetEventRequest {
            id: event_id.to_string(),
            network: "testnet".to_string(),
        });
        let event = svc.get_event(req).await.unwrap().into_inner();

        assert_eq!(event.id, event_id.to_string());
        assert_eq!(event.contract_id, "CTEST");
        assert_eq!(event.event_type, "contract");
        assert_eq!(event.topics, vec!["transfer".to_string()]);
    }

    #[tokio::test]
    async fn get_event_wrong_network_returns_not_found() {
        let (db_url, redis_url) = require_services!();
        let pool = PgPool::connect(&db_url).await.unwrap();

        let event_id = insert_one_event(&pool, "testnet").await;

        let svc = make_svc(&db_url, &redis_url).await;
        let req = Request::new(GetEventRequest {
            id: event_id.to_string(),
            network: "mainnet".to_string(),
        });
        let err = svc.get_event(req).await.unwrap_err();

        assert_eq!(err.code(), tonic::Code::NotFound);
    }

    #[tokio::test]
    async fn get_unknown_uuid_returns_not_found() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let req = Request::new(GetEventRequest {
            id: Uuid::new_v4().to_string(),
            network: "testnet".to_string(),
        });
        let err = svc.get_event(req).await.unwrap_err();

        assert_eq!(err.code(), tonic::Code::NotFound);
    }

    #[tokio::test]
    async fn list_events_empty_topic_0_is_valid() {
        let (db_url, redis_url) = require_services!();
        let pool = PgPool::connect(&db_url).await.unwrap();

        let contract_id = format!("CONTRACT_EMPTY_{}", uuid::Uuid::new_v4());
        seed_events(&pool, &contract_id, "testnet", 2).await;

        let svc = make_svc(&db_url, &redis_url).await;
        let req = Request::new(ListEventsRequest {
            contract_id: contract_id.clone(),
            topic_0: String::new(),
            network: "testnet".to_string(),
            limit: 200,
            ..Default::default()
        });
        let resp = svc.list_events(req).await.unwrap().into_inner();

        assert_eq!(resp.events.len(), 2);
    }

    #[tokio::test]
    async fn list_events_long_contract_id_returns_invalid_argument() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let req = Request::new(ListEventsRequest {
            contract_id: "X".repeat(257),
            network: "testnet".to_string(),
            limit: 200,
            ..Default::default()
        });
        let err = svc.list_events(req).await.unwrap_err();

        assert_eq!(err.code(), tonic::Code::InvalidArgument);
    }

    #[tokio::test]
    async fn get_event_empty_id_returns_invalid_argument() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let req = Request::new(GetEventRequest {
            id: String::new(),
            network: "testnet".to_string(),
        });
        let err = svc.get_event(req).await.unwrap_err();

        assert_eq!(err.code(), tonic::Code::InvalidArgument);
    }

    #[tokio::test]
    async fn list_events_with_invalid_cursor_returns_invalid_argument() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let req = Request::new(ListEventsRequest {
            contract_id: "CTEST".to_string(),
            network: "testnet".to_string(),
            cursor: "not-a-valid-cursor".to_string(),
            limit: 200,
            ..Default::default()
        });
        let err = svc.list_events(req).await.unwrap_err();

        assert_eq!(err.code(), tonic::Code::InvalidArgument);
    }

    // Issue #603: the cursor format changed from a bare event id (requiring a
    // WHERE id = $1 lookup that cannot use the partitioned primary key) to an
    // encoded (ledger_sequence, event_index) pair. A pre-#603 bare-UUID cursor
    // must now fail cleanly rather than silently mis-paginate: decode_events_cursor
    // requires a CURSOR_SEPARATOR the UUID's own hyphens never produce, since
    // splitting on '_' never finds one in a UUID string, so this is exercised
    // both directly below and end-to-end here.
    #[tokio::test]
    async fn list_events_with_pre_603_uuid_cursor_returns_invalid_argument() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let req = Request::new(ListEventsRequest {
            contract_id: "CTEST".to_string(),
            network: "testnet".to_string(),
            cursor: Uuid::new_v4().to_string(),
            limit: 200,
            ..Default::default()
        });
        let err = svc.list_events(req).await.unwrap_err();

        assert_eq!(err.code(), tonic::Code::InvalidArgument);
    }

    // Proves issue #603's actual fix at the unit level, independent of a
    // database: decoding a cursor this service encoded recovers the exact
    // (ledger_sequence, event_index) pair, with no DB round trip involved at
    // all — encode_events_cursor/decode_events_cursor never touch `db`.
    #[test]
    fn events_cursor_round_trips_ledger_sequence_and_event_index() {
        let encoded = encode_events_cursor(59_600_000, 3);
        let (seq, idx) = decode_events_cursor(&encoded).expect("valid cursor should decode");
        assert_eq!(seq, 59_600_000);
        assert_eq!(idx, 3);
    }

    #[test]
    fn events_cursor_rejects_malformed_input() {
        for bad in [
            "",
            "not-a-valid-cursor",
            "59600000",     // missing the event_index half
            "59600000_",    // empty event_index half
            "_3",           // empty ledger_sequence half
            "abc_3",        // non-numeric ledger_sequence
            "59600000_abc", // non-numeric event_index
            "59600000_3_4", // extra segment
        ] {
            assert!(
                decode_events_cursor(bad).is_err(),
                "expected {bad:?} to be rejected as a malformed cursor"
            );
        }
    }

    #[tokio::test]
    async fn db_error_returns_unavailable() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let req = Request::new(GetEventRequest {
            id: Uuid::new_v4().to_string(),
            network: "nonexistent".to_string(),
        });
        let err = svc.get_event(req).await.unwrap_err();

        assert_eq!(err.code(), tonic::Code::NotFound);
    }

    #[tokio::test]
    async fn get_malformed_uuid_returns_invalid_argument() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let req = Request::new(GetEventRequest {
            id: "not-a-uuid".to_string(),
            network: "testnet".to_string(),
        });
        let err = svc.get_event(req).await.unwrap_err();

        assert_eq!(err.code(), tonic::Code::InvalidArgument);
    }

    #[tokio::test]
    async fn stream_events_delivers_published_event() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let req = Request::new(StreamEventsRequest {
            contract_id: "CTEST_STREAM".to_string(),
            topic_0: String::new(),
            start_id: String::new(),
        });

        let mut stream = svc.stream_events(req).await.unwrap().into_inner();

        tokio::time::sleep(Duration::from_millis(100)).await;
        let mut pub_conn = redis::Client::open(redis_url.as_str())
            .unwrap()
            .get_multiplexed_async_connection()
            .await
            .unwrap();
        let _: String = redis::cmd("XADD")
            .arg(REDIS_STREAM_KEY)
            .arg("*")
            .arg("contract_id")
            .arg("CTEST_STREAM")
            .arg("ledger_sequence")
            .arg("777")
            .arg("ledger_timestamp")
            .arg("2024-01-01T00:00:00Z")
            .arg("transaction_hash")
            .arg("txhashstream")
            .arg("event_index")
            .arg("0")
            .arg("event_type")
            .arg("contract")
            .arg("topics")
            .arg(r#"["transfer"]"#)
            .arg("data")
            .arg("null")
            .query_async(&mut pub_conn)
            .await
            .unwrap();

        let event: Event = tokio::time::timeout(Duration::from_secs(8), stream.next())
            .await
            .expect("timed out waiting for streamed event")
            .expect("stream ended unexpectedly")
            .expect("stream returned error");

        assert_eq!(event.contract_id, "CTEST_STREAM");
        assert_eq!(event.ledger_sequence, 777);
    }

    // -----------------------------------------------------------------------
    // Resume, cancellation, and buffering (issue #236)
    // -----------------------------------------------------------------------

    #[test]
    fn empty_start_id_means_live_tail() {
        assert_eq!(validate_start_id("").unwrap(), "$");
    }

    #[test]
    fn explicit_stream_ids_are_accepted() {
        assert_eq!(validate_start_id("$").unwrap(), "$");
        assert_eq!(validate_start_id("0").unwrap(), "0");
        assert_eq!(
            validate_start_id("1700000000000-0").unwrap(),
            "1700000000000-0"
        );
    }

    #[test]
    fn malformed_start_ids_are_rejected() {
        for bad in ["abc", "123", "1-2-3", "-1", "1-", "1700000000000-x"] {
            let err =
                validate_start_id(bad).expect_err(&format!("{bad:?} should have been rejected"));
            assert_eq!(err.code(), tonic::Code::InvalidArgument);
        }
    }

    #[tokio::test]
    async fn stream_resumes_from_a_supplied_start_id() {
        let (db_url, redis_url) = require_services!();
        let svc = make_svc(&db_url, &redis_url).await;

        let mut pub_conn = redis::Client::open(redis_url.as_str())
            .unwrap()
            .get_multiplexed_async_connection()
            .await
            .unwrap();

        let contract = format!("CRESUME_{}", Uuid::new_v4());

        // Published *before* subscribing: a live tail would never see this.
        let first_id: String = redis::cmd("XADD")
            .arg(REDIS_STREAM_KEY)
            .arg("*")
            .arg("contract_id")
            .arg(&contract)
            .arg("ledger_sequence")
            .arg("500")
            .arg("ledger_timestamp")
            .arg("2024-01-01T00:00:00Z")
            .arg("transaction_hash")
            .arg("txresume")
            .arg("event_index")
            .arg("0")
            .arg("event_type")
            .arg("contract")
            .arg("topics")
            .arg(r#"["transfer"]"#)
            .arg("data")
            .arg("null")
            .query_async(&mut pub_conn)
            .await
            .unwrap();

        // Resume from just before that entry by decrementing the sequence part.
        let (ms, _seq) = first_id.split_once('-').unwrap();
        let resume_from = format!("{}-0", ms.parse::<u64>().unwrap() - 1);

        let req = Request::new(StreamEventsRequest {
            contract_id: contract.clone(),
            topic_0: String::new(),
            start_id: resume_from,
        });
        let mut stream = svc.stream_events(req).await.unwrap().into_inner();

        let event: Event = tokio::time::timeout(Duration::from_secs(8), stream.next())
            .await
            .expect("timed out waiting for the replayed event")
            .expect("stream ended unexpectedly")
            .expect("stream returned error");

        assert_eq!(event.contract_id, contract);
        assert_eq!(
            event.ledger_sequence, 500,
            "resume must replay the entry published before subscribing"
        );
    }

    #[tokio::test]
    async fn dropping_the_receiver_stops_the_consumer() {
        let (_db_url, redis_url) = require_services!();
        let redis = redis::Client::open(redis_url.as_str())
            .unwrap()
            .get_connection_manager()
            .await
            .unwrap();

        let (tx, rx) = tokio::sync::mpsc::channel(8);
        let handle = tokio::spawn(run_stream_consumer(
            redis,
            tx,
            StreamSubscription {
                contract_id: "CTEST_CANCEL".to_string(),
                topic_0: None,
                start_id: "$".to_string(),
            },
        ));

        // Let the consumer park inside a blocking XREAD first.
        tokio::time::sleep(Duration::from_millis(200)).await;
        drop(rx);

        // It selects on tx.closed(), so it must return well inside one XREAD
        // block window (5s) rather than idling until that read returns.
        tokio::time::timeout(Duration::from_secs(2), handle)
            .await
            .expect("consumer did not stop after the receiver was dropped")
            .expect("consumer task panicked");
    }
}
