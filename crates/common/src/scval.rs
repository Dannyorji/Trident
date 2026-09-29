//! # Shared ScVal decoding
//!
//! The one place Soroban `ScVal`s are rendered into strings (topics) and JSON
//! (event bodies), used by every component that writes decoded values —
//! `trident-indexer`'s live parser and `trident-backfill`'s re-ingest path
//! (issue #506).
//!
//! Before this module existed the backfill crate carried a stale copy of the
//! indexer's decoder: it predated the exact U256/I256 rendering from #415 and
//! lacked the Timepoint/Duration/Error arms, so a backfilled event could store
//! *different* values than the live path stored for the same XDR. A shared
//! decoder makes that divergence structurally impossible.
//!
//! ## Coverage contract
//!
//! Every match here is **exhaustive with no wildcard arm**. `ScVal` is a
//! closed enum in `stellar-xdr`, so a new variant introduced by an XDR
//! upgrade fails compilation instead of silently degrading to a `Debug`
//! string in production — the loudest possible failure mode, caught before
//! the code can ship (issue #506; the previous `other =>` catch-alls coerced
//! unknown variants to `Debug` strings and only bumped a metric nothing
//! alerted on).
//!
//! Variants that are *representable but never legitimately appear in event
//! payloads* (`ContractInstance`, `LedgerKeyContractInstance`,
//! `LedgerKeyNonce`) are decoded faithfully into tagged JSON objects AND
//! surfaced: a `tracing::warn!` names the variant and
//! [`UNEXPECTED_SCVAL_VARIANT_TOTAL`] counts it, so an anomaly in testnet
//! traffic reaches the operator instead of hiding inside a stored blob.

use base64::{engine::general_purpose::STANDARD, Engine};
use serde_json::Value as Json;
use stellar_strkey::{ed25519, Contract};
use stellar_xdr::curr::{
    AccountId, ClaimableBalanceId, ContractExecutable, ContractId, Limited, Limits, PublicKey,
    ReadXdr, ScAddress, ScContractInstance, ScVal,
};

use crate::TridentError;

/// Counter bumped whenever a structurally valid but anomalous-in-context
/// variant (`ContractInstance` / `LedgerKeyContractInstance` /
/// `LedgerKeyNonce`) is decoded from an event payload. Described and seeded
/// by the indexer's metrics installer; alerted on by
/// `TridentIndexerUnexpectedScValVariant` (monitoring/alerts.yml).
///
/// Emitted through the global `metrics` recorder: in binaries that install
/// one (the indexer) it lands in Prometheus; in binaries that do not (the
/// backfill CLI) it is a no-op and the `tracing::warn!` still fires.
pub const UNEXPECTED_SCVAL_VARIANT_TOTAL: &str = "trident_scval_unexpected_variant_total";

fn record_unexpected_variant(context: &str, variant: &str) {
    tracing::warn!(
        variant,
        context,
        "decoded an ScVal variant that should not appear in event payloads; \
         stored faithfully, review the emitting contract"
    );
    metrics::counter!(UNEXPECTED_SCVAL_VARIANT_TOTAL).increment(1);
}

/// Maximum XDR reader recursion depth accepted when decoding event XDR
/// (issue #507).
///
/// This bounds `stellar-xdr` READER FRAMES, not container levels: each
/// nested container holds several concurrently-active `read_xdr` frames
/// (a Vec level ~4, a Map level ~5), so a frame budget of 500 admits
/// roughly 100+ nested container levels. That is deliberate: the Soroban
/// host itself permits contract values up to 100 container levels
/// (`DEFAULT_HOST_DEPTH_LIMIT`) and pairs that with an XDR read/write
/// depth of 500 (`DEFAULT_XDR_RW_LIMITS`) for exactly this frame
/// multiplier — mirroring those numbers means every protocol-legal event
/// decodes while a hostile payload nested beyond anything the chain can
/// produce still yields a handled error instead of a stack-overflow abort
/// (the failure mode `Limits::depth` exists to prevent). Rendering
/// recursion in `scval_to_json` is bounded by the same value, since a
/// decoded value cannot be deeper than its wire form.
pub const MAX_SCVAL_DEPTH: u32 = 500;

/// Maximum decoded size in bytes of a single event XDR value (issue #507).
/// Real Soroban event payloads are a few hundred bytes; 2 MiB is orders of
/// magnitude of headroom while bounding what a hostile length claim can
/// make the decoder allocate or scan.
pub const MAX_SCVAL_BYTES: usize = 2 * 1024 * 1024;

// Base64 expands 3 bytes to 4 characters; anything longer than this cannot
// decode to an in-budget payload, so it is rejected before the base64
// decoder allocates for it.
const MAX_SCVAL_B64_LEN: usize = MAX_SCVAL_BYTES.div_ceil(3) * 4 + 4;

/// Decode a base64-encoded XDR `ScVal` as returned by Soroban RPC.
///
/// Hardened against hostile input (issue #507): input size and container
/// depth are bounded (see [`MAX_SCVAL_BYTES`], [`MAX_SCVAL_DEPTH`]), the
/// byte-length limit handed to the XDR reader is the actual input length so
/// a lying length prefix cannot make it read past the payload, and trailing
/// bytes after the value are rejected — a value that decodes but leaves
/// bytes behind is malformed, and accepting it here while the
/// testnet-correctness reference path (`ScVal::from_xdr`) rejects it would
/// let production and verification disagree about the same wire bytes.
pub fn decode_scval(b64: &str) -> Result<ScVal, TridentError> {
    if b64.len() > MAX_SCVAL_B64_LEN {
        return Err(TridentError::parse(anyhow::anyhow!(
            "event XDR too large: {} base64 chars (limit {MAX_SCVAL_B64_LEN})",
            b64.len()
        )));
    }
    let bytes = STANDARD
        .decode(b64)
        .map_err(|e| TridentError::parse(anyhow::Error::new(e).context("base64 decode")))?;
    if bytes.len() > MAX_SCVAL_BYTES {
        return Err(TridentError::parse(anyhow::anyhow!(
            "event XDR too large: {} bytes (limit {MAX_SCVAL_BYTES})",
            bytes.len()
        )));
    }
    let len = bytes.len();
    let mut cursor = std::io::Cursor::new(bytes);
    let val = ScVal::read_xdr(&mut Limited::new(
        &mut cursor,
        Limits {
            depth: MAX_SCVAL_DEPTH,
            len,
        },
    ))
    .map_err(|e| TridentError::parse(anyhow::Error::new(e).context("XDR decode ScVal")))?;
    let consumed = cursor.position() as usize;
    if consumed != len {
        return Err(TridentError::parse(anyhow::anyhow!(
            "trailing bytes after ScVal: consumed {consumed} of {len}"
        )));
    }
    Ok(val)
}

/// Convert a topic `ScVal` to a compact string representation.
pub fn scval_to_string(val: &ScVal) -> String {
    match val {
        ScVal::Symbol(s) => s.to_utf8_string_lossy(),
        ScVal::String(s) => s.to_utf8_string_lossy(),
        ScVal::Bool(b) => b.to_string(),
        ScVal::Void => "void".into(),
        ScVal::U32(n) => n.to_string(),
        ScVal::I32(n) => n.to_string(),
        ScVal::U64(n) => n.to_string(),
        ScVal::I64(n) => n.to_string(),
        ScVal::U128(parts) => {
            let val = ((parts.hi as u128) << 64) | (parts.lo as u128);
            val.to_string()
        }
        ScVal::I128(parts) => {
            let val = ((parts.hi as i128) << 64) | (parts.lo as i128);
            val.to_string()
        }
        ScVal::U256(parts) => {
            u256_limbs_to_decimal([parts.hi_hi, parts.hi_lo, parts.lo_hi, parts.lo_lo])
        }
        ScVal::I256(parts) => {
            i256_limbs_to_decimal(parts.hi_hi, parts.hi_lo, parts.lo_hi, parts.lo_lo)
        }
        ScVal::Bytes(b) => hex::encode(b.as_slice()),
        ScVal::Address(addr) => scaddress_to_string(addr),
        // Timepoint and Duration are u64 newtypes; without these arms they fell
        // through to the debug catch-all and rendered as "Timepoint(1700000000)"
        // rather than a usable value, while also tripping the
        // unhandled-variant metric on well-understood types (issue #415).
        ScVal::Timepoint(t) => t.0.to_string(),
        ScVal::Duration(d) => d.0.to_string(),
        // A contract error in topic/data position. Rendered via Debug
        // deliberately: the variant carries a code whose meaning is
        // contract-defined, so there is no stable scalar to project it to.
        ScVal::Error(e) => format!("{e:?}"),
        // Containers in topic position: compact bracketed forms, matching the
        // shapes issue #209 established (and its golden tests pin) — these
        // previously hit the Debug catch-all and wrongly tripped the
        // unhandled-variant metric (issue #506).
        ScVal::Vec(Some(items)) => format!(
            "[{}]",
            items
                .iter()
                .map(scval_to_string)
                .collect::<Vec<_>>()
                .join(",")
        ),
        ScVal::Vec(None) => "Vec(None)".to_string(),
        ScVal::Map(Some(entries)) => format!(
            "{{{}}}",
            entries
                .iter()
                .map(|e| format!("{}:{}", scval_to_string(&e.key), scval_to_string(&e.val)))
                .collect::<Vec<_>>()
                .join(",")
        ),
        ScVal::Map(None) => "Map(None)".to_string(),
        // Complex host-object types: #209's compact topic forms — the variant
        // name with just enough payload to be useful; full structure only via
        // scval_to_json. Still surfaced via warn + metric (issue #506): these
        // are structurally valid but anomalous in an event payload.
        ScVal::ContractInstance(inst) => {
            record_unexpected_variant("scval_to_string", "ContractInstance");
            match &inst.executable {
                ContractExecutable::Wasm(hash) => {
                    format!("contract_instance(wasm:{})", hex::encode(hash.0))
                }
                ContractExecutable::StellarAsset => "contract_instance(stellar_asset)".to_string(),
            }
        }
        ScVal::LedgerKeyContractInstance => {
            record_unexpected_variant("scval_to_string", "LedgerKeyContractInstance");
            "ledger_key_contract_instance".to_string()
        }
        ScVal::LedgerKeyNonce(nonce) => {
            record_unexpected_variant("scval_to_string", "LedgerKeyNonce");
            nonce.nonce.to_string()
        }
    }
}

/// Recursively convert a `ScVal` to a `serde_json::Value` for the event body.
pub fn scval_to_json(val: &ScVal) -> Json {
    match val {
        ScVal::Void => Json::Null,
        ScVal::Bool(b) => Json::Bool(*b),
        ScVal::Symbol(s) => Json::String(s.to_utf8_string_lossy()),
        ScVal::String(s) => Json::String(s.to_utf8_string_lossy()),
        ScVal::U32(n) => Json::from(*n),
        ScVal::I32(n) => Json::from(*n),
        ScVal::U64(n) => Json::from(*n),
        ScVal::I64(n) => Json::from(*n),
        ScVal::U128(parts) => {
            let v = ((parts.hi as u128) << 64) | (parts.lo as u128);
            // Use string for values that overflow JSON's safe integer range
            if v <= u64::MAX as u128 {
                Json::from(v as u64)
            } else {
                Json::String(v.to_string())
            }
        }
        ScVal::I128(parts) => {
            let v = ((parts.hi as i128) << 64) | (parts.lo as i128);
            if v >= i64::MIN as i128 && v <= i64::MAX as i128 {
                Json::from(v as i64)
            } else {
                Json::String(v.to_string())
            }
        }
        ScVal::U256(parts) => Json::String(u256_limbs_to_decimal([
            parts.hi_hi,
            parts.hi_lo,
            parts.lo_hi,
            parts.lo_lo,
        ])),
        ScVal::I256(parts) => Json::String(i256_limbs_to_decimal(
            parts.hi_hi,
            parts.hi_lo,
            parts.lo_hi,
            parts.lo_lo,
        )),
        ScVal::Bytes(b) => Json::String(hex::encode(b.as_slice())),
        ScVal::Address(addr) => Json::String(scaddress_to_string(addr)),
        // u64-valued, so emitted as strings for the same reason U64/I64 are:
        // values above 2^53 do not survive a JSON number round-trip through a
        // JavaScript consumer (issue #415).
        ScVal::Timepoint(t) => Json::String(t.0.to_string()),
        ScVal::Duration(d) => Json::String(d.0.to_string()),
        ScVal::Error(e) => Json::String(format!("{e:?}")),
        ScVal::Vec(Some(items)) => Json::Array(items.iter().map(scval_to_json).collect()),
        ScVal::Vec(None) => Json::Array(vec![]),
        ScVal::Map(Some(entries)) => scmap_to_json(entries),
        ScVal::Map(None) => Json::Object(serde_json::Map::new()),
        // Complex host-object types, in #209's documented JSON shapes (see
        // docs/indexer/scval-json-mapping.md) — decoded faithfully, never
        // coerced to Debug strings — and still surfaced via warn + metric
        // (issue #506): structurally valid, but no well-behaved contract
        // emits them in an event payload.
        ScVal::ContractInstance(instance) => {
            record_unexpected_variant("scval_to_json", "ContractInstance");
            contract_instance_to_json(instance)
        }
        ScVal::LedgerKeyContractInstance => {
            record_unexpected_variant("scval_to_json", "LedgerKeyContractInstance");
            Json::String("ledger_key_contract_instance".into())
        }
        ScVal::LedgerKeyNonce(key) => {
            record_unexpected_variant("scval_to_json", "LedgerKeyNonce");
            // The nonce is i64; emitted as a string like every other value
            // that cannot survive a JavaScript number round-trip.
            serde_json::json!({ "nonce": key.nonce.to_string() })
        }
    }
}

/// Convert a decoded `ScMap` to a JSON object. Shared by `ScVal::Map` and
/// `ScVal::ContractInstance`'s `storage` field, which is the same underlying
/// type (issue #209).
fn scmap_to_json(entries: &stellar_xdr::curr::ScMap) -> Json {
    let obj: serde_json::Map<String, Json> = entries
        .iter()
        .map(|e| (scval_to_string(&e.key), scval_to_json(&e.val)))
        .collect();
    Json::Object(obj)
}

/// Faithful JSON rendering of a `ContractInstance` value, in #209's shape:
/// the executable reference plus its instance storage decoded with the same
/// rules as any other map.
fn contract_instance_to_json(instance: &ScContractInstance) -> Json {
    let executable = match &instance.executable {
        ContractExecutable::Wasm(hash) => {
            let mut m = serde_json::Map::new();
            m.insert("type".into(), Json::String("wasm".into()));
            m.insert("wasm_hash".into(), Json::String(hex::encode(hash.0)));
            Json::Object(m)
        }
        ContractExecutable::StellarAsset => {
            let mut m = serde_json::Map::new();
            m.insert("type".into(), Json::String("stellar_asset".into()));
            Json::Object(m)
        }
    };
    let storage = match &instance.storage {
        Some(entries) => scmap_to_json(entries),
        None => Json::Null,
    };
    let mut obj = serde_json::Map::new();
    obj.insert("executable".into(), executable);
    obj.insert("storage".into(), storage);
    Json::Object(obj)
}

/// Render any `ScAddress` as its canonical strkey.
///
/// Exhaustive: the muxed-account (`M...`), claimable-balance (`B...`), and
/// liquidity-pool (`L...`) address forms added in stellar-xdr 26.x render as
/// real strkeys rather than falling to a Debug catch-all (issue #506) — a
/// consumer can feed any address this returns straight back to Horizon/RPC.
pub fn scaddress_to_string(addr: &ScAddress) -> String {
    match addr {
        ScAddress::Account(AccountId(PublicKey::PublicKeyTypeEd25519(bytes))) => {
            // stellar-strkey 0.0.16+ returns heapless::String — convert to std::String
            ed25519::PublicKey(bytes.0).to_string().as_str().to_owned()
        }
        // stellar-xdr 26.x wraps the hash in ContractId; the inner Hash holds [u8; 32]
        ScAddress::Contract(ContractId(hash)) => Contract(hash.0).to_string().as_str().to_owned(),
        ScAddress::MuxedAccount(muxed) => ed25519::MuxedAccount {
            ed25519: muxed.ed25519.0,
            id: muxed.id,
        }
        .to_string()
        .as_str()
        .to_owned(),
        ScAddress::ClaimableBalance(ClaimableBalanceId::ClaimableBalanceIdTypeV0(hash)) => {
            stellar_strkey::ClaimableBalance::V0(hash.0)
                .to_string()
                .as_str()
                .to_owned()
        }
        ScAddress::LiquidityPool(pool_id) => stellar_strkey::LiquidityPool(pool_id.0 .0)
            .to_string()
            .as_str()
            .to_owned(),
    }
}

/// Render a 256-bit unsigned value, supplied as four 64-bit limbs
/// (most-significant first), as a decimal string.
///
/// Rust has no u256, and the previous implementation packed all four limbs
/// into a u128 with 32-bit shifts — which both truncated the top half and
/// mis-positioned the rest, so any value above 2^128 decoded to a
/// plausible-looking but wrong number. Long multiplication over decimal
/// digits avoids needing a big-integer dependency for the one place we need
/// this (issue #415).
fn u256_limbs_to_decimal(limbs: [u64; 4]) -> String {
    // digits holds the running value, least-significant decimal digit first.
    let mut digits: Vec<u8> = vec![0];
    for limb in limbs {
        // value = value * 2^64 + limb, done as two steps over base-10 digits.
        for _ in 0..64 {
            let mut carry = 0u8;
            for d in digits.iter_mut() {
                let doubled = *d * 2 + carry;
                *d = doubled % 10;
                carry = doubled / 10;
            }
            if carry > 0 {
                digits.push(carry);
            }
        }
        let mut carry = limb as u128;
        let mut i = 0;
        while carry > 0 || i < digits.len() {
            if i == digits.len() {
                digits.push(0);
            }
            let sum = digits[i] as u128 + (carry % 10);
            digits[i] = (sum % 10) as u8;
            carry = carry / 10 + sum / 10;
            i += 1;
        }
    }
    while digits.len() > 1 && digits.last() == Some(&0) {
        digits.pop();
    }
    digits.iter().rev().map(|d| (b'0' + d) as char).collect()
}

/// Render a 256-bit signed value from its limbs. `hi_hi` is the signed
/// most-significant limb; negatives are two's complement across all 256 bits,
/// so they are negated into the unsigned domain and printed with a sign.
fn i256_limbs_to_decimal(hi_hi: i64, hi_lo: u64, lo_hi: u64, lo_lo: u64) -> String {
    if hi_hi >= 0 {
        return u256_limbs_to_decimal([hi_hi as u64, hi_lo, lo_hi, lo_lo]);
    }
    // Two's complement negate: invert all limbs, then add one with carry.
    let mut limbs = [!(hi_hi as u64), !hi_lo, !lo_hi, !lo_lo];
    for limb in limbs.iter_mut().rev() {
        let (next, overflow) = limb.overflowing_add(1);
        *limb = next;
        if !overflow {
            break;
        }
    }
    format!("-{}", u256_limbs_to_decimal(limbs))
}

#[cfg(test)]
mod tests {
    use super::*;
    use stellar_xdr::curr::{
        Hash, MuxedEd25519Account, PoolId, ScMap, ScMapEntry, ScNonceKey, ScSymbol, ScVec, Uint256,
        WriteXdr,
    };

    /// Encode a value and decode it back through the production path, proving
    /// the test exercises real XDR bytes rather than in-memory values.
    fn roundtrip(val: &ScVal) -> ScVal {
        let mut buf = Vec::new();
        val.write_xdr(&mut Limited::new(&mut buf, Limits::none()))
            .expect("encode test ScVal");
        let b64 = base64::engine::general_purpose::STANDARD.encode(buf);
        decode_scval(&b64).expect("decode test ScVal")
    }

    #[test]
    fn contract_instance_decodes_to_structured_forms_not_debug_string() {
        let storage = ScMap(
            vec![ScMapEntry {
                key: ScVal::Symbol(ScSymbol("admin".try_into().expect("symbol"))),
                val: ScVal::U32(7),
            }]
            .try_into()
            .expect("map"),
        );
        let val = ScVal::ContractInstance(ScContractInstance {
            executable: ContractExecutable::Wasm(Hash([0xAB; 32])),
            storage: Some(storage),
        });

        let json = scval_to_json(&roundtrip(&val));
        // #209's documented shape (docs/indexer/scval-json-mapping.md).
        assert_eq!(json["executable"]["type"], "wasm");
        assert_eq!(json["executable"]["wasm_hash"], hex::encode([0xAB; 32]));
        assert_eq!(json["storage"]["admin"], 7);
        // The topic form is #209's compact rendering, never a Debug dump.
        let s = scval_to_string(&val);
        assert_eq!(
            s,
            format!("contract_instance(wasm:{})", hex::encode([0xAB; 32]))
        );
        assert!(!s.contains("ScContractInstance"), "Debug leak: {s}");
    }

    #[test]
    fn ledger_key_variants_decode_to_documented_shapes() {
        let nonce = ScVal::LedgerKeyNonce(ScNonceKey { nonce: i64::MIN });
        let json = scval_to_json(&roundtrip(&nonce));
        assert_eq!(json["nonce"], i64::MIN.to_string());
        assert_eq!(scval_to_string(&nonce), i64::MIN.to_string());

        let key = ScVal::LedgerKeyContractInstance;
        let json = scval_to_json(&roundtrip(&key));
        assert_eq!(json, Json::String("ledger_key_contract_instance".into()));
    }

    #[test]
    fn containers_in_topic_position_render_compactly() {
        let vec_val = ScVal::Vec(Some(ScVec(
            vec![
                ScVal::U32(1),
                ScVal::Symbol(ScSymbol("x".try_into().expect("symbol"))),
            ]
            .try_into()
            .expect("vec"),
        )));
        // #209's compact topic form: elements joined with commas, symbols
        // rendered bare (the JSON form remains fully quoted).
        assert_eq!(scval_to_string(&roundtrip(&vec_val)), "[1,x]");
    }

    #[test]
    fn every_scaddress_form_renders_as_a_strkey() {
        let muxed = ScAddress::MuxedAccount(MuxedEd25519Account {
            id: 42,
            ed25519: Uint256([7; 32]),
        });
        let rendered = scaddress_to_string(&muxed);
        assert!(rendered.starts_with('M'), "muxed strkey, got {rendered}");

        let cb = ScAddress::ClaimableBalance(ClaimableBalanceId::ClaimableBalanceIdTypeV0(Hash(
            [9; 32],
        )));
        let rendered = scaddress_to_string(&cb);
        assert!(
            rendered.starts_with('B'),
            "claimable-balance strkey, got {rendered}"
        );

        let lp = ScAddress::LiquidityPool(PoolId(Hash([3; 32])));
        let rendered = scaddress_to_string(&lp);
        assert!(
            rendered.starts_with('L'),
            "liquidity-pool strkey, got {rendered}"
        );

        for addr in [muxed, cb, lp] {
            let s = scaddress_to_string(&addr);
            assert!(!s.contains("ScAddress"), "Debug leak: {s}");
            // Round-trip through real XDR in address position.
            let json = scval_to_json(&roundtrip(&ScVal::Address(addr)));
            assert_eq!(json, Json::String(s));
        }
    }

    // -----------------------------------------------------------------------
    // Hostile input (issue #507). Deterministic boundary cases; the
    // randomized battery lives in the indexer's parser::tests so it rides
    // the CI fuzz pass (PROPTEST_CASES=50000).
    // -----------------------------------------------------------------------

    /// Wire bytes for a Vec nested `depth` levels around a U32, built
    /// directly as bytes so tests can exceed any depth the in-memory
    /// constructors could safely build (encoding a deep value recurses too).
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

    fn decode_wire(wire: &[u8]) -> Result<ScVal, TridentError> {
        decode_scval(&base64::engine::general_purpose::STANDARD.encode(wire))
    }

    #[test]
    fn plausible_nesting_decodes_and_renders() {
        // 100 container levels — the Soroban host's own DEFAULT_HOST_DEPTH_LIMIT,
        // i.e. the deepest value a contract can legally emit. Must decode, and
        // rendering the decoded value (which recurses to the same depth) must
        // succeed too. This is the regression guard against a frame budget
        // set below what the protocol permits.
        let val = decode_wire(&nested_vec_wire(100)).expect("host-legal nesting must decode");
        let _ = scval_to_string(&val);
        let _ = scval_to_json(&val);
    }

    #[test]
    fn hostile_nesting_depth_errors_instead_of_overflowing_the_stack() {
        // Far past the budget: a handled error, never a SIGABRT. Without the
        // depth limit this input recurses ~50k frames and kills the process.
        let result = decode_wire(&nested_vec_wire(50_000));
        assert!(result.is_err(), "hostile nesting must be rejected");
    }

    #[test]
    fn trailing_bytes_after_the_value_are_rejected() {
        // A value that decodes but leaves bytes behind is malformed. The
        // testnet-correctness reference path (ScVal::from_xdr) already
        // rejects it; production must agree about the same wire bytes.
        let mut wire = Vec::new();
        ScVal::U32(7)
            .write_xdr(&mut Limited::new(&mut wire, Limits::none()))
            .expect("encode");
        wire.extend_from_slice(&[0xDE, 0xAD]);
        let err = decode_wire(&wire).expect_err("trailing bytes must be rejected");
        assert!(
            format!("{err}").contains("trailing"),
            "unexpected error: {err}"
        );
    }

    #[test]
    fn lying_length_prefix_is_a_handled_error() {
        // Bytes value claiming u32::MAX length with 4 real bytes: must fail
        // within the input-length budget, not scan or allocate 4 GiB.
        let mut wire = 13u32.to_be_bytes().to_vec(); // ScValType::Bytes
        wire.extend_from_slice(&u32::MAX.to_be_bytes());
        wire.extend_from_slice(&[0xAA; 4]);
        assert!(decode_wire(&wire).is_err());
    }

    #[test]
    fn oversized_input_is_rejected_before_base64_decode() {
        let huge = "A".repeat(MAX_SCVAL_B64_LEN + 1);
        let err = decode_scval(&huge).expect_err("oversized input must be rejected");
        assert!(
            format!("{err}").contains("too large"),
            "unexpected error: {err}"
        );
    }

    #[test]
    fn empty_input_is_a_handled_error() {
        assert!(decode_scval("").is_err());
    }

    #[test]
    fn u256_extremes_render_exact_decimals() {
        let max = ScVal::U256(stellar_xdr::curr::UInt256Parts {
            hi_hi: u64::MAX,
            hi_lo: u64::MAX,
            lo_hi: u64::MAX,
            lo_lo: u64::MAX,
        });
        assert_eq!(
            scval_to_string(&roundtrip(&max)),
            "115792089237316195423570985008687907853269984665640564039457584007913129639935"
        );
        let min_i256 = ScVal::I256(stellar_xdr::curr::Int256Parts {
            hi_hi: i64::MIN,
            hi_lo: 0,
            lo_hi: 0,
            lo_lo: 0,
        });
        assert_eq!(
            scval_to_string(&roundtrip(&min_i256)),
            "-57896044618658097711785492504343953926634992332820282019728792003956564819968"
        );
    }
}
