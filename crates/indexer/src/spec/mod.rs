//! # Spec (issue #260)
//!
//! Fetches a tracked contract's deployed WASM via `getLedgerEntries`
//! (instance entry -> code hash -> code entry) and parses the embedded
//! `contractspecv0` custom section (contractmeta / SEP-48) into its exported
//! functions. Parsed specs are keyed by WASM code hash and cached in memory
//! so contracts sharing the same deployed code (e.g. many token instances)
//! only pay the parse cost once.
//!
//! Interface classification from the parsed functions (issue #269) lives in
//! [`interfaces`].

use std::collections::HashMap;
use std::sync::Mutex;

use base64::{engine::general_purpose::STANDARD, Engine};
use stellar_xdr::curr::{
    ContractDataDurability, ContractExecutable, ContractId, Hash, LedgerEntryData, LedgerKey,
    LedgerKeyContractCode, LedgerKeyContractData, Limited, Limits, ReadXdr, ScAddress,
    ScContractInstance, ScSpecEntry, ScVal, WriteXdr,
};
use trident_common::TridentError;

use crate::rpc::RpcClient;

pub mod interfaces;

/// One function exported by a contract, as captured from its spec.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct SpecFunction {
    pub name: String,
}

/// Parsed + classified spec for a contract at its currently-deployed code
/// hash.
#[derive(Debug, Clone)]
pub struct ContractSpec {
    pub code_hash: String,
    pub has_spec: bool,
    pub functions: Vec<SpecFunction>,
    pub contract_type: String,
    pub interfaces: Vec<String>,
}

/// In-memory cache of parsed spec functions keyed by WASM code hash (hex).
/// Avoids re-fetching and re-parsing the same WASM for every contract that
/// happens to share it.
#[derive(Default)]
pub struct SpecCache {
    by_code_hash: Mutex<HashMap<String, Vec<SpecFunction>>>,
}

impl SpecCache {
    pub fn new() -> Self {
        Self::default()
    }

    /// Parsed functions already known for `code_hash`, whether they came from
    /// a WASM this process downloaded or from `contract_specs` at startup
    /// (issue #726).
    pub fn get(&self, code_hash: &str) -> Option<Vec<SpecFunction>> {
        self.by_code_hash
            .lock()
            .expect("spec cache mutex poisoned")
            .get(code_hash)
            .cloned()
    }

    /// Remember the parsed functions for `code_hash`. Keying is by WASM hash,
    /// so what is cached is content-addressed and cannot go stale.
    pub fn insert(&self, code_hash: impl Into<String>, functions: Vec<SpecFunction>) {
        self.by_code_hash
            .lock()
            .expect("spec cache mutex poisoned")
            .insert(code_hash.into(), functions);
    }
}

/// Build the `ScAddress` for a contract strkey (`C...`).
pub(crate) fn contract_address(contract_id: &str) -> Result<ScAddress, TridentError> {
    let strkey = stellar_strkey::Contract::from_string(contract_id).map_err(|e| {
        TridentError::parse(anyhow::anyhow!("invalid contract id {contract_id}: {e}"))
    })?;
    Ok(ScAddress::Contract(ContractId(Hash(strkey.0))))
}

fn encode_key(key: &LedgerKey) -> Result<String, TridentError> {
    let mut buf = Vec::new();
    key.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
        .map_err(|e| TridentError::parse(anyhow::Error::new(e).context("encode LedgerKey")))?;
    Ok(STANDARD.encode(buf))
}

fn decode_entry(xdr_b64: &str) -> Result<LedgerEntryData, TridentError> {
    let bytes = STANDARD.decode(xdr_b64).map_err(|e| {
        TridentError::parse(anyhow::Error::new(e).context("base64 decode ledger entry"))
    })?;
    let mut cursor = std::io::Cursor::new(bytes);
    LedgerEntryData::read_xdr(&mut Limited::new(&mut cursor, Limits::none())).map_err(|e| {
        TridentError::parse(anyhow::Error::new(e).context("XDR decode LedgerEntryData"))
    })
}

/// Fetch the WASM bytecode for a given code hash via its contract-code
/// ledger entry.
async fn fetch_wasm(rpc: &RpcClient, hash: &Hash) -> Result<Vec<u8>, TridentError> {
    let code_key = LedgerKey::ContractCode(LedgerKeyContractCode { hash: hash.clone() });
    let entries = rpc.get_ledger_entries(&[encode_key(&code_key)?]).await?;
    let Some(entry) = entries.first() else {
        return Ok(Vec::new());
    };
    let LedgerEntryData::ContractCode(code_entry) = decode_entry(&entry.xdr)? else {
        return Ok(Vec::new());
    };
    Ok(code_entry.code.to_vec())
}

// ---------------------------------------------------------------------------
// WASM `contractspecv0` custom-section parsing
// ---------------------------------------------------------------------------

fn read_leb128_u32(buf: &[u8], pos: &mut usize) -> Option<u32> {
    let mut result: u32 = 0;
    let mut shift: u32 = 0;
    loop {
        let byte = *buf.get(*pos)?;
        *pos += 1;
        result |= ((byte & 0x7f) as u32) << shift;
        if byte & 0x80 == 0 {
            break;
        }
        shift += 7;
        if shift >= 35 {
            return None;
        }
    }
    Some(result)
}

/// Locate a named custom section inside a WASM binary. This is a minimal
/// walk of the module header just to find `contractspecv0` — not a general
/// WASM parser.
fn extract_custom_section<'a>(wasm: &'a [u8], name: &str) -> Option<&'a [u8]> {
    if wasm.len() < 8 || &wasm[0..4] != b"\0asm" {
        return None;
    }
    let mut pos = 8usize;
    while pos < wasm.len() {
        let id = *wasm.get(pos)?;
        pos += 1;
        let size = read_leb128_u32(wasm, &mut pos)? as usize;
        let section_start = pos;
        let section_end = section_start.checked_add(size)?;
        if section_end > wasm.len() {
            return None;
        }
        if id == 0 {
            let mut p = section_start;
            let name_len = read_leb128_u32(wasm, &mut p)? as usize;
            if let Some(name_end) = p.checked_add(name_len) {
                if name_end <= section_end {
                    if let Ok(section_name) = std::str::from_utf8(&wasm[p..name_end]) {
                        if section_name == name {
                            return Some(&wasm[name_end..section_end]);
                        }
                    }
                }
            }
        }
        pos = section_end;
    }
    None
}

/// Decode the sequence of XDR `ScSpecEntry` values packed into the
/// `contractspecv0` custom section. Stops at the first entry that fails to
/// decode rather than erroring — a contract with no embedded spec, or one
/// this parser can't fully make sense of, degrades to an empty function list
/// instead of failing the fetch (issue #260: "handle contracts without an
/// embedded spec gracefully").
fn parse_spec_entries(wasm: &[u8]) -> Vec<ScSpecEntry> {
    let Some(section) = extract_custom_section(wasm, "contractspecv0") else {
        return Vec::new();
    };
    let mut reader = section;
    let mut entries = Vec::new();
    while !reader.is_empty() {
        let mut limited = Limited::new(&mut reader, Limits::none());
        match ScSpecEntry::read_xdr(&mut limited) {
            Ok(entry) => entries.push(entry),
            Err(_) => break,
        }
    }
    entries
}

fn spec_functions(entries: &[ScSpecEntry]) -> Vec<SpecFunction> {
    entries
        .iter()
        .filter_map(|entry| match entry {
            ScSpecEntry::FunctionV0(f) => Some(SpecFunction {
                name: f.name.to_utf8_string_lossy(),
            }),
            _ => None,
        })
        .collect()
}

/// Batch fetch of every contract's spec (issues #260, #269, #725).
///
/// Three phases so one pass costs one round-trip per distinct code hash
/// instead of one per contract:
///
/// 1. Resolve every contract's deployed code hash with a single
///    `getLedgerEntries` over the instance entries (the transport chunks it,
///    issue #725).
/// 2. For each distinct code hash not already in `cache`, download and parse
///    the WASM once — contracts sharing deployed code (many token instances)
///    pay for nothing extra, and a hash the cache already holds costs no RPC
///    call at all.
/// 3. Rebuild each [`ContractSpec`] from those functions and classify its
///    interfaces.
///
/// Best-effort: an absent instance entry (archived, never written, a Stellar
/// Asset Contract) or a WASM fetch that fails yields `None` for the contracts
/// it covers, and the caller retries on the next pass — the caller sees a
/// per-contract `None`, not a failure. `Err` is reserved for the instance-key
/// lookup itself failing, which skips the whole pass.
pub async fn fetch_contract_specs(
    rpc: &RpcClient,
    cache: &SpecCache,
    contract_ids: &[String],
) -> Result<Vec<(String, Option<ContractSpec>)>, TridentError> {
    if contract_ids.is_empty() {
        return Ok(Vec::new());
    }

    // Phase 1: instance entries for every contract, in one call.
    let mut key_to_contract: HashMap<String, &str> = HashMap::with_capacity(contract_ids.len());
    let mut instance_keys = Vec::with_capacity(contract_ids.len());
    for contract_id in contract_ids {
        let key = encode_key(&LedgerKey::ContractData(LedgerKeyContractData {
            contract: contract_address(contract_id)?,
            key: ScVal::LedgerKeyContractInstance,
            durability: ContractDataDurability::Persistent,
        }))?;
        if key_to_contract.insert(key.clone(), contract_id).is_none() {
            instance_keys.push(key);
        }
    }

    let entries = rpc.get_ledger_entries(&instance_keys).await?;

    // Map each contract to its deployed hash. An entry we cannot decode is
    // logged and treated like a missing one: nothing to persist, rather than
    // a poison pass for every other contract in the batch.
    let mut hashes: HashMap<&str, Hash> = HashMap::with_capacity(contract_ids.len());
    for entry in &entries {
        let Some(contract_id) = key_to_contract.get(entry.key.as_str()).copied() else {
            continue;
        };
        let hash = match decode_entry(&entry.xdr) {
            Ok(LedgerEntryData::ContractData(contract_data)) => match contract_data.val {
                ScVal::ContractInstance(ScContractInstance { executable, .. }) => {
                    match executable {
                        ContractExecutable::Wasm(hash) => Some(hash),
                        ContractExecutable::StellarAsset => None,
                    }
                }
                _ => None,
            },
            Ok(_) => None,
            Err(e) => {
                tracing::warn!(
                    contract_id,
                    error = %e,
                    "Failed to decode contract instance entry; skipping this contract"
                );
                None
            }
        };
        if let Some(hash) = hash {
            hashes.insert(contract_id, hash);
        }
    }

    // Phase 2: one WASM fetch per distinct code hash the cache lacks.
    let mut by_hash: HashMap<String, &Hash> = HashMap::new();
    for hash in hashes.values() {
        by_hash.entry(hex::encode(hash.0)).or_insert(hash);
    }
    for (code_hash, hash) in by_hash {
        if cache.get(&code_hash).is_some() {
            continue;
        }
        match fetch_wasm(rpc, hash).await {
            Ok(wasm) => {
                cache.insert(code_hash, spec_functions(&parse_spec_entries(&wasm)));
            }
            Err(e) => {
                // Not cached, so the next pass retries this hash. Contracts
                // mapping to it get `None` below rather than a stale spec.
                tracing::warn!(
                    code_hash,
                    error = %e,
                    "Failed to fetch contract WASM; will retry next pass"
                );
            }
        }
    }

    // Phase 3: classify from the cached functions.
    let mut specs = Vec::with_capacity(contract_ids.len());
    for contract_id in contract_ids {
        let Some(hash) = hashes.get(contract_id.as_str()) else {
            specs.push((contract_id.clone(), None));
            continue;
        };
        let code_hash = hex::encode(hash.0);
        let Some(functions) = cache.get(&code_hash) else {
            specs.push((contract_id.clone(), None));
            continue;
        };
        let function_names: Vec<&str> = functions.iter().map(|f| f.name.as_str()).collect();
        let detected = interfaces::detect_interfaces(&function_names);
        specs.push((
            contract_id.clone(),
            Some(ContractSpec {
                code_hash,
                has_spec: !functions.is_empty(),
                functions,
                contract_type: detected.contract_type.to_string(),
                interfaces: detected.interfaces,
            }),
        ));
    }
    Ok(specs)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// Build a minimal valid WASM module (magic + version, no sections).
    pub(crate) fn empty_wasm() -> Vec<u8> {
        vec![0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00]
    }

    #[test]
    fn extract_custom_section_returns_none_when_absent() {
        assert!(extract_custom_section(&empty_wasm(), "contractspecv0").is_none());
    }

    #[test]
    fn extract_custom_section_returns_none_for_non_wasm_bytes() {
        assert!(extract_custom_section(b"not wasm", "contractspecv0").is_none());
    }

    /// Encode a WASM custom section (id 0) with the given name and payload,
    /// appended after a minimal module header.
    pub(crate) fn wasm_with_custom_section(name: &str, payload: &[u8]) -> Vec<u8> {
        let mut section = Vec::new();
        write_leb128_u32(&mut section, name.len() as u32);
        section.extend_from_slice(name.as_bytes());
        section.extend_from_slice(payload);

        let mut wasm = empty_wasm();
        wasm.push(0); // custom section id
        write_leb128_u32(&mut wasm, section.len() as u32);
        wasm.extend_from_slice(&section);
        wasm
    }

    pub(crate) fn write_leb128_u32(buf: &mut Vec<u8>, mut value: u32) {
        loop {
            let byte = (value & 0x7f) as u8;
            value >>= 7;
            if value == 0 {
                buf.push(byte);
                break;
            }
            buf.push(byte | 0x80);
        }
    }

    #[test]
    fn extract_custom_section_finds_named_payload() {
        let wasm = wasm_with_custom_section("contractspecv0", &[1, 2, 3]);
        let section = extract_custom_section(&wasm, "contractspecv0").expect("section present");
        assert_eq!(section, &[1, 2, 3]);
    }

    #[test]
    fn extract_custom_section_ignores_other_names() {
        let wasm = wasm_with_custom_section("name", &[9, 9]);
        assert!(extract_custom_section(&wasm, "contractspecv0").is_none());
    }

    #[test]
    fn parse_spec_entries_decodes_a_function_entry() {
        use stellar_xdr::curr::{ScSpecFunctionV0, ScSymbol, StringM, VecM};

        let entry = ScSpecEntry::FunctionV0(ScSpecFunctionV0 {
            doc: StringM::default(),
            name: ScSymbol::try_from("transfer".to_string()).unwrap(),
            inputs: VecM::default(),
            outputs: VecM::default(),
        });
        let mut buf = Vec::new();
        entry
            .write_xdr(&mut Limited::new(&mut buf, Limits::none()))
            .unwrap();

        let wasm = wasm_with_custom_section("contractspecv0", &buf);
        let entries = parse_spec_entries(&wasm);
        let functions = spec_functions(&entries);

        assert_eq!(functions.len(), 1);
        assert_eq!(functions[0].name, "transfer");
    }

    #[test]
    fn parse_spec_entries_empty_when_no_section() {
        assert!(parse_spec_entries(&empty_wasm()).is_empty());
    }

    /// WASM containing a single `contractspecv0` function named `name`.
    pub(crate) fn wasm_with_function(name: &str) -> Vec<u8> {
        use stellar_xdr::curr::{ScSpecFunctionV0, ScSymbol, StringM, VecM};

        let entry = ScSpecEntry::FunctionV0(ScSpecFunctionV0 {
            doc: StringM::default(),
            name: ScSymbol::try_from(name.to_string()).unwrap(),
            inputs: VecM::default(),
            outputs: VecM::default(),
        });
        let mut buf = Vec::new();
        entry
            .write_xdr(&mut Limited::new(&mut buf, Limits::none()))
            .unwrap();
        wasm_with_custom_section("contractspecv0", &buf)
    }

    pub(crate) fn contract_strkey(seed: u8) -> String {
        format!("{}", stellar_strkey::Contract([seed; 32]))
    }

    fn add_entry(entries: &mut HashMap<String, String>, entry: (String, String)) {
        entries.insert(entry.0, entry.1);
    }

    pub(crate) fn encode_entry(data: &LedgerEntryData) -> String {
        let mut buf = Vec::new();
        data.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
            .unwrap();
        STANDARD.encode(buf)
    }

    pub(crate) fn instance_ledger_entry(contract_id: &str, hash: &Hash) -> (String, String) {
        use stellar_xdr::curr::{ContractDataEntry, ExtensionPoint};

        let contract = contract_address(contract_id).unwrap();
        let key = LedgerKey::ContractData(LedgerKeyContractData {
            contract: contract.clone(),
            key: ScVal::LedgerKeyContractInstance,
            durability: ContractDataDurability::Persistent,
        });
        let data = LedgerEntryData::ContractData(ContractDataEntry {
            ext: ExtensionPoint::V0,
            contract,
            key: ScVal::LedgerKeyContractInstance,
            durability: ContractDataDurability::Persistent,
            val: ScVal::ContractInstance(ScContractInstance {
                executable: ContractExecutable::Wasm(hash.clone()),
                storage: None,
            }),
        });
        (encode_key(&key).unwrap(), encode_entry(&data))
    }

    pub(crate) fn code_ledger_entry(hash: &Hash, wasm: &[u8]) -> (String, String) {
        use stellar_xdr::curr::{BytesM, ContractCodeEntry, ContractCodeEntryExt};

        let key = LedgerKey::ContractCode(LedgerKeyContractCode { hash: hash.clone() });
        let data = LedgerEntryData::ContractCode(ContractCodeEntry {
            ext: ContractCodeEntryExt::V0,
            hash: hash.clone(),
            code: BytesM::try_from(wasm.to_vec()).unwrap(),
        });
        (encode_key(&key).unwrap(), encode_entry(&data))
    }

    /// `getLedgerEntries` responder for a fixed ledger-entry dictionary,
    /// decoding each requested key only to count code-entry requests.
    pub(crate) struct LedgerEntriesResponder {
        pub(crate) entries: HashMap<String, String>,
        pub(crate) code_requests: std::sync::Arc<std::sync::atomic::AtomicUsize>,
    }

    impl wiremock::Respond for LedgerEntriesResponder {
        fn respond(&self, request: &wiremock::Request) -> wiremock::ResponseTemplate {
            use std::io::Cursor;

            let body: serde_json::Value = serde_json::from_slice(&request.body).unwrap();
            let mut response_entries = Vec::new();
            for key in body["params"]["keys"].as_array().unwrap() {
                let key = key.as_str().unwrap();
                let key_xdr = STANDARD.decode(key).unwrap();
                let decoded =
                    LedgerKey::read_xdr(&mut Limited::new(Cursor::new(key_xdr), Limits::none()))
                        .unwrap();
                if matches!(decoded, LedgerKey::ContractCode(_)) {
                    self.code_requests
                        .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                }
                if let Some(xdr) = self.entries.get(key) {
                    response_entries.push(serde_json::json!({ "key": key, "xdr": xdr }));
                }
            }
            wiremock::ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 5,
                "result": { "entries": response_entries }
            }))
        }
    }

    /// A pass resolves every contract's code hash with one request, fetches
    /// each distinct WASM at most once, skips hashes the cache already holds,
    /// and yields per-contract `None` instead of failing the batch (issue
    /// #725).
    #[tokio::test]
    async fn fetch_contract_specs_batches_and_dedupes_wasm_fetches() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        use std::sync::Arc;
        use wiremock::matchers::method;
        use wiremock::{Mock, MockServer};

        let shared_hash = Hash([0x11; 32]);
        let broken_hash = Hash([0x22; 32]);
        let cached_hash = Hash([0x33; 32]);

        let contract_a = contract_strkey(0xa1);
        let contract_b = contract_strkey(0xb2);
        let contract_c = contract_strkey(0xc3);
        let contract_d = contract_strkey(0xd4);
        let contract_missing = contract_strkey(0xe5);

        let mut entries = HashMap::new();
        add_entry(
            &mut entries,
            instance_ledger_entry(&contract_a, &shared_hash),
        );
        add_entry(
            &mut entries,
            instance_ledger_entry(&contract_b, &shared_hash),
        );
        add_entry(
            &mut entries,
            instance_ledger_entry(&contract_c, &broken_hash),
        );
        add_entry(
            &mut entries,
            instance_ledger_entry(&contract_d, &cached_hash),
        );
        // `contract_missing` deliberately has no instance entry.
        add_entry(
            &mut entries,
            code_ledger_entry(&shared_hash, &wasm_with_function("hello")),
        );
        // Present but not valid base64: the WASM fetch for this hash fails,
        // and only the contract that maps to it should end up `None`.
        entries.insert(
            code_ledger_entry(&broken_hash, b"irrelevant").0,
            "%%not-base64%%".to_string(),
        );

        let code_requests = Arc::new(AtomicUsize::new(0));
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(LedgerEntriesResponder {
                entries,
                code_requests: code_requests.clone(),
            })
            .mount(&server)
            .await;

        let rpc = RpcClient::with_settings(server.uri(), &crate::rpc::RpcHttpSettings::default())
            .unwrap();
        let cache = SpecCache::new();
        cache.insert(
            hex::encode(cached_hash.0),
            vec![SpecFunction {
                name: "seeded_fn".to_string(),
            }],
        );

        let contract_ids = vec![
            contract_a.clone(),
            contract_b.clone(),
            contract_c.clone(),
            contract_d.clone(),
            contract_missing.clone(),
        ];
        let specs = fetch_contract_specs(&rpc, &cache, &contract_ids)
            .await
            .expect("batch fetch must succeed even when one contract is unresolvable");

        assert_eq!(specs.len(), contract_ids.len());
        assert_eq!(specs[0].0, contract_a);
        assert_eq!(specs[1].0, contract_b);

        let spec_a = specs[0].1.as_ref().expect("contract a has a spec");
        assert_eq!(spec_a.code_hash, hex::encode(shared_hash.0));
        assert_eq!(spec_a.functions.len(), 1);
        assert_eq!(spec_a.functions[0].name, "hello");
        // Contract b shares a's code hash and is served from the cache the
        // first contract populated during this same pass.
        let spec_b = specs[1].1.as_ref().expect("contract b has a spec");
        assert_eq!(spec_b.code_hash, spec_a.code_hash);
        assert_eq!(spec_b.functions[0].name, "hello");

        assert!(
            specs[2].1.is_none(),
            "broken WASM fetch yields None, not an error"
        );

        let spec_d = specs[3].1.as_ref().expect("contract d has a spec");
        assert_eq!(spec_d.code_hash, hex::encode(cached_hash.0));
        assert_eq!(spec_d.functions[0].name, "seeded_fn");

        assert!(specs[4].1.is_none(), "missing instance entry yields None");

        // One request for the shared hash + one attempt for the broken hash;
        // the preseeded hash costs no request at all.
        assert_eq!(code_requests.load(Ordering::SeqCst), 2);
    }
}
