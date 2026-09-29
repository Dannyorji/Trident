use serde::Deserialize;
use serde_json::Value as Json;
// ScVal decoding is shared with the live indexer (issue #506): this crate
// previously carried a stale copy that predated the exact U256/I256
// rendering from #415 and lacked the Timepoint/Duration/Error arms, so a
// backfilled event could store different values than the live path stored
// for the same XDR. One decoder makes that divergence impossible.
use trident_common::scval::{decode_scval, scval_to_json, scval_to_string};
use trident_common::{EventType, SorobanEvent, TridentError};

/// Accept a field the RPC sends as either a JSON string or a JSON number.
/// `ledger` is quoted on older servers and a bare integer on current ones;
/// see the matching helper in `trident-indexer`'s rpc module.
fn string_or_number<'de, D>(deserializer: D) -> Result<String, D::Error>
where
    D: serde::Deserializer<'de>,
{
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum StringOrNumber {
        String(String),
        Number(u64),
    }

    Ok(match StringOrNumber::deserialize(deserializer)? {
        StringOrNumber::String(s) => s,
        StringOrNumber::Number(n) => n.to_string(),
    })
}

fn default_true() -> bool {
    true
}

#[derive(Deserialize, Debug, Clone)]
pub struct RawEvent {
    #[serde(rename = "type")]
    pub event_type: String,
    #[serde(deserialize_with = "string_or_number")]
    pub ledger: String,
    #[serde(rename = "ledgerClosedAt")]
    pub ledger_closed_at: String,
    #[serde(rename = "contractId")]
    pub contract_id: Option<String>,
    pub id: String,
    /// Removed in stellar-rpc v22 (stellar-rpc#382) in favour of `id`; kept
    /// optional so older servers still parse. Use [`RawEvent::page_cursor`].
    #[serde(rename = "pagingToken", default)]
    pub paging_token: Option<String>,
    #[serde(rename = "txHash")]
    pub tx_hash: String,
    /// Operation index within the transaction, added in stellar-rpc#383.
    /// Absent on older servers, where the index was encoded in `id`.
    #[serde(rename = "operationIndex", default)]
    pub operation_index: Option<u32>,
    pub topic: Vec<String>,
    pub value: String,
    /// Deprecated upstream (stellar-rpc#4590); absent means not filtered out.
    #[serde(rename = "inSuccessfulContractCall", default = "default_true")]
    pub in_successful_contract_call: bool,
}

impl RawEvent {
    /// Token to resume paging from, preferring `pagingToken` when present and
    /// falling back to `id`, its designated replacement.
    pub fn page_cursor(&self) -> String {
        self.paging_token.clone().unwrap_or_else(|| self.id.clone())
    }
}

pub struct EventsPage {
    pub events: Vec<RawEvent>,
    #[allow(dead_code)]
    pub latest_ledger: u64,
}

pub struct Parser {
    pub index_diagnostic: bool,
}

impl Parser {
    pub fn new(index_diagnostic: bool) -> Self {
        Self { index_diagnostic }
    }

    pub fn parse_event(&self, raw: &RawEvent) -> Result<Option<SorobanEvent>, TridentError> {
        let event_type = parse_event_type(&raw.event_type)?;

        if event_type == EventType::Diagnostic && !self.index_diagnostic {
            return Ok(None);
        }

        if !raw.in_successful_contract_call {
            return Ok(None);
        }

        let contract_id = raw.contract_id.clone().unwrap_or_default();

        let topics: Vec<String> = raw
            .topic
            .iter()
            .map(|xdr| decode_scval(xdr).map(|v| scval_to_string(&v)))
            .collect::<Result<_, _>>()?;

        let data = if raw.value.is_empty() {
            Json::Null
        } else {
            decode_scval(&raw.value).map(|v| scval_to_json(&v))?
        };

        let ledger_sequence: u64 = raw
            .ledger
            .parse()
            .map_err(|_| TridentError::parse(anyhow::anyhow!("invalid ledger: {}", raw.ledger)))?;

        // Prefer the explicit operationIndex (stellar-rpc#383); the legacy
        // `id` suffix is only correct on servers predating #382, which changed
        // the `id` format and made this parse fall through to 0 for every
        // event — colliding them all on the natural key (issue #388).
        let event_index: u32 = raw.operation_index.unwrap_or_else(|| {
            raw.id
                .split('-')
                .next_back()
                .and_then(|s| s.parse().ok())
                .unwrap_or(0)
        });

        Ok(Some(SorobanEvent {
            contract_id,
            topics,
            data,
            ledger_sequence,
            ledger_timestamp: raw.ledger_closed_at.clone(),
            transaction_hash: raw.tx_hash.clone(),
            event_index,
            raw_event_index: event_index,
            event_type,
        }))
    }
}

fn parse_event_type(raw: &str) -> Result<EventType, TridentError> {
    match raw {
        "contract" => Ok(EventType::Contract),
        "system" => Ok(EventType::System),
        "diagnostic" => Ok(EventType::Diagnostic),
        other => Err(TridentError::parse(anyhow::anyhow!(
            "unknown event type: {other}"
        ))),
    }
}
