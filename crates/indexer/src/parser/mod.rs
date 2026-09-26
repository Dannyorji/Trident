//! # Parser
//!
//! Owns XDR decoding and event normalisation. Responsibilities:
//!
//! - Decoding raw base64-encoded XDR strings as returned by the Soroban RPC
//!   `getEvents` method into typed Rust values via the `stellar-xdr` crate.
//! - Normalising decoded `ScVal` topics into human-readable string representations
//!   and the event body into a `serde_json::Value` for storage and forwarding.
//! - Type coercion: Symbol/String → plain string, Address → strkey, I128/U128 →
//!   decimal string, Bool → "true"/"false", Bytes → hex, Map/Vec → JSON object/array.
//! - Returning `TridentError::ParseError` for any input that cannot be decoded so
//!   the caller (Streamer) can decide whether to skip or halt.

use serde_json::Value as Json;
use stellar_xdr::curr::ScVal;
use trident_common::{EventType, SorobanEvent, TridentError};

// ScVal decoding moved to the shared crate so the live parser and the
// backfill re-ingest path can never render the same XDR differently
// (issue #506). Re-exported here so in-crate callers and the existing test
// suite — including the proptest fuzz pass CI runs at PROPTEST_CASES=50000 —
// keep addressing them through `crate::parser::*`.
pub use trident_common::scval::{
    decode_scval, scaddress_to_string, scval_to_json, scval_to_string,
};

use crate::rpc::RawEvent;

pub mod invocation_metrics;
pub mod nft_events;
pub mod sac;
pub mod token_events;

use nft_events::NftEvent;
use sac::SacRegistry;
use token_events::TokenEvent;

/// A normalised event together with its optional typed projections.
#[derive(Debug)]
pub struct ParsedEvent {
    pub event: SorobanEvent,
    /// `Some` only for standard SEP-41 / SAC value-movement events whose payload
    /// matches the token interface layout.
    pub token: Option<TokenEvent>,
    /// `Some` only for NFT mint/transfer events (issue #275).
    #[allow(dead_code)]
    pub nft: Option<NftEvent>,
}

pub struct Parser {
    pub index_diagnostic: bool,
    /// Tracked SAC contract id -> asset context lookup (issue #262). Empty
    /// when no assets are configured; every contract then decodes with no
    /// asset context, same as before this feature existed.
    sac_registry: SacRegistry,
}

impl Parser {
    pub fn new(index_diagnostic: bool) -> Self {
        Self {
            index_diagnostic,
            sac_registry: SacRegistry::default(),
        }
    }

    /// Attach a pre-built SAC registry (issue #262). Kept separate from `new`
    /// so callers that don't track any assets pay no extra setup cost.
    pub fn with_sac_registry(mut self, sac_registry: SacRegistry) -> Self {
        self.sac_registry = sac_registry;
        self
    }

    /// Decode a raw RPC event into a normalised `SorobanEvent` plus, when the
    /// payload matches the standard token interface, a typed token projection
    /// (issue #211).
    ///
    /// The topic and body `ScVal`s are decoded once and reused for both outputs,
    /// so the projection costs no extra XDR work.
    pub fn parse_event_with_projection(
        &self,
        raw: &RawEvent,
    ) -> Result<Option<ParsedEvent>, TridentError> {
        let event_type = parse_event_type(&raw.event_type)?;

        if event_type == EventType::Diagnostic && !self.index_diagnostic {
            return Ok(None);
        }

        // Skip events emitted by failed contract calls — they have no observable effect.
        if !raw.in_successful_contract_call {
            return Ok(None);
        }

        let contract_id = raw.contract_id.clone().unwrap_or_default();

        let topic_vals: Vec<ScVal> = raw
            .topic
            .iter()
            .map(|xdr| decode_scval(xdr))
            .collect::<Result<_, _>>()?;
        let topics: Vec<String> = topic_vals.iter().map(scval_to_string).collect();

        let (data, data_val) = if raw.value.is_empty() {
            (Json::Null, ScVal::Void)
        } else {
            let val = decode_scval(&raw.value)?;
            (scval_to_json(&val), val)
        };

        // Attach asset context when the emitting contract is a recognised SAC
        // instance (issue #262). Any token-interface contract is eligible for
        // typed projection; only known SACs also carry asset_code/issuer.
        let token = token_events::decode_token_event(&topic_vals, &data_val).map(|mut event| {
            if let Some((code, issuer)) = self.sac_registry.lookup(&contract_id) {
                event.asset_code = Some(code.to_string());
                event.asset_issuer = Some(issuer.to_string());
            }
            event
        });
        let nft = nft_events::decode_nft_event(&topic_vals, &data_val);

        let ledger_sequence: u64 = raw
            .ledger
            .parse()
            .map_err(|_| TridentError::parse(anyhow::anyhow!("invalid ledger: {}", raw.ledger)))?;

        let event_index = raw_event_index(raw);

        Ok(Some(ParsedEvent {
            event: SorobanEvent {
                contract_id,
                topics,
                data,
                ledger_sequence,
                ledger_timestamp: raw.ledger_closed_at.clone(),
                transaction_hash: raw.tx_hash.clone(),
                event_index,
                raw_event_index: event_index,
                event_type,
            },
            token,
            nft,
        }))
    }
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

/// Position of an event within its transaction.
///
/// `soroban_events` carries a natural-key constraint on
/// `(ledger_sequence, transaction_hash, event_index, network)` (migration
/// 0025), so this must distinguish every event sharing a transaction or the
/// batch insert aborts with a duplicate-key violation.
///
/// It used to be scraped off the tail of the opaque `id`, which was formatted
/// `"{encoded}-{index}"`. stellar-rpc#382 changed `id`, so the parse silently
/// fell through to `unwrap_or(0)` and stamped *every* event with 0, colliding
/// the whole batch (issue #388).
///
/// stellar-rpc#383 added an explicit `operationIndex`, which is the correct
/// source where available; servers predating it omit the field, so the legacy
/// `id` suffix stays as the fallback, and 0 is the last resort.
///
/// A single operation can emit several events, so this alone is not guaranteed
/// unique within a transaction — [`assign_unique_event_indexes`] resolves any
/// remaining ties before insert.
pub(crate) fn raw_event_index(raw: &RawEvent) -> u32 {
    raw.operation_index
        .or_else(|| {
            raw.id
                .split('-')
                .next_back()
                .and_then(|s| s.parse::<u32>().ok())
        })
        .unwrap_or(0)
}

/// Break ties so `(ledger_sequence, transaction_hash, event_index, network)`
/// is unique across a batch, as migration 0025's constraint requires.
///
/// `raw_event_index` derives a position per event, but a single operation can
/// emit several events, and older servers may not supply an index at all — in
/// both cases two events in the same transaction land on the same value and
/// the batch insert aborts with a duplicate-key violation (issue #388).
///
/// Walks the batch in order and, whenever a `(ledger, tx_hash, index)` triple
/// repeats, advances the index to the next free slot. Events that already have
/// distinct indices are left untouched, so behaviour against a server that
/// reports them correctly is unchanged.
pub(crate) fn assign_unique_event_indexes(events: &mut [SorobanEvent]) {
    let mut seen: std::collections::HashSet<(u64, String, u32)> = std::collections::HashSet::new();
    for event in events.iter_mut() {
        while !seen.insert((
            event.ledger_sequence,
            event.transaction_hash.clone(),
            event.event_index,
        )) {
            event.event_index = event.event_index.saturating_add(1);
        }
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
#[cfg(test)]
mod tests {
    use super::*;
    use base64::{engine::general_purpose::STANDARD, Engine};
    use stellar_xdr::curr::{
        AccountId, BytesM, ContractExecutable, ContractId, Hash, Int128Parts, Limited, Limits,
        PublicKey, ScAddress, ScContractInstance, ScMap, ScMapEntry, ScNonceKey, ScString,
        ScSymbol, ScVal, Uint256, VecM, WriteXdr,
    };

    use crate::rpc::RawEvent;

    fn ev(ledger: u64, tx: &str, index: u32) -> SorobanEvent {
        SorobanEvent {
            contract_id: "C".to_string(),
            topics: Vec::new(),
            data: serde_json::json!(null),
            ledger_sequence: ledger,
            ledger_timestamp: "2026-08-09T20:00:00Z".to_string(),
            transaction_hash: tx.to_string(),
            event_index: index,
            raw_event_index: index,
            event_type: EventType::Contract,
        }
    }

    #[test]
    fn duplicate_event_indexes_within_a_transaction_are_separated() {
        // Regression (#388): every event arrived with index 0, so the batch
        // violated the (ledger, tx_hash, event_index, network) constraint from
        // migration 0025 and no events were ever inserted.
        let mut events = vec![ev(7, "aa", 0), ev(7, "aa", 0), ev(7, "aa", 0)];
        assign_unique_event_indexes(&mut events);
        let indexes: Vec<u32> = events.iter().map(|e| e.event_index).collect();
        assert_eq!(indexes, vec![0, 1, 2]);
    }

    #[test]
    fn distinct_event_indexes_are_left_alone() {
        // A server that reports operationIndex correctly must not be perturbed.
        let mut events = vec![ev(7, "aa", 0), ev(7, "aa", 1), ev(7, "aa", 2)];
        assign_unique_event_indexes(&mut events);
        let indexes: Vec<u32> = events.iter().map(|e| e.event_index).collect();
        assert_eq!(indexes, vec![0, 1, 2]);
    }

    #[test]
    fn the_key_is_scoped_per_transaction_and_ledger() {
        // The constraint includes tx_hash and ledger, so the same index in a
        // different transaction or ledger is legitimate and must be preserved.
        let mut events = vec![ev(7, "aa", 0), ev(7, "bb", 0), ev(8, "aa", 0)];
        assign_unique_event_indexes(&mut events);
        let indexes: Vec<u32> = events.iter().map(|e| e.event_index).collect();
        assert_eq!(indexes, vec![0, 0, 0]);
    }

    #[test]
    fn event_index_prefers_operation_index_over_the_legacy_id_suffix() {
        let mut raw = raw_event_fixture();
        raw.id = "0000000000000000007-0000000009".to_string();
        raw.operation_index = Some(3);
        assert_eq!(raw_event_index(&raw), 3);
    }

    #[test]
    fn event_index_falls_back_to_the_id_suffix_on_older_servers() {
        let mut raw = raw_event_fixture();
        raw.id = "0000000000000000007-0000000009".to_string();
        raw.operation_index = None;
        assert_eq!(raw_event_index(&raw), 9);
    }

    fn xdr_b64(val: &ScVal) -> String {
        let mut buf = Vec::new();
        val.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
            .expect("XDR encode failed");
        STANDARD.encode(buf)
    }

    fn sym(s: &str) -> ScVal {
        ScVal::Symbol(ScSymbol::try_from(s.to_string()).expect("symbol too long"))
    }

    fn make_event(
        event_type: &str,
        contract_id: Option<&str>,
        topics: Vec<ScVal>,
        value: ScVal,
        successful: bool,
    ) -> RawEvent {
        RawEvent {
            event_type: event_type.to_string(),
            ledger: "500".to_string(),
            ledger_closed_at: "2024-06-01T00:00:00Z".to_string(),
            contract_id: contract_id.map(str::to_string),
            id: "0000000000500000-0".to_string(),
            paging_token: Some("token1".to_string()),
            tx_hash: "deadbeefdeadbeef".to_string(),
            operation_index: None,
            topic: topics.iter().map(xdr_b64).collect(),
            value: xdr_b64(&value),
            in_successful_contract_call: successful,
        }
    }

    fn raw_event_fixture() -> RawEvent {
        make_event("contract", Some("CA"), vec![], ScVal::Void, true)
    }

    #[test]
    fn token_transfer_topics_and_amount_decoded() {
        // Standard SEP-41 transfer: topics=[Symbol("transfer"), Addr(from), Addr(to)], data=I128(amount)
        let from = ScVal::Address(ScAddress::Account(AccountId(
            PublicKey::PublicKeyTypeEd25519(Uint256([1u8; 32])),
        )));
        let to = ScVal::Address(ScAddress::Account(AccountId(
            PublicKey::PublicKeyTypeEd25519(Uint256([2u8; 32])),
        )));
        let amount = ScVal::I128(Int128Parts {
            hi: 0,
            lo: 1_000_000,
        });

        let contract_id = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM";
        let raw = make_event(
            "contract",
            Some(contract_id),
            vec![sym("transfer"), from, to],
            amount,
            true,
        );

        let parser = Parser::new(false);
        let event = parser
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap()
            .event;

        assert_eq!(event.contract_id, contract_id);
        assert_eq!(event.topics[0], "transfer");
        assert_eq!(event.data, serde_json::json!(1_000_000u64));
        assert_eq!(event.ledger_sequence, 500);
        assert_eq!(event.transaction_hash, "deadbeefdeadbeef");
    }

    #[test]
    fn mint_event_symbol_and_address_topics() {
        let to = ScVal::Address(ScAddress::Account(AccountId(
            PublicKey::PublicKeyTypeEd25519(Uint256([3u8; 32])),
        )));
        let amount = ScVal::I128(Int128Parts { hi: 0, lo: 5_000 });

        let raw = make_event(
            "contract",
            Some("CONTRACT"),
            vec![sym("mint"), to],
            amount,
            true,
        );

        let parser = Parser::new(false);
        let event = parser
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap()
            .event;

        assert_eq!(event.topics[0], "mint");
        assert_eq!(event.data, serde_json::json!(5_000u64));
    }

    #[test]
    fn transfer_event_yields_a_token_projection() {
        let from = ScVal::Address(ScAddress::Account(AccountId(
            PublicKey::PublicKeyTypeEd25519(Uint256([1u8; 32])),
        )));
        let to = ScVal::Address(ScAddress::Account(AccountId(
            PublicKey::PublicKeyTypeEd25519(Uint256([2u8; 32])),
        )));
        let amount = ScVal::I128(Int128Parts { hi: 0, lo: 7_500 });

        let raw = make_event(
            "contract",
            Some("CTOKEN"),
            vec![sym("transfer"), from, to],
            amount,
            true,
        );

        let parsed = Parser::new(false)
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap();
        let token = parsed.token.expect("transfer must produce a projection");
        assert_eq!(token.amount.as_deref(), Some("7500"));
        assert_eq!(parsed.event.contract_id, "CTOKEN");
    }

    #[test]
    fn non_token_event_yields_no_projection() {
        let raw = make_event(
            "contract",
            Some("CAPP"),
            vec![sym("swap")],
            ScVal::Void,
            true,
        );
        let parsed = Parser::new(false)
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap();
        assert!(
            parsed.token.is_none(),
            "a non-token event must not be projected"
        );
    }

    #[test]
    fn map_event_data_decodes_to_json_object() {
        let entries: Vec<ScMapEntry> = vec![
            ScMapEntry {
                key: ScVal::Symbol(ScSymbol::try_from("amount".to_string()).unwrap()),
                val: ScVal::I128(Int128Parts { hi: 0, lo: 100 }),
            },
            ScMapEntry {
                key: ScVal::Symbol(ScSymbol::try_from("fee".to_string()).unwrap()),
                val: ScVal::I128(Int128Parts { hi: 0, lo: 1 }),
            },
        ];
        let map_val = ScVal::Map(Some(ScMap(VecM::try_from(entries).unwrap())));
        let raw = make_event("contract", None, vec![sym("custom")], map_val, true);

        let parser = Parser::new(false);
        let event = parser
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap()
            .event;

        let obj = event
            .data
            .as_object()
            .expect("data should be a JSON object");
        assert_eq!(obj["amount"], serde_json::json!(100u64));
        assert_eq!(obj["fee"], serde_json::json!(1u64));
    }

    #[test]
    fn diagnostic_event_skipped_when_index_diagnostic_false() {
        let raw = make_event("diagnostic", None, vec![sym("debug")], ScVal::Void, true);
        let parser = Parser::new(false);
        assert!(
            parser.parse_event_with_projection(&raw).unwrap().is_none(),
            "diagnostic events must be skipped when index_diagnostic=false"
        );
    }

    #[test]
    fn diagnostic_event_included_when_index_diagnostic_true() {
        let raw = make_event("diagnostic", None, vec![sym("debug")], ScVal::Void, true);
        let parser = Parser::new(true);
        assert!(
            parser.parse_event_with_projection(&raw).unwrap().is_some(),
            "diagnostic events must be indexed when index_diagnostic=true"
        );
    }

    #[test]
    fn failed_contract_call_filtered() {
        let raw = make_event("contract", None, vec![sym("transfer")], ScVal::Void, false);
        let parser = Parser::new(false);
        assert!(
            parser.parse_event_with_projection(&raw).unwrap().is_none(),
            "events from failed contract calls must be filtered out"
        );
    }

    #[test]
    fn large_i128_decoded_as_json_string() {
        // i128::MAX — the largest value the decoder can represent, and well
        // beyond the 2^53 range a JSON number can carry losslessly, so it must
        // come back as a decimal string rather than a number.
        let v = ScVal::I128(Int128Parts {
            hi: (i128::MAX >> 64) as i64,
            lo: u64::MAX,
        });

        let raw = make_event("contract", None, vec![], v, true);
        let event = Parser::new(false)
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap()
            .event;

        assert!(
            event.data.is_string(),
            "large i128 must be a JSON string, got: {event:?}"
        );
        assert_eq!(
            event.data.as_str().unwrap(),
            "170141183460469231731687303715884105727",
            "exact decimal string must survive round-trip XDR -> JSON"
        );
    }

    #[test]
    fn u256_decoded_as_decimal_string() {
        let v = ScVal::U256(stellar_xdr::curr::UInt256Parts {
            hi_hi: 0,
            hi_lo: 0,
            lo_hi: 0,
            lo_lo: 1,
        });

        let raw = make_event("contract", None, vec![], v, true);
        let event = Parser::new(false)
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap()
            .event;

        assert_eq!(
            event.data,
            serde_json::json!("1"),
            "u256(1) must encode as \"1\""
        );
    }

    #[test]
    fn i256_decoded_as_decimal_string() {
        // -1 as a 256-bit two's-complement integer: every limb is all-ones.
        // `hi_hi` is the signed high limb; the rest are unsigned.
        let v = ScVal::I256(stellar_xdr::curr::Int256Parts {
            hi_hi: -1,
            hi_lo: u64::MAX,
            lo_hi: u64::MAX,
            lo_lo: u64::MAX,
        });

        let raw = make_event("contract", None, vec![], v, true);
        let event = Parser::new(false)
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap()
            .event;

        assert_eq!(
            event.data,
            serde_json::json!("-1"),
            "i256(-1) must encode as \"-1\""
        );
    }

    #[test]
    fn small_i128_remains_json_number() {
        let v = ScVal::I128(Int128Parts { hi: 0, lo: 123 });
        let raw = make_event("contract", None, vec![], v, true);
        let event = Parser::new(false)
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap()
            .event;

        assert!(
            event.data.is_number(),
            "small i128 that fits in i64 should remain a JSON number: {event:?}",
        );
        assert_eq!(event.data, serde_json::json!(123));
    }

    #[test]
    fn contract_address_topic_decoded_to_strkey() {
        let contract_hash = [0xABu8; 32];
        let addr = ScVal::Address(ScAddress::Contract(ContractId(Hash(contract_hash))));
        let raw = make_event(
            "contract",
            None,
            vec![sym("event"), addr],
            ScVal::Void,
            true,
        );

        let parser = Parser::new(false);
        let event = parser
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap()
            .event;

        assert!(
            event.topics[1].starts_with('C'),
            "contract strkey must start with C, got: {}",
            event.topics[1]
        );
        assert_eq!(
            event.topics[1].len(),
            56,
            "contract strkey must be 56 chars"
        );
    }

    // -----------------------------------------------------------------------
    // SAC asset context (issue #262)
    //
    // The fixture JSON carries topics/data/expected exactly like the plain
    // token_events fixtures; the tracked asset's SAC contract id is derived at
    // test time (rather than hardcoded) and used as the fixture event's
    // contract_id, so the test stays correct regardless of the exact strkey
    // the derivation produces.
    // -----------------------------------------------------------------------

    const TEST_PASSPHRASE: &str = "Test SDF Network ; September 2015";

    #[test]
    fn sac_transfer_fixture_gets_asset_context_attached() {
        let fixture: serde_json::Value = serde_json::from_str(include_str!(
            "../../fixtures/token_events/sac_transfer.json"
        ))
        .expect("fixture JSON");

        let asset_code = fixture["asset_code"].as_str().unwrap();
        let asset_issuer = fixture["asset_issuer"].as_str().unwrap();
        let contract_id =
            sac::derive_sac_contract_id(asset_code, asset_issuer, TEST_PASSPHRASE).unwrap();

        let registry = SacRegistry::build(
            &[sac::TrackedAsset {
                code: asset_code.to_string(),
                issuer: asset_issuer.to_string(),
            }],
            TEST_PASSPHRASE,
        )
        .unwrap();

        let topics: Vec<ScVal> = fixture["topics"]
            .as_array()
            .unwrap()
            .iter()
            .map(|t| decode_scval(t.as_str().unwrap()).unwrap())
            .collect();
        let value = decode_scval(fixture["data"].as_str().unwrap()).unwrap();

        let raw = make_event("contract", Some(&contract_id), topics, value, true);

        let parser = Parser::new(false).with_sac_registry(registry);
        let parsed = parser.parse_event_with_projection(&raw).unwrap().unwrap();
        let token = parsed.token.expect("transfer must produce a projection");

        let expected = &fixture["expected"];
        assert_eq!(token.amount.as_deref(), expected["amount"].as_str());
        assert_eq!(token.asset_code.as_deref(), Some(asset_code));
        assert_eq!(token.asset_issuer.as_deref(), Some(asset_issuer));
    }

    #[test]
    fn sac_transfer_from_an_untracked_contract_gets_no_asset_context() {
        // Same event shape as sac_transfer.json, but the contract_id is an
        // arbitrary contract not present in the SAC registry — this must not
        // be misattributed to any tracked asset.
        let fixture: serde_json::Value = serde_json::from_str(include_str!(
            "../../fixtures/token_events/sac_transfer_untracked.json"
        ))
        .expect("fixture JSON");

        let registry = SacRegistry::build(
            &[sac::TrackedAsset {
                code: "USDC".to_string(),
                issuer: "GAIRCEIRCEIRCEIRCEIRCEIRCEIRCEIRCEIRCEIRCEIRCEIRCEIRCF6M".to_string(),
            }],
            TEST_PASSPHRASE,
        )
        .unwrap();

        let topics: Vec<ScVal> = fixture["topics"]
            .as_array()
            .unwrap()
            .iter()
            .map(|t| decode_scval(t.as_str().unwrap()).unwrap())
            .collect();
        let value = decode_scval(fixture["data"].as_str().unwrap()).unwrap();

        let raw = make_event(
            "contract",
            Some("CARBITRARYUNRELATEDCONTRACTNOTINREGISTRY"),
            topics,
            value,
            true,
        );

        let parser = Parser::new(false).with_sac_registry(registry);
        let parsed = parser.parse_event_with_projection(&raw).unwrap().unwrap();
        let token = parsed
            .token
            .expect("transfer must still produce a projection");

        assert!(token.asset_code.is_none());
        assert!(token.asset_issuer.is_none());
    }

    #[test]
    fn parser_with_no_sac_registry_attaches_no_asset_context() {
        // Default Parser::new (no with_sac_registry call) must behave exactly
        // as before this feature existed.
        let from = ScVal::Address(ScAddress::Account(AccountId(
            PublicKey::PublicKeyTypeEd25519(Uint256([1u8; 32])),
        )));
        let to = ScVal::Address(ScAddress::Account(AccountId(
            PublicKey::PublicKeyTypeEd25519(Uint256([2u8; 32])),
        )));
        let amount = ScVal::I128(Int128Parts { hi: 0, lo: 42 });
        let raw = make_event(
            "contract",
            Some("CANYCONTRACT"),
            vec![sym("transfer"), from, to],
            amount,
            true,
        );

        let parsed = Parser::new(false)
            .parse_event_with_projection(&raw)
            .unwrap()
            .unwrap();
        let token = parsed.token.expect("transfer must produce a projection");
        assert!(token.asset_code.is_none());
        assert!(token.asset_issuer.is_none());
    }

    // -----------------------------------------------------------------------
    // ScVal variant coverage (issue #415)
    // -----------------------------------------------------------------------

    #[test]
    fn scval_to_string_bool_true() {
        assert_eq!(scval_to_string(&ScVal::Bool(true)), "true");
    }

    #[test]
    fn scval_to_string_bool_false() {
        assert_eq!(scval_to_string(&ScVal::Bool(false)), "false");
    }

    #[test]
    fn scval_to_string_string() {
        // Construct via XDR round-trip since ScString wraps StringM.
        let sm = stellar_xdr::curr::StringM::try_from(b"hello".to_vec()).unwrap();
        let val = ScVal::String(ScString(sm));
        assert_eq!(scval_to_string(&val), "hello");
    }

    #[test]
    fn scval_to_string_u32() {
        assert_eq!(scval_to_string(&ScVal::U32(42)), "42");
    }

    #[test]
    fn scval_to_string_i32() {
        assert_eq!(scval_to_string(&ScVal::I32(-7)), "-7");
    }

    #[test]
    fn scval_to_string_u64() {
        assert_eq!(
            scval_to_string(&ScVal::U64(u64::MAX)),
            "18446744073709551615"
        );
    }

    #[test]
    fn scval_to_string_i64() {
        assert_eq!(
            scval_to_string(&ScVal::I64(i64::MIN)),
            "-9223372036854775808"
        );
    }

    // Large-integer variants (issue #415). The decoder handles these but
    // nothing covered them, which is how the 256-bit packing bug below
    // survived: it produces a plausible-looking number, just the wrong one.
    #[test]
    fn scval_to_string_u128_uses_full_width() {
        let val = ScVal::U128(stellar_xdr::curr::UInt128Parts { hi: 1, lo: 0 });
        // hi=1 means 2^64, not 1.
        assert_eq!(scval_to_string(&val), "18446744073709551616");
    }

    #[test]
    fn scval_to_string_u128_max() {
        let val = ScVal::U128(stellar_xdr::curr::UInt128Parts {
            hi: u64::MAX,
            lo: u64::MAX,
        });
        assert_eq!(scval_to_string(&val), u128::MAX.to_string());
    }

    #[test]
    fn scval_to_string_i128_negative() {
        // -1 as two's complement across the two halves.
        let val = ScVal::I128(Int128Parts {
            hi: -1,
            lo: u64::MAX,
        });
        assert_eq!(scval_to_string(&val), "-1");
    }

    #[test]
    fn scval_to_string_i128_min() {
        let val = ScVal::I128(Int128Parts {
            hi: i64::MIN,
            lo: 0,
        });
        assert_eq!(scval_to_string(&val), i128::MIN.to_string());
    }

    #[test]
    fn u256_decodes_beyond_128_bits() {
        // hi_hi=1 means 2^192. The previous implementation packed all four
        // limbs into a u128 with 32-bit shifts, so this decoded to a wrong
        // (much smaller) number rather than failing loudly.
        let val = ScVal::U256(stellar_xdr::curr::UInt256Parts {
            hi_hi: 1,
            hi_lo: 0,
            lo_hi: 0,
            lo_lo: 0,
        });
        assert_eq!(
            scval_to_string(&val),
            "6277101735386680763835789423207666416102355444464034512896"
        );
    }

    #[test]
    fn u256_max_is_exact() {
        let val = ScVal::U256(stellar_xdr::curr::UInt256Parts {
            hi_hi: u64::MAX,
            hi_lo: u64::MAX,
            lo_hi: u64::MAX,
            lo_lo: u64::MAX,
        });
        assert_eq!(
            scval_to_string(&val),
            "115792089237316195423570985008687907853269984665640564039457584007913129639935"
        );
    }

    #[test]
    fn u256_small_value_round_trips() {
        let val = ScVal::U256(stellar_xdr::curr::UInt256Parts {
            hi_hi: 0,
            hi_lo: 0,
            lo_hi: 0,
            lo_lo: 12345,
        });
        assert_eq!(scval_to_string(&val), "12345");
    }

    #[test]
    fn i256_negative_one() {
        // -1 is all bits set across every limb.
        let val = ScVal::I256(stellar_xdr::curr::Int256Parts {
            hi_hi: -1,
            hi_lo: u64::MAX,
            lo_hi: u64::MAX,
            lo_lo: u64::MAX,
        });
        assert_eq!(scval_to_string(&val), "-1");
    }

    #[test]
    fn i256_min_is_exact() {
        let val = ScVal::I256(stellar_xdr::curr::Int256Parts {
            hi_hi: i64::MIN,
            hi_lo: 0,
            lo_hi: 0,
            lo_lo: 0,
        });
        assert_eq!(
            scval_to_string(&val),
            "-57896044618658097711785492504343953926634992332820282019728792003956564819968"
        );
    }

    // Timepoint/Duration/Error (issue #415). Before these arms existed all
    // three fell through to the debug catch-all, so they rendered as
    // "Timepoint(1700000000)" and incremented the unhandled-variant metric
    // despite being fully understood types.
    #[test]
    fn scval_to_string_timepoint() {
        let val = ScVal::Timepoint(stellar_xdr::curr::TimePoint(1_700_000_000));
        assert_eq!(scval_to_string(&val), "1700000000");
    }

    #[test]
    fn scval_to_string_duration() {
        let val = ScVal::Duration(stellar_xdr::curr::Duration(86_400));
        assert_eq!(scval_to_string(&val), "86400");
    }

    #[test]
    fn scval_to_json_timepoint_and_duration_are_strings() {
        // u64-valued, so they must not become JSON numbers: anything above
        // 2^53 loses precision in a JavaScript consumer.
        let tp = ScVal::Timepoint(stellar_xdr::curr::TimePoint(u64::MAX));
        assert_eq!(
            scval_to_json(&tp),
            Json::String("18446744073709551615".to_string())
        );
        let d = ScVal::Duration(stellar_xdr::curr::Duration(u64::MAX));
        assert_eq!(
            scval_to_json(&d),
            Json::String("18446744073709551615".to_string())
        );
    }

    #[test]
    fn timepoint_does_not_count_as_unhandled_variant() {
        // The point of the dedicated arms: a known type must not trip the
        // unhandled-variant signal, or that metric stops meaning anything.
        let val = ScVal::Timepoint(stellar_xdr::curr::TimePoint(1));
        let rendered = scval_to_string(&val);
        assert!(
            !rendered.contains("Timepoint"),
            "rendered via the debug catch-all: {rendered}"
        );
    }

    #[test]
    fn scval_to_json_bytes_is_hex_not_base64() {
        // docs/soroban-event-model.md claimed base64 for ScvBytes while the
        // code has always emitted hex. Pinning the real behaviour so the doc
        // and the encoder cannot drift apart again.
        let val = ScVal::Bytes(stellar_xdr::curr::ScBytes(
            BytesM::try_from(vec![0xDE, 0xAD, 0xBE, 0xEF]).unwrap(),
        ));
        assert_eq!(scval_to_json(&val), Json::String("deadbeef".to_string()));
    }

    #[test]
    fn scval_to_string_bytes() {
        let val = ScVal::Bytes(stellar_xdr::curr::ScBytes(
            BytesM::try_from(vec![0xDE, 0xAD, 0xBE, 0xEF]).unwrap(),
        ));
        assert_eq!(scval_to_string(&val), "deadbeef");
    }

    #[test]
    fn scval_to_string_vec_some() {
        let items = VecM::try_from(vec![ScVal::U32(1), ScVal::U32(2)]).unwrap();
        let val = ScVal::Vec(Some(stellar_xdr::curr::ScVec(items)));
        let s = scval_to_string(&val);
        assert!(s.contains('1'));
        assert!(s.contains('2'));
    }

    #[test]
    fn scval_to_string_vec_none() {
        // An ABSENT vec renders as the explicit "Vec(None)" marker (#209's
        // documented shape) — distinct from "[]", which is a present-but-
        // empty vec. The distinction survived the move to the shared decoder
        // (issue #506).
        assert_eq!(scval_to_string(&ScVal::Vec(None)), "Vec(None)");
        assert_eq!(
            scval_to_string(&ScVal::Vec(Some(stellar_xdr::curr::ScVec(
                vec![].try_into().unwrap()
            )))),
            "[]"
        );
    }

    #[test]
    fn scval_to_json_bool() {
        assert_eq!(scval_to_json(&ScVal::Bool(true)), Json::Bool(true));
        assert_eq!(scval_to_json(&ScVal::Bool(false)), Json::Bool(false));
    }

    #[test]
    fn scval_to_json_string() {
        let sm = stellar_xdr::curr::StringM::try_from(b"world".to_vec()).unwrap();
        let val = ScVal::String(ScString(sm));
        assert_eq!(scval_to_json(&val), Json::String("world".to_string()));
    }

    #[test]
    fn scval_to_json_u32() {
        assert_eq!(scval_to_json(&ScVal::U32(99)), Json::from(99u32));
    }

    #[test]
    fn scval_to_json_i32() {
        assert_eq!(scval_to_json(&ScVal::I32(-3)), Json::from(-3i32));
    }

    #[test]
    fn scval_to_json_u64() {
        assert_eq!(scval_to_json(&ScVal::U64(123)), Json::from(123u64));
    }

    #[test]
    fn scval_to_json_i64() {
        assert_eq!(scval_to_json(&ScVal::I64(-456)), Json::from(-456i64));
    }

    #[test]
    fn scval_to_json_bytes() {
        let val = ScVal::Bytes(stellar_xdr::curr::ScBytes(
            BytesM::try_from(vec![0xCA, 0xFE]).unwrap(),
        ));
        assert_eq!(scval_to_json(&val), Json::String("cafe".to_string()));
    }

    #[test]
    fn scval_to_json_vec_some() {
        let items = VecM::try_from(vec![ScVal::U32(10), ScVal::U32(20)]).unwrap();
        let val = ScVal::Vec(Some(stellar_xdr::curr::ScVec(items)));
        let json = scval_to_json(&val);
        let arr = json.as_array().expect("Vec(Some) must be a JSON array");
        assert_eq!(arr.len(), 2);
        assert_eq!(arr[0], Json::from(10u32));
        assert_eq!(arr[1], Json::from(20u32));
    }

    #[test]
    fn scval_to_json_vec_none() {
        let json = scval_to_json(&ScVal::Vec(None));
        let arr = json
            .as_array()
            .expect("Vec(None) must be an empty JSON array");
        assert!(arr.is_empty());
    }

    #[test]
    fn scval_to_json_void() {
        assert_eq!(scval_to_json(&ScVal::Void), Json::Null);
    }

    // -----------------------------------------------------------------------
    // Full ScVal variant coverage (issue #209)
    // -----------------------------------------------------------------------
    //
    // `ScVal` (stellar-xdr `curr::ScVal`) has exactly 22 variants. `scval_to_string`
    // and `scval_to_json` match every one explicitly with no wildcard arm, so
    // adding a 23rd variant on a future stellar-xdr upgrade is a compile error
    // here rather than a silent debug-format fallback. This table exercises
    // each variant once end-to-end and pins its documented JSON shape (see
    // docs/indexer/scval-json-mapping.md) as a golden test.

    fn sample_contract_instance_wasm() -> ScVal {
        ScVal::ContractInstance(ScContractInstance {
            executable: ContractExecutable::Wasm(Hash([0xAB; 32])),
            storage: None,
        })
    }

    fn sample_contract_instance_stellar_asset_with_storage() -> ScVal {
        let entries = VecM::try_from(vec![ScMapEntry {
            key: ScVal::Symbol(ScSymbol::try_from("decimals".to_string()).unwrap()),
            val: ScVal::U32(7),
        }])
        .unwrap();
        ScVal::ContractInstance(ScContractInstance {
            executable: ContractExecutable::StellarAsset,
            storage: Some(ScMap(entries)),
        })
    }

    #[test]
    fn every_scval_variant_has_a_documented_json_shape() {
        let map_entries = VecM::try_from(vec![ScMapEntry {
            key: ScVal::Symbol(ScSymbol::try_from("k".to_string()).unwrap()),
            val: ScVal::U32(1),
        }])
        .unwrap();

        let cases: Vec<(&str, ScVal, Json)> = vec![
            ("Bool", ScVal::Bool(true), Json::Bool(true)),
            ("Void", ScVal::Void, Json::Null),
            (
                "Error",
                ScVal::Error(stellar_xdr::curr::ScError::Contract(9)),
                Json::String(format!("{:?}", stellar_xdr::curr::ScError::Contract(9))),
            ),
            ("U32", ScVal::U32(42), Json::from(42u32)),
            ("I32", ScVal::I32(-42), Json::from(-42i32)),
            ("U64", ScVal::U64(42), Json::from(42u64)),
            ("I64", ScVal::I64(-42), Json::from(-42i64)),
            (
                "Timepoint",
                ScVal::Timepoint(stellar_xdr::curr::TimePoint(1_700_000_000)),
                Json::String("1700000000".to_string()),
            ),
            (
                "Duration",
                ScVal::Duration(stellar_xdr::curr::Duration(60)),
                Json::String("60".to_string()),
            ),
            (
                "U128",
                ScVal::U128(stellar_xdr::curr::UInt128Parts { hi: 0, lo: 100 }),
                Json::from(100u64),
            ),
            (
                "I128",
                ScVal::I128(Int128Parts {
                    hi: -1,
                    lo: u64::MAX,
                }),
                Json::from(-1i64),
            ),
            (
                "U256",
                ScVal::U256(stellar_xdr::curr::UInt256Parts {
                    hi_hi: 0,
                    hi_lo: 0,
                    lo_hi: 0,
                    lo_lo: 5,
                }),
                Json::String("5".to_string()),
            ),
            (
                "I256",
                ScVal::I256(stellar_xdr::curr::Int256Parts {
                    hi_hi: 0,
                    hi_lo: 0,
                    lo_hi: 0,
                    lo_lo: 5,
                }),
                Json::String("5".to_string()),
            ),
            (
                "Bytes",
                ScVal::Bytes(stellar_xdr::curr::ScBytes(
                    BytesM::try_from(vec![0xDE, 0xAD]).unwrap(),
                )),
                Json::String("dead".to_string()),
            ),
            (
                "String",
                ScVal::String(ScString(
                    stellar_xdr::curr::StringM::try_from(b"hi".to_vec()).unwrap(),
                )),
                Json::String("hi".to_string()),
            ),
            (
                "Symbol",
                ScVal::Symbol(ScSymbol::try_from("sym".to_string()).unwrap()),
                Json::String("sym".to_string()),
            ),
            (
                "Vec(Some)",
                ScVal::Vec(Some(stellar_xdr::curr::ScVec(
                    VecM::try_from(vec![ScVal::U32(1)]).unwrap(),
                ))),
                Json::Array(vec![Json::from(1u32)]),
            ),
            ("Vec(None)", ScVal::Vec(None), Json::Array(vec![])),
            (
                "Map(Some)",
                ScVal::Map(Some(ScMap(map_entries))),
                Json::Object({
                    let mut m = serde_json::Map::new();
                    m.insert("k".to_string(), Json::from(1u32));
                    m
                }),
            ),
            (
                "Map(None)",
                ScVal::Map(None),
                Json::Object(serde_json::Map::new()),
            ),
            (
                "Address",
                ScVal::Address(ScAddress::Contract(ContractId(Hash([0u8; 32])))),
                Json::String(scaddress_to_string(&ScAddress::Contract(ContractId(Hash(
                    [0u8; 32],
                ))))),
            ),
            (
                "ContractInstance(Wasm)",
                sample_contract_instance_wasm(),
                Json::Object({
                    let mut executable = serde_json::Map::new();
                    executable.insert("type".to_string(), Json::String("wasm".to_string()));
                    executable.insert(
                        "wasm_hash".to_string(),
                        Json::String(hex::encode([0xAB; 32])),
                    );
                    let mut m = serde_json::Map::new();
                    m.insert("executable".to_string(), Json::Object(executable));
                    m.insert("storage".to_string(), Json::Null);
                    m
                }),
            ),
            (
                "ContractInstance(StellarAsset+storage)",
                sample_contract_instance_stellar_asset_with_storage(),
                Json::Object({
                    let mut executable = serde_json::Map::new();
                    executable.insert(
                        "type".to_string(),
                        Json::String("stellar_asset".to_string()),
                    );
                    let mut storage = serde_json::Map::new();
                    storage.insert("decimals".to_string(), Json::from(7u32));
                    let mut m = serde_json::Map::new();
                    m.insert("executable".to_string(), Json::Object(executable));
                    m.insert("storage".to_string(), Json::Object(storage));
                    m
                }),
            ),
            (
                "LedgerKeyContractInstance",
                ScVal::LedgerKeyContractInstance,
                Json::String("ledger_key_contract_instance".to_string()),
            ),
            (
                "LedgerKeyNonce",
                ScVal::LedgerKeyNonce(ScNonceKey { nonce: 99 }),
                Json::Object({
                    let mut m = serde_json::Map::new();
                    m.insert("nonce".to_string(), Json::String("99".to_string()));
                    m
                }),
            ),
        ];

        for (name, val, expected_json) in cases {
            // Never panics, decodes to the documented shape, and round-trips
            // through serde_json (guards against e.g. a NaN or non-finite
            // float slipping through, which this codec never produces but a
            // future edit could).
            let json = scval_to_json(&val);
            assert_eq!(json, expected_json, "unexpected JSON shape for {name}");
            let s = serde_json::to_string(&json)
                .unwrap_or_else(|e| panic!("{name} JSON must serialise: {e}"));
            let _: Json = serde_json::from_str(&s)
                .unwrap_or_else(|e| panic!("{name} JSON must round-trip: {e}"));

            // scval_to_string must not panic on any variant either.
            let _ = scval_to_string(&val);
        }
    }

    #[test]
    fn contract_instance_string_repr_names_the_executable_kind() {
        assert!(scval_to_string(&sample_contract_instance_wasm()).contains("wasm"));
        assert!(
            scval_to_string(&sample_contract_instance_stellar_asset_with_storage())
                .contains("stellar_asset")
        );
    }

    #[test]
    fn ledger_key_variants_have_stable_string_repr() {
        assert_eq!(
            scval_to_string(&ScVal::LedgerKeyContractInstance),
            "ledger_key_contract_instance"
        );
        assert_eq!(
            scval_to_string(&ScVal::LedgerKeyNonce(ScNonceKey { nonce: 99 })),
            "99"
        );
    }

    #[test]
    fn nested_map_inside_vec_inside_contract_instance_storage_decodes_recursively() {
        // Recursive/nested structures (issue #209 acceptance criterion): a Map
        // holding a Vec holding a Map, nested inside a ContractInstance's
        // storage, all decode without truncation or panic.
        let inner_map = ScMap(
            VecM::try_from(vec![ScMapEntry {
                key: ScVal::Symbol(ScSymbol::try_from("inner".to_string()).unwrap()),
                val: ScVal::I32(-1),
            }])
            .unwrap(),
        );
        let vec_of_maps = ScVal::Vec(Some(stellar_xdr::curr::ScVec(
            VecM::try_from(vec![ScVal::Map(Some(inner_map))]).unwrap(),
        )));
        let outer_storage = ScMap(
            VecM::try_from(vec![ScMapEntry {
                key: ScVal::Symbol(ScSymbol::try_from("items".to_string()).unwrap()),
                val: vec_of_maps,
            }])
            .unwrap(),
        );
        let val = ScVal::ContractInstance(ScContractInstance {
            executable: ContractExecutable::StellarAsset,
            storage: Some(outer_storage),
        });

        let json = scval_to_json(&val);
        let inner = &json["storage"]["items"][0]["inner"];
        assert_eq!(*inner, Json::from(-1i32));
    }

    #[test]
    fn decode_scval_round_trip() {
        // Encode a ScVal to XDR, base64, decode, and verify it matches.
        let original = ScVal::U64(42);
        let b64 = xdr_b64(&original);
        let decoded = decode_scval(&b64).unwrap();
        assert_eq!(scval_to_string(&decoded), "42");
    }

    #[test]
    fn decode_scval_rejects_garbage() {
        assert!(decode_scval("not-valid-base64!!!").is_err());
    }

    // -----------------------------------------------------------------------
    // Dead-letter / poison-event tests (issue #414)
    //
    // Verifies that events with corrupt XDR payloads produce a clean error
    // (not a panic) so the streamer can dead-letter them and advance the
    // cursor past the poison event.
    // -----------------------------------------------------------------------

    #[test]
    fn poison_event_with_corrupt_topic_returns_error_not_panic() {
        let raw = RawEvent {
            event_type: "contract".to_string(),
            ledger: "500".to_string(),
            ledger_closed_at: "2024-06-01T00:00:00Z".to_string(),
            contract_id: Some("CA".to_string()),
            id: "0000000000500000-0".to_string(),
            paging_token: None,
            tx_hash: "deadbeef".to_string(),
            operation_index: None,
            topic: vec!["not-valid-xdr!!!".to_string()],
            value: "AAAAAQ==".to_string(), // valid XDR: ScVal::U32(1)
            in_successful_contract_call: true,
        };

        let parser = Parser::new(false);
        let result = parser.parse_event_with_projection(&raw);
        assert!(result.is_err(), "corrupt topic must produce an error");
    }

    #[test]
    fn poison_event_with_corrupt_value_returns_error_not_panic() {
        let raw = RawEvent {
            event_type: "contract".to_string(),
            ledger: "500".to_string(),
            ledger_closed_at: "2024-06-01T00:00:00Z".to_string(),
            contract_id: None,
            id: "0000000000500000-0".to_string(),
            paging_token: None,
            tx_hash: "deadbeef".to_string(),
            operation_index: None,
            topic: vec!["AAAAAA==".to_string()], // valid XDR: ScVal::Void
            value: "garbage!!!".to_string(),
            in_successful_contract_call: true,
        };

        let parser = Parser::new(false);
        let result = parser.parse_event_with_projection(&raw);
        assert!(result.is_err(), "corrupt value must produce an error");
    }

    #[test]
    fn valid_event_after_poison_event_can_still_parse() {
        // Simulates two events in sequence: the first is poison, the second
        // is valid. The parser is stateless so the second must succeed
        // independently — the streamer can advance the cursor past the first.
        let poison = RawEvent {
            event_type: "contract".to_string(),
            ledger: "500".to_string(),
            ledger_closed_at: "2024-06-01T00:00:00Z".to_string(),
            contract_id: None,
            id: "0000000000500000-0".to_string(),
            paging_token: None,
            tx_hash: "deadbeef".to_string(),
            operation_index: None,
            topic: vec!["not-valid-xdr!!!".to_string()],
            value: "AAAAAQ==".to_string(),
            in_successful_contract_call: true,
        };

        let valid = make_event("contract", None, vec![sym("transfer")], ScVal::Void, true);

        let parser = Parser::new(false);
        assert!(parser.parse_event_with_projection(&poison).is_err());
        assert!(parser
            .parse_event_with_projection(&valid)
            .unwrap()
            .is_some());
    }

    // -----------------------------------------------------------------------
    // Property-based fuzz tests (issue #416)
    // -----------------------------------------------------------------------

    use proptest::prelude::*;

    /// Generate random base64-encoded XDR from arbitrary ScVal values.
    fn arb_scval_b64() -> impl Strategy<Value = String> {
        // At least 9 bytes: the arms below index up to bytes[8] to fill a
        // u64/i64. A shorter vector panics inside the generator itself,
        // so the test fails before the parser is ever called.
        proptest::collection::vec(any::<u8>(), 9..128).prop_map(|bytes| {
            let discriminant = bytes[0] % 6;
            let val = match discriminant {
                0 => ScVal::Void,
                1 => ScVal::Bool(bytes[1] % 2 == 0),
                2 => ScVal::U32(u32::from_le_bytes([bytes[1], bytes[2], bytes[3], bytes[4]])),
                3 => ScVal::I32(i32::from_le_bytes([bytes[1], bytes[2], bytes[3], bytes[4]])),
                4 => ScVal::U64(u64::from_le_bytes([
                    bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7], bytes[8],
                ])),
                _ => ScVal::I64(i64::from_le_bytes([
                    bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7], bytes[8],
                ])),
            };
            xdr_b64(&val)
        })
    }

    /// Generate arbitrary raw base64 (may not be valid XDR).
    fn arb_raw_b64() -> impl Strategy<Value = String> {
        proptest::collection::vec(any::<u8>(), 0..256)
            .prop_map(|bytes| base64::engine::general_purpose::STANDARD.encode(&bytes))
    }

    /// Generate arbitrary ScVal values.
    fn arb_scval_val() -> impl Strategy<Value = ScVal> {
        // At least 9 bytes: the arms below index up to bytes[8] to fill a
        // u64/i64. A shorter vector panics inside the generator itself,
        // so the test fails before the parser is ever called.
        proptest::collection::vec(any::<u8>(), 9..128).prop_map(|bytes| {
            let d = bytes[0] % 8;
            match d {
                0 => ScVal::Void,
                1 => ScVal::Bool(bytes[1] % 2 == 0),
                2 => ScVal::U32(u32::from_le_bytes([bytes[1], bytes[2], bytes[3], bytes[4]])),
                3 => ScVal::I32(i32::from_le_bytes([bytes[1], bytes[2], bytes[3], bytes[4]])),
                4 => ScVal::U64(u64::from_le_bytes([
                    bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7], bytes[8],
                ])),
                5 => ScVal::I64(i64::from_le_bytes([
                    bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7], bytes[8],
                ])),
                6 => ScVal::Symbol(
                    ScSymbol::try_from(
                        String::from_utf8_lossy(&bytes[1..std::cmp::min(32, bytes.len())])
                            .into_owned(),
                    )
                    .unwrap_or_else(|_| ScSymbol::try_from("x".to_string()).unwrap()),
                ),
                _ => ScVal::Bytes(stellar_xdr::curr::ScBytes(
                    BytesM::try_from(bytes[1..std::cmp::min(64, bytes.len())].to_vec())
                        .unwrap_or_default(),
                )),
            }
        })
    }

    proptest! {
        #![proptest_config(ProptestConfig::with_cases(2000))]

        #[test]
        fn decode_scval_never_panics_on_arbitrary_xdr(b64 in arb_scval_b64()) {
            let _ = decode_scval(&b64);
        }

        #[test]
        fn decode_scval_never_panics_on_random_bytes(b64 in arb_raw_b64()) {
            let _ = decode_scval(&b64);
        }

        #[test]
        fn scval_to_string_never_panics(val in arb_scval_val()) {
            let _ = scval_to_string(&val);
        }

        #[test]
        fn scval_to_json_never_panics(val in arb_scval_val()) {
            let _ = scval_to_json(&val);
        }

        #[test]
        fn scval_to_json_output_is_valid_json(val in arb_scval_val()) {
            let json = scval_to_json(&val);
            let s = serde_json::to_string(&json).expect("scval_to_json must produce valid JSON");
            let _: serde_json::Value = serde_json::from_str(&s).expect("JSON round-trip must succeed");
        }

        #[test]
        fn decode_then_scval_to_string_roundtrip(b64 in arb_scval_b64()) {
            if let Ok(val) = decode_scval(&b64) {
                let s = scval_to_string(&val);
                if !matches!(val, ScVal::Void) {
                    assert!(!s.is_empty(), "scval_to_string must not return empty for non-Void");
                }
            }
        }
    }

    // -----------------------------------------------------------------------
    // Whole-entry-point fuzzing (issue #219)
    // -----------------------------------------------------------------------
    //
    // The #416 suite above fuzzes `decode_scval`/`scval_to_string`/
    // `scval_to_json` directly. Those are internal helpers; the actual attack
    // surface facing untrusted on-chain bytes is `Parser::parse_event_with_
    // projection`, which additionally parses `event_type`/`ledger` strings,
    // decodes every topic, and runs the SEP-41/NFT/invocation projections on
    // top of the decoded value. This suite drives that entry point directly.

    /// Generate an arbitrary, frequently-malformed `RawEvent` — the actual
    /// parser entry point. Each field mixes a plausible value with garbage so
    /// the fuzzer exercises the full decode -> normalise -> project pipeline
    /// (event-type parsing, per-topic XDR decode, token/NFT projection),
    /// not just a single inner conversion.
    fn arb_raw_event() -> impl Strategy<Value = RawEvent> {
        let arb_sc_encoded = prop_oneof![arb_scval_b64(), arb_raw_b64()];
        (
            prop_oneof![
                3 => Just("contract".to_string()),
                3 => Just("system".to_string()),
                3 => Just("diagnostic".to_string()),
                1 => "[a-zA-Z]{0,12}",
            ],
            prop_oneof![
                3 => "[0-9]{1,10}",
                1 => "[a-zA-Z!]{0,10}",
                1 => Just(String::new()),
            ],
            proptest::option::of("[A-Za-z0-9]{0,56}"),
            proptest::collection::vec(arb_sc_encoded.clone(), 0..6),
            prop_oneof![
                3 => arb_scval_b64(),
                2 => arb_raw_b64(),
                1 => Just(String::new()),
            ],
            any::<bool>(),
        )
            .prop_map(
                |(event_type, ledger, contract_id, topic, value, successful)| RawEvent {
                    event_type,
                    ledger,
                    ledger_closed_at: "2024-06-01T00:00:00Z".to_string(),
                    contract_id,
                    id: "fuzz-id".to_string(),
                    paging_token: None,
                    tx_hash: "fuzzhash".to_string(),
                    operation_index: None,
                    topic,
                    value,
                    in_successful_contract_call: successful,
                },
            )
    }

    // Hostile-input fuzzing (issue #507): truncation, trailing bytes, lying
    // length prefixes, and hostile nesting depth. The parser consumes
    // untrusted network data, so malformed input must yield a handled error
    // — never a panic, a stack overflow, or an unbounded allocation. These
    // run in the same parser::tests:: pass CI re-executes with
    // PROPTEST_CASES=50000 (.github/workflows/ci.yml, rust job).
    // -----------------------------------------------------------------------

    /// Wire bytes for a Vec nested `depth` levels around a U32, built
    /// directly as bytes: encoding a deep value through the XDR writer
    /// would recurse just as deep, so hostile depths must be synthesized.
    fn nested_vec_wire(depth: u32) -> Vec<u8> {
        let mut out = Vec::new();
        for _ in 0..depth {
            out.extend_from_slice(&16u32.to_be_bytes()); // ScValType::Vec
            out.extend_from_slice(&1u32.to_be_bytes()); // Option: Some
            out.extend_from_slice(&1u32.to_be_bytes()); // one element
        }
        out.extend_from_slice(&3u32.to_be_bytes()); // ScValType::U32
        out.extend_from_slice(&7u32.to_be_bytes());
        out
    }

    proptest! {
        #![proptest_config(ProptestConfig::with_cases(2000))]

        /// The core invariant this issue asks for: for any input, the parser
        /// entry point returns `Ok(Some(_))`, `Ok(None)`, or an `Err` the
        /// caller can record — it never panics. A panic here would abort the
        /// streamer's poll loop on a single crafted event (issue #219).
        #[test]
        fn parse_event_with_projection_never_panics(raw in arb_raw_event()) {
            let parser = Parser::new(true);
            let _ = parser.parse_event_with_projection(&raw);
        }

        /// Same invariant with diagnostic events excluded, matching the
        /// production default (`INDEX_DIAGNOSTIC=false`) and its early-return
        /// branch in `parse_event_with_projection`.
        #[test]
        fn parse_event_with_projection_never_panics_index_diagnostic_false(raw in arb_raw_event()) {
            let parser = Parser::new(false);
            let _ = parser.parse_event_with_projection(&raw);
        }

        #[test]
        fn truncated_valid_xdr_is_a_handled_error(b64 in arb_scval_b64(), cut in any::<prop::sample::Index>()) {
            let bytes = STANDARD.decode(&b64).expect("generator emits valid base64");
            // Every strict prefix of a valid encoding is incomplete: XDR is
            // self-delimiting, so the decoder must error — and must not panic.
            let cut = cut.index(bytes.len().max(1));
            if cut < bytes.len() {
                let truncated = STANDARD.encode(&bytes[..cut]);
                prop_assert!(decode_scval(&truncated).is_err(),
                    "strict prefix of a valid encoding decoded successfully");
            }
        }

        #[test]
        fn trailing_garbage_after_a_valid_value_is_rejected(
            b64 in arb_scval_b64(),
            extra in proptest::collection::vec(any::<u8>(), 1..32),
        ) {
            let mut bytes = STANDARD.decode(&b64).expect("generator emits valid base64");
            bytes.extend(extra);
            prop_assert!(decode_scval(&STANDARD.encode(&bytes)).is_err(),
                "value followed by trailing bytes must be rejected");
        }

        #[test]
        fn arbitrary_nesting_depth_never_panics(depth in 1u32..300) {
            // Below the budget this decodes and renders; above it, a handled
            // error. Either way the process survives — without the depth
            // limit a deep payload overflows the stack, which is a SIGABRT,
            // not an Err. The boundary assertions pin the budget in
            // CONTAINER LEVELS: everything the Soroban host can legally emit
            // (<= 100 levels, DEFAULT_HOST_DEPTH_LIMIT) must decode; each
            // Vec level costs ~4 reader frames against MAX_SCVAL_DEPTH=500,
            // so failures may only start well past the host limit.
            let b64 = STANDARD.encode(nested_vec_wire(depth));
            match decode_scval(&b64) {
                Ok(val) => {
                    let _ = scval_to_string(&val);
                    let _ = scval_to_json(&val);
                }
                Err(_) => {
                    prop_assert!(
                        depth > 100,
                        "host-legal nesting must decode, failed at depth {depth}"
                    );
                }
            }
        }

        #[test]
        fn deep_nesting_beyond_the_budget_is_rejected(depth in 200u32..2000) {
            let b64 = STANDARD.encode(nested_vec_wire(depth));
            prop_assert!(decode_scval(&b64).is_err(),
                "nesting past MAX_SCVAL_DEPTH must be a handled error");
        }

        #[test]
        fn lying_collection_length_prefix_never_panics(claim in any::<u32>(), tag in 0u32..24) {
            // A collection/bytes discriminant followed by an arbitrary length
            // claim and almost no real data: the reader's byte budget is the
            // input length, so the claim cannot drive allocation or scanning
            // past the payload.
            let mut wire = tag.to_be_bytes().to_vec();
            wire.extend_from_slice(&claim.to_be_bytes());
            wire.extend_from_slice(&[0xAA; 8]);
            let _ = decode_scval(&STANDARD.encode(&wire));
        }
    }

    /// Seed corpus of realistic (non-random) event shapes, run as ordinary
    /// regression tests rather than through proptest's random search (issue
    /// #219: "seed the corpus with real testnet events"). These mirror
    /// payload shapes actually seen from Soroban RPC — a SEP-41 transfer, a
    /// diagnostic event, a system fee event, and an event with a Map-typed
    /// body — so the fuzz suite's random search is complemented by fixed
    /// cases known to matter, and a future edit that breaks one of these
    /// shapes fails immediately instead of waiting on proptest to rediscover
    /// it.
    #[test]
    fn seed_corpus_of_realistic_events_parses_without_panicking() {
        let addr = |seed: u8| {
            ScVal::Address(ScAddress::Account(AccountId(
                PublicKey::PublicKeyTypeEd25519(stellar_xdr::curr::Uint256([seed; 32])),
            )))
        };

        let seeds: Vec<RawEvent> = vec![
            // A standard SEP-41 `transfer` event.
            make_event(
                "contract",
                Some("CCONTRACT"),
                vec![sym("transfer"), addr(1), addr(2)],
                ScVal::I128(Int128Parts {
                    hi: 0,
                    lo: 1_000_000,
                }),
                true,
            ),
            // A diagnostic event (skipped unless index_diagnostic is set).
            make_event(
                "diagnostic",
                Some("CCONTRACT"),
                vec![sym("log")],
                ScVal::String(ScString(
                    stellar_xdr::curr::StringM::try_from(b"debug".to_vec()).unwrap(),
                )),
                true,
            ),
            // A system event with a Map-typed body (fee/resource accounting
            // shapes commonly look like this).
            make_event(
                "system",
                None,
                vec![sym("fee_charged")],
                ScVal::Map(Some(ScMap(
                    VecM::try_from(vec![ScMapEntry {
                        key: ScVal::Symbol(ScSymbol::try_from("amount".to_string()).unwrap()),
                        val: ScVal::I64(12_345),
                    }])
                    .unwrap(),
                ))),
                true,
            ),
            // An event from a failed contract call — must be skipped
            // (`Ok(None)`), not treated as an error.
            make_event(
                "contract",
                Some("CCONTRACT"),
                vec![sym("transfer")],
                ScVal::Void,
                false,
            ),
        ];

        let parser = Parser::new(true);
        for raw in &seeds {
            let result = parser.parse_event_with_projection(raw);
            assert!(
                result.is_ok(),
                "seed corpus event must parse cleanly: {raw:?} -> {result:?}"
            );
        }
    }
}
