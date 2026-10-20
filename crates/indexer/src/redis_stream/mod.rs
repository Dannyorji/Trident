pub mod relay;

use redis::{streams::StreamMaxlen, AsyncCommands};
use trident_common::{SorobanEvent, TridentError};

const STREAM_KEY: &str = "trident:events";

/// Publish a normalised event onto the Redis Stream, trimming it to at most
/// `maxlen` entries (approximate trim — `MAXLEN ~`). The Go API layer
/// consumes this stream to fan out to WebSocket subscribers.
///
/// `network` is written on every entry so consumers can keep mainnet and
/// testnet traffic apart on the shared stream (issue #741).
///
/// `event_id` carries the deterministic event id when the caller knows it. The
/// outbox relay delivers at least once, so the same event can appear twice on
/// the stream after a crash between `XADD` and the outbox update; that field is
/// what lets a consumer discard the repeat (issue #200).
pub async fn publish_event(
    conn: &mut redis::aio::MultiplexedConnection,
    event: &SorobanEvent,
    maxlen: u64,
    event_id: Option<&str>,
    network: &str,
) -> Result<(), TridentError> {
    let fields = stream_fields(event, event_id, network)?;
    let fields: Vec<(&str, &str)> = fields.iter().map(|(k, v)| (*k, v.as_str())).collect();

    let _: String = conn
        .xadd_maxlen(
            STREAM_KEY,
            StreamMaxlen::Approx(maxlen as usize),
            "*",
            &fields,
        )
        .await
        .map_err(|e| TridentError::storage(anyhow::Error::new(e).context("redis xadd")))?;

    Ok(())
}

/// Build the field list of one stream entry. `event_id` is the event's UUID and
/// is written as both `event_id` (relay dedupe) and `id` (the public id that
/// matches `GET /v1/events/{id}`).
fn stream_fields(
    event: &SorobanEvent,
    event_id: Option<&str>,
    network: &str,
) -> Result<Vec<(&'static str, String)>, TridentError> {
    let topics = serde_json::to_string(&event.topics)
        .map_err(|e| TridentError::storage(anyhow::Error::new(e).context("topics serialise")))?;
    let mut fields = vec![
        ("contract_id", event.contract_id.clone()),
        ("network", network.to_string()),
        ("ledger_sequence", event.ledger_sequence.to_string()),
        ("ledger_timestamp", event.ledger_timestamp.clone()),
        ("transaction_hash", event.transaction_hash.clone()),
        ("event_index", event.event_index.to_string()),
        (
            "event_type",
            format!("{:?}", event.event_type).to_lowercase(),
        ),
        ("topics", topics),
        ("data", event.data.to_string()),
    ];
    if let Some(id) = event_id {
        fields.push(("event_id", id.to_string()));
        fields.push(("id", id.to_string()));
    }
    Ok(fields)
}

#[cfg(test)]
mod tests {
    use super::*;
    use trident_common::EventType;

    fn event() -> SorobanEvent {
        SorobanEvent {
            contract_id: "CABC".into(),
            topics: vec!["transfer".into()],
            data: serde_json::json!({"amount": 1}),
            ledger_sequence: 42,
            ledger_timestamp: "2026-01-01T00:00:00Z".into(),
            transaction_hash: "abc".into(),
            event_index: 3,
            raw_event_index: 3,
            event_type: EventType::Contract,
        }
    }

    fn get<'a>(fields: &'a [(&'static str, String)], key: &str) -> Option<&'a str> {
        fields
            .iter()
            .find(|(k, _)| *k == key)
            .map(|(_, v)| v.as_str())
    }

    #[test]
    fn entry_carries_network_and_event_id() {
        let fields = stream_fields(
            &event(),
            Some("0b1d1c1e-0000-4000-8000-000000000001"),
            "mainnet",
        )
        .unwrap();
        assert_eq!(get(&fields, "network"), Some("mainnet"));
        assert_eq!(
            get(&fields, "id"),
            Some("0b1d1c1e-0000-4000-8000-000000000001")
        );
        assert_eq!(get(&fields, "event_id"), get(&fields, "id"));
        assert_eq!(get(&fields, "contract_id"), Some("CABC"));
    }

    #[test]
    fn entry_without_event_id_still_has_network() {
        let fields = stream_fields(&event(), None, "testnet").unwrap();
        assert_eq!(get(&fields, "network"), Some("testnet"));
        assert_eq!(get(&fields, "id"), None);
    }
}
