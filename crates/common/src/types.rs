use serde::{Deserialize, Serialize};
use std::str::FromStr;

// ---------------------------------------------------------------------------
// Issue #271 — Contract liveness / TTL tracking
// ---------------------------------------------------------------------------

/// Whether a contract's ledger entry is currently live or has been archived.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum LivenessStatus {
    Live,
    Archived,
}

/// TTL and liveness snapshot for a tracked contract entry (issue #271).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractLiveness {
    pub contract_id: String,
    pub status: LivenessStatus,
    /// The ledger sequence after which the entry is archived (`liveUntilLedger`
    /// from `getLedgerEntries`). `None` when the entry is already archived.
    pub live_until_ledger: Option<u32>,
    /// Ledgers remaining until archival (`live_until_ledger - current_ledger`).
    /// Negative when already archived. `None` when unknown.
    pub ledgers_until_archive: Option<i64>,
    pub last_checked_ledger: u32,
}

impl ContractLiveness {
    pub fn from_rpc(
        contract_id: String,
        current_ledger: u32,
        live_until_ledger: Option<u32>,
        last_checked_ledger: u32,
    ) -> Self {
        let ledgers_until_archive = live_until_ledger.map(|l| l as i64 - current_ledger as i64);
        let status = match ledgers_until_archive {
            Some(n) if n > 0 => LivenessStatus::Live,
            _ => LivenessStatus::Archived,
        };
        Self {
            contract_id,
            status,
            live_until_ledger,
            ledgers_until_archive,
            last_checked_ledger,
        }
    }

    /// Returns `true` when the entry will be archived within `threshold` ledgers.
    pub fn is_near_archival(&self, threshold: u32) -> bool {
        matches!(self.ledgers_until_archive, Some(n) if n >= 0 && n <= threshold as i64)
    }
}

// ---------------------------------------------------------------------------
// Issue #273 — Source verification
// ---------------------------------------------------------------------------

/// Verification state for a deployed contract's source code (issue #273).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum VerificationStatus {
    Unverified,
    Pending,
    Verified,
    Mismatch,
    Failed,
}

/// Metadata about the source build submitted for verification (issue #273).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SourceBuildMetadata {
    pub repository_url: String,
    pub commit_sha: String,
    pub toolchain_version: String,
    pub build_command: String,
    pub wasm_path: String,
}

/// Source-verification record for a contract (issue #273).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractVerification {
    pub contract_id: String,
    pub status: VerificationStatus,
    /// SHA-256 hash of the WASM bytecode currently deployed on-chain.
    pub on_chain_hash: String,
    /// SHA-256 hash produced by the submitted source build, if available.
    pub source_hash: Option<String>,
    pub build_metadata: Option<SourceBuildMetadata>,
    /// ISO 8601 UTC timestamp of the last successful verification.
    pub verified_at: Option<String>,
}

impl ContractVerification {
    /// Compare the on-chain WASM hash to the hash produced by a source build.
    pub fn compare_hashes(on_chain: &str, produced: &str) -> VerificationStatus {
        if on_chain == produced {
            VerificationStatus::Verified
        } else {
            VerificationStatus::Mismatch
        }
    }
}

/// Distinguishes the three event categories emitted by the Soroban runtime.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum EventType {
    /// Emitted explicitly by contract code via `env.events().publish(...)`.
    Contract,
    /// Emitted by the Soroban host itself (e.g. fee events).
    System,
    /// Emitted only when diagnostic mode is enabled; never stored by default.
    Diagnostic,
}

/// Normalised representation of a single Soroban event as stored in PostgreSQL
/// and published onto Redis Streams.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SorobanEvent {
    /// Strkey-encoded contract address (C...).
    pub contract_id: String,
    /// Ordered list of topic values, XDR-decoded to their string representations.
    pub topics: Vec<String>,
    /// Decoded event body. Scalar XDR types are coerced to JSON primitives;
    /// map/vec types become JSON objects/arrays.
    pub data: serde_json::Value,
    /// Ledger sequence number in which this event was emitted.
    pub ledger_sequence: u64,
    /// ISO 8601 UTC timestamp of the ledger close.
    pub ledger_timestamp: String,
    /// Hash of the transaction that emitted this event.
    pub transaction_hash: String,
    /// Zero-based index of this event within its transaction, made unique
    /// within a batch by `assign_unique_event_indexes` when the RPC reports
    /// (or falls back to) a colliding value. Stored for the
    /// `(ledger_sequence, transaction_hash, event_index, network)` natural
    /// key constraint (migration 0025).
    ///
    /// NOT stable across re-indexing runs with different batch/page
    /// boundaries (issue #599) — do not derive an identifier from this
    /// field. Use `raw_event_index` for that.
    pub event_index: u32,
    /// The event's position as reported (or derived) directly from the RPC,
    /// before any batch-local tie-breaking. Depends only on the event
    /// itself, so it is stable across re-indexing runs regardless of batch
    /// composition (issue #599) — this, not `event_index`, is what
    /// `event_uuid` must be derived from.
    pub raw_event_index: u32,
    /// Category of event as reported by the Soroban host.
    pub event_type: EventType,
}

// ---------------------------------------------------------------------------
// Issue #252 — network as a typed value
// ---------------------------------------------------------------------------

/// Known Stellar networks supported by Trident.
///
/// The string forms are the values stored in the `network` column and enforced
/// by the CHECK constraints added in migration 0031. Keep the two in sync: a
/// value accepted here but rejected by the constraint fails at write time.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Network {
    Mainnet,
    Testnet,
    Futurenet,
    Sandbox,
}

impl Network {
    pub const MAINNET: &'static str = "mainnet";
    pub const TESTNET: &'static str = "testnet";
    pub const FUTURENET: &'static str = "futurenet";
    pub const SANDBOX: &'static str = "sandbox";

    pub fn as_str(&self) -> &'static str {
        match self {
            Network::Mainnet => Self::MAINNET,
            Network::Testnet => Self::TESTNET,
            Network::Futurenet => Self::FUTURENET,
            Network::Sandbox => Self::SANDBOX,
        }
    }

    pub fn all() -> &'static [&'static str] {
        &[Self::MAINNET, Self::TESTNET, Self::FUTURENET, Self::SANDBOX]
    }
}

impl FromStr for Network {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.to_lowercase().as_str() {
            "mainnet" => Ok(Network::Mainnet),
            "testnet" => Ok(Network::Testnet),
            "futurenet" => Ok(Network::Futurenet),
            "sandbox" => Ok(Network::Sandbox),
            other => Err(anyhow::anyhow!(
                "invalid network: {other}. Expected one of: mainnet, testnet, futurenet, sandbox"
            )),
        }
    }
}

impl std::fmt::Display for Network {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.as_str())
    }
}
