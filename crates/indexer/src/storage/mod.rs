//! # Storage snapshots (issue #270)
//!
//! Indexes contract storage (ledger entry) snapshots for tracked contracts,
//! bounded to what the allowlist and the current poll page actually touched:
//! for tracked contracts detected as SEP-41 tokens (issue #269), fetches the
//! `Balance(Address)` data entry for every address that moved funds in this
//! page's decoded token events, and records a new row only when the value
//! changed since the last observed snapshot.

use base64::{engine::general_purpose::STANDARD, Engine};
use stellar_xdr::curr::{
    AccountId, ContractDataDurability, ContractId, Hash, Int128Parts, LedgerEntryData, LedgerKey,
    LedgerKeyContractData, Limited, Limits, PublicKey, ReadXdr, ScAddress, ScSymbol, ScVal, ScVec,
    Uint256, VecM, WriteXdr,
};
use trident_common::TridentError;

use crate::rpc::RpcClient;
use crate::spec::contract_address;

/// Maximum keys the Soroban RPC accepts in a single `getLedgerEntries`
/// request. Unlike `getEvents` (`filters::MAX_CONTRACT_IDS_PER_FILTER`), the
/// RPC has no documented per-call constant for this method, so this value is
/// the commonly observed provider cap; exceeding it fails the whole call
/// (issue #662).
pub const MAX_LEDGER_ENTRY_KEYS_PER_REQUEST: usize = 200;

/// One observed contract-storage value, ready to diff against the last
/// persisted snapshot and insert if it changed.
#[derive(Debug)]
pub struct StorageObservation {
    pub storage_key: String,
    pub key_json: serde_json::Value,
    pub value_json: Option<serde_json::Value>,
}

fn decode_address(strkey: &str) -> Result<ScAddress, TridentError> {
    match stellar_strkey::Strkey::from_string(strkey) {
        Ok(stellar_strkey::Strkey::PublicKeyEd25519(pk)) => Ok(ScAddress::Account(AccountId(
            PublicKey::PublicKeyTypeEd25519(Uint256(pk.0)),
        ))),
        Ok(stellar_strkey::Strkey::Contract(c)) => Ok(ScAddress::Contract(ContractId(Hash(c.0)))),
        _ => Err(TridentError::parse(anyhow::anyhow!(
            "unsupported address strkey: {strkey}"
        ))),
    }
}

/// Build the `Balance(Address)` ledger key used by the reference SEP-41
/// token contract (contracts/token, issue #267) and by soroban-examples'
/// token contract — the de facto standard `DataKey::Balance(Address)`
/// storage layout.
fn balance_key(contract_id: &str, holder: &str) -> Result<LedgerKey, TridentError> {
    let entries: Vec<ScVal> = vec![
        ScVal::Symbol(ScSymbol::try_from("Balance".to_string()).expect("literal fits ScSymbol")),
        ScVal::Address(decode_address(holder)?),
    ];
    let key = ScVal::Vec(Some(ScVec(
        VecM::try_from(entries).expect("2 elements fits"),
    )));

    Ok(LedgerKey::ContractData(LedgerKeyContractData {
        contract: contract_address(contract_id)?,
        key,
        durability: ContractDataDurability::Persistent,
    }))
}

fn encode_key(key: &LedgerKey) -> Result<String, TridentError> {
    let mut buf = Vec::new();
    key.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
        .map_err(|e| TridentError::parse(anyhow::Error::new(e).context("encode LedgerKey")))?;
    Ok(STANDARD.encode(buf))
}

fn i128_to_json(parts: &Int128Parts) -> serde_json::Value {
    let value = ((parts.hi as i128) << 64) | (parts.lo as u128 as i128);
    serde_json::json!(value.to_string())
}

/// Fetch the current `Balance(Address)` entry for each holder of a tracked
/// SEP-41 token contract. Holders not present in this batch are silently
/// skipped (an address with no balance entry has never held the asset).
pub async fn fetch_balance_snapshots(
    rpc: &RpcClient,
    contract_id: &str,
    holders: &[String],
) -> Result<Vec<StorageObservation>, TridentError> {
    if holders.is_empty() {
        return Ok(Vec::new());
    }

    let mut keys = Vec::with_capacity(holders.len());
    let mut key_b64s = Vec::with_capacity(holders.len());
    for holder in holders {
        let key = balance_key(contract_id, holder)?;
        key_b64s.push(encode_key(&key)?);
        keys.push((holder, key_b64s.last().unwrap().clone()));
    }

    // Chunk against the RPC's key-count cap the same way getEvents filters
    // are chunked against MAX_CONTRACT_IDS_PER_FILTER (issue #662) — a page
    // of transfer events can realistically produce more unique holders than
    // one getLedgerEntries call accepts, and exceeding it fails the whole
    // call rather than just the excess keys.
    let mut entries = Vec::with_capacity(key_b64s.len());
    for chunk in key_b64s.chunks(MAX_LEDGER_ENTRY_KEYS_PER_REQUEST) {
        entries.extend(rpc.get_ledger_entries(chunk).await?);
    }

    // getLedgerEntries only returns entries that exist, so match results back
    // to holders by the (base64) key it was requested with.
    let mut observations = Vec::with_capacity(entries.len());
    for entry in &entries {
        let Some((holder, _)) = keys.iter().find(|(_, k)| k == &entry.key) else {
            continue;
        };

        let bytes = STANDARD.decode(&entry.xdr).map_err(|e| {
            TridentError::parse(anyhow::Error::new(e).context("base64 decode ledger entry"))
        })?;
        let mut cursor = std::io::Cursor::new(bytes);
        let data = LedgerEntryData::read_xdr(&mut Limited::new(&mut cursor, Limits::none()))
            .map_err(|e| {
                TridentError::parse(anyhow::Error::new(e).context("XDR decode LedgerEntryData"))
            })?;

        let LedgerEntryData::ContractData(contract_data) = data else {
            continue;
        };

        let value_json = match &contract_data.val {
            ScVal::I128(parts) => Some(i128_to_json(parts)),
            other => Some(serde_json::Value::String(format!("{other:?}"))),
        };

        observations.push(StorageObservation {
            storage_key: entry.key.clone(),
            key_json: serde_json::json!({ "Balance": holder }),
            value_json,
        });
    }

    Ok(observations)
}

#[cfg(test)]
mod tests {
    use super::*;

    const TEST_CONTRACT: &str = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM";
    const TEST_HOLDER_A: &str = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF";
    const TEST_HOLDER_B: &str = "GDPQS7HP6HHBD6UG2E5GLTIKQZX2PZEA6YWUVKXEYCKSN6QCHKGWCOTM";

    #[test]
    fn balance_key_is_deterministic_for_the_same_holder() {
        let a = encode_key(&balance_key(TEST_CONTRACT, TEST_HOLDER_A).unwrap()).unwrap();
        let b = encode_key(&balance_key(TEST_CONTRACT, TEST_HOLDER_A).unwrap()).unwrap();
        assert_eq!(a, b);
    }

    #[test]
    fn balance_key_differs_per_holder() {
        let a = encode_key(&balance_key(TEST_CONTRACT, TEST_HOLDER_A).unwrap()).unwrap();
        let b = encode_key(&balance_key(TEST_CONTRACT, TEST_HOLDER_B).unwrap()).unwrap();
        assert_ne!(a, b);
    }

    #[test]
    fn rejects_an_invalid_holder_strkey() {
        assert!(balance_key(TEST_CONTRACT, "not-a-strkey").is_err());
    }

    fn distinct_holder(i: usize) -> String {
        // Deterministic but distinct 32-byte ed25519 public keys, strkey
        // encoded, so each holder produces a different Balance() ledger key.
        use stellar_strkey::ed25519::PublicKey as StrkeyPublicKey;
        let mut bytes = [0u8; 32];
        bytes[0..8].copy_from_slice(&(i as u64).to_be_bytes());
        StrkeyPublicKey(bytes).to_string()
    }

    /// A holder count that exceeds the RPC's per-call key cap must still
    /// complete, split across multiple chunked `getLedgerEntries` calls
    /// rather than sending one oversized request that the provider rejects
    /// outright (issue #662).
    #[tokio::test]
    async fn fetch_balance_snapshots_chunks_beyond_the_per_request_key_cap() {
        use crate::rpc::{RpcClient, RpcHttpSettings};
        use std::sync::atomic::{AtomicUsize, Ordering};
        use std::sync::Arc;
        use wiremock::matchers::method;
        use wiremock::{Mock, MockServer, Request, Respond, ResponseTemplate};

        struct CountingEmptyResponder {
            calls: Arc<AtomicUsize>,
        }

        impl Respond for CountingEmptyResponder {
            fn respond(&self, _req: &Request) -> ResponseTemplate {
                self.calls.fetch_add(1, Ordering::SeqCst);
                ResponseTemplate::new(200).set_body_json(serde_json::json!({
                    "jsonrpc": "2.0",
                    "id": 5,
                    "result": { "entries": [] }
                }))
            }
        }

        let calls = Arc::new(AtomicUsize::new(0));
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(CountingEmptyResponder {
                calls: calls.clone(),
            })
            .mount(&server)
            .await;

        let rpc = RpcClient::with_settings(server.uri(), &RpcHttpSettings::default()).unwrap();

        let holder_count = MAX_LEDGER_ENTRY_KEYS_PER_REQUEST + 50;
        let holders: Vec<String> = (0..holder_count).map(distinct_holder).collect();

        let observations = fetch_balance_snapshots(&rpc, TEST_CONTRACT, &holders)
            .await
            .expect("chunked fetch must succeed");

        assert!(observations.is_empty());
        assert_eq!(
            calls.load(Ordering::SeqCst),
            2,
            "holder count over the cap must split into 2 requests"
        );
    }
}
