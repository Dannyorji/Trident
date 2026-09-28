// Example code that deserializes and serializes the model.
// extern crate serde;
// #[macro_use]
// extern crate serde_derive;
// extern crate serde_json;
//
// use generated_module::OpenAPIModels;
//
// fn main() {
//     let json = r#"{"answer": 42}"#;
//     let model: OpenAPIModels = serde_json::from_str(&json).unwrap();
// }

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct OpenApiModels {
    pub admin_key_usage_response: Option<AdminKeyUsageResponse>,

    #[serde(rename = "APIKeyResponse")]
    pub api_key_response: Option<ApiKeyResponse>,

    pub contract_call_request: Option<ContractCallRequest>,

    pub contract_call_response: Option<ContractCallResponse>,

    pub contract_event_field_schema: Option<ContractEventFieldSchema>,

    pub contract_event_schema: Option<ContractEventSchema>,

    pub contract_event_schema_response: Option<ContractEventSchemaResponse>,

    pub contract_registration_request: Option<ContractRegistrationRequest>,

    pub contract_response: Option<ContractResponse>,

    pub contract_spec_function: Option<ContractSpecFunction>,

    pub contract_spec_response: Option<ContractSpecResponse>,

    pub contract_stats: Option<ContractStats>,

    pub contract_stats_response: Option<ContractStatsResponse>,

    pub contract_storage_history_response: Option<ContractStorageHistoryResponse>,

    pub contract_storage_response: Option<ContractStorageResponse>,

    pub contract_storage_value: Option<ContractStorageValue>,

    pub endpoint_usage: Option<EndpointUsage>,

    pub error_response: Option<ErrorResponse>,

    pub event_list_response: Option<EventListResponse>,

    pub indexer_stats_response: Option<IndexerStatsResponse>,

    #[serde(rename = "ListAPIKeysResponse")]
    pub list_api_keys_response: Option<ListApiKeysResponse>,

    pub list_contracts_response: Option<ListContractsResponse>,

    pub list_webhooks_response: Option<ListWebhooksResponse>,

    pub liveness_response: Option<LivenessResponse>,

    pub ready_checks: Option<ReadyChecks>,

    pub ready_response: Option<ReadyResponse>,

    pub soroban_event: Option<SorobanEvent>,

    pub token_metadata_response: Option<TokenMetadataResponse>,

    pub usage_response: Option<UsageResponse>,

    pub usage_rollup_row: Option<UsageRollupRow>,

    pub version_response: Option<VersionResponse>,

    pub webhook_create_request: Option<WebhookCreateRequest>,

    pub webhook_create_response: Option<WebhookCreateResponse>,

    pub webhook_delivery: Option<WebhookDelivery>,

    pub webhook_replay_response: Option<WebhookReplayResponse>,

    pub webhook_rotate_secret_response: Option<WebhookRotateSecretResponse>,

    pub webhook_status_response: Option<WebhookStatusResponse>,

    pub webhook_subscription: Option<WebhookSubscription>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AdminKeyUsageResponse {
    pub api_key_id: String,

    /// Per-endpoint breakdown; empty when the window has no requests
    pub by_endpoint: Vec<EndpointUsage>,

    pub from: String,

    /// Requests with status code < 400
    pub successful_requests: i64,

    pub to: String,

    pub total_requests: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EndpointUsage {
    pub avg_duration_ms: f64,

    pub endpoint: String,

    pub requests: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ApiKeyResponse {
    pub created_at: String,

    pub created_by: Option<String>,

    pub id: String,

    /// Raw key, returned only at creation time.
    pub key: Option<String>,

    pub key_prefix: String,

    pub label: String,

    pub last_used_at: String,

    pub network: Network,

    pub rate_limit_tier: String,

    pub request_count: i64,

    pub revoked_at: Option<String>,
}

/// Network queried
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Network {
    Mainnet,

    Testnet,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractCallRequest {
    /// Base64-encoded XDR ScVal arguments, in order
    pub args: Option<Vec<String>>,

    /// Contract function name to invoke
    pub function: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractCallResponse {
    /// Simulation error message; present only when success=false
    pub error: Option<String>,

    /// Raw base64 XDR of the return value; omitted on failure
    pub raw_xdr: Option<String>,

    /// Decoded return value; omitted when undecodable or failed
    pub result: Option<serde_json::Value>,

    /// False when the simulation itself reported a failure (still HTTP 200)
    pub success: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractEventFieldSchema {
    /// Stable field name for this event payload position or property
    pub name: String,

    /// Field type inferred from the contract interface or observed payloads
    #[serde(rename = "type")]
    pub contract_event_field_schema_type: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractEventSchema {
    /// Contract event name (topic_0)
    pub event_name: String,

    /// Named fields for this event payload
    pub fields: Vec<ContractEventFieldSchema>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractEventSchemaResponse {
    /// Contract code hash for this schema version
    pub code_hash: String,

    /// Soroban contract address
    pub contract_id: String,

    /// Observed event names and their typed field schemas
    pub events: Vec<ContractEventSchema>,

    /// Network queried
    pub network: Network,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractRegistrationRequest {
    /// Contract address (C... strkey, 56 characters)
    pub contract_id: String,

    /// Ledger sequence to start indexing from
    pub index_from: Option<i64>,

    /// Human-readable label
    pub label: Option<String>,

    /// Network scope; omitted or empty means all networks
    pub network: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractResponse {
    /// Stellar contract id (C... strkey).
    pub contract_id: String,

    pub created_at: String,

    pub id: String,

    /// Ledger sequence indexing began from.
    pub index_from: i64,

    pub label: Option<String>,

    pub network: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractSpecFunction {
    /// Exported function name
    pub name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractSpecResponse {
    /// Deployed WASM code hash this spec was parsed from
    pub code_hash: String,

    /// Soroban contract address
    pub contract_id: String,

    /// Primary classification derived from detected interfaces (e.g. token, nft, custom)
    pub contract_type: String,

    /// Functions captured from the contract's spec
    pub functions: Vec<ContractSpecFunction>,

    /// Whether an embedded contractspecv0 section was found
    pub has_spec: bool,

    /// Every standard interface detected from the contract's spec functions
    pub interfaces: Vec<String>,

    /// Network queried
    pub network: Network,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractStats {
    pub avg_cpu_instructions: f64,

    pub avg_fee_charged: f64,

    pub avg_read_bytes: f64,

    pub avg_write_bytes: f64,

    /// Soroban contract address
    pub contract_id: String,

    /// Total events for this contract in range
    pub event_count: i64,

    pub invocation_count: i64,

    /// Timestamp of last event for this contract
    pub last_seen_at: String,

    /// Latest ledger sequence for this contract
    pub last_seen_ledger: i64,

    pub total_fee_charged: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractStatsResponse {
    /// Contracts sorted by event count (descending)
    pub contracts: Vec<ContractStats>,

    /// Lower bound of queried ledger range
    pub from_ledger: i64,

    /// Timestamp when response was generated
    pub generated_at: String,

    /// Whether more pages are available
    pub has_more: bool,

    /// Network queried
    pub network: Network,

    /// Opaque cursor to pass as the cursor parameter for the next page
    pub next_cursor: Option<String>,

    /// Upper bound of queried ledger range
    pub to_ledger: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractStorageHistoryResponse {
    /// The contract whose storage history was queried
    pub contract_id: String,

    /// Whether more pages are available
    pub has_more: bool,

    /// Network the contract is indexed on
    pub network: String,

    /// Opaque cursor to pass as the cursor parameter for the next page
    pub next_cursor: Option<String>,

    /// The storage key whose history was queried
    pub storage_key: String,

    /// Storage history entries, oldest first
    pub values: Vec<ContractStorageValue>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractStorageValue {
    /// Human-readable decoded storage key
    pub key: Option<serde_json::Value>,

    /// Ledger sequence at which this value was observed
    pub ledger_sequence: i64,

    /// Timestamp this snapshot row was recorded
    pub observed_at: String,

    /// Base64-encoded XDR LedgerKey this value was read from
    pub storage_key: String,

    /// Human-readable decoded value (absent when the entry was removed)
    pub value: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContractStorageResponse {
    /// Soroban contract address
    pub contract_id: String,

    /// Network queried
    pub network: Network,

    /// Storage snapshot values (latest, or full history when queried via /storage/history)
    pub values: Vec<ContractStorageValue>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ErrorResponse {
    pub error: Error,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Error {
    /// Machine-readable error code. Matches httputil.ErrorCode exactly
    /// (services/api/internal/httputil/errors.go).
    pub code: Code,

    /// Human-readable error message
    pub message: String,

    /// Request ID for debugging
    pub request_id: Option<String>,
}

/// Machine-readable error code. Matches httputil.ErrorCode exactly
/// (services/api/internal/httputil/errors.go).
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum Code {
    Conflict,

    Forbidden,

    Internal,

    #[serde(rename = "INVALID_ARGUMENT")]
    InvalidArgument,

    #[serde(rename = "NOT_FOUND")]
    NotFound,

    #[serde(rename = "PAYLOAD_TOO_LARGE")]
    PayloadTooLarge,

    #[serde(rename = "RATE_LIMITED")]
    RateLimited,

    Unauthorized,

    Unavailable,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EventListResponse {
    /// List of events
    pub events: Vec<SorobanEvent>,

    /// Whether more results are available
    pub has_more: bool,

    /// Opaque cursor for next page (null if has_more is false)
    pub next_cursor: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SorobanEvent {
    /// Soroban contract address
    pub contract_id: String,

    /// Timestamp when event was indexed
    pub created_at: String,

    /// Event data (XDR-encoded)
    pub data: String,

    /// Event index within transaction
    pub event_index: i64,

    /// Type of event
    pub event_type: EventType,

    /// Unique event identifier
    pub id: String,

    /// Ledger sequence number
    pub ledger_sequence: i64,

    /// Ledger timestamp in ISO 8601 format
    pub ledger_timestamp: String,

    /// Event topics (XDR-encoded)
    pub topics: Vec<String>,

    /// Transaction hash (XDR-encoded)
    pub transaction_hash: String,
}

/// Type of event
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EventType {
    Contract,

    Diagnostic,

    System,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct IndexerStatsResponse {
    /// Average poll duration in milliseconds
    pub avg_poll_duration_ms: i64,

    /// Current chain tip ledger (from RPC)
    pub chain_tip_ledger: i64,

    /// Cumulative events indexed
    pub events_indexed_total: i64,

    /// Events processed in last poll
    pub events_last_poll: i64,

    /// Number of ledgers behind chain tip
    pub lag_ledgers: i64,

    /// Estimated wall-clock staleness in seconds: lag_ledgers times Stellar's protocol-target
    /// ledger close time (~5s). Null whenever lag_ledgers is null. See
    /// docs/observability/data-freshness.md for the full freshness contract this field is part
    /// of.
    pub lag_seconds_estimated: f64,

    /// Latest indexed ledger sequence
    pub last_ledger_indexed: i64,

    /// Timestamp of last successful poll
    pub last_poll_at: String,

    /// Network name from NETWORK environment variable
    pub network: String,

    /// Indexer health status
    pub status: IndexerStatsResponseStatus,
}

/// Indexer health status
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum IndexerStatsResponseStatus {
    Healthy,

    Lagging,

    Stalled,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ListApiKeysResponse {
    pub api_keys: Vec<ApiKeyResponse>,

    /// Whether another page is available.
    pub has_more: bool,

    /// Opaque cursor for the next page (null if has_more is false).
    pub next_cursor: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ListContractsResponse {
    pub contracts: Vec<ContractResponse>,

    /// Whether another page is available.
    pub has_more: bool,

    /// Opaque cursor for the next page (null if has_more is false).
    pub next_cursor: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ListWebhooksResponse {
    /// Whether another page is available.
    pub has_more: bool,

    /// Opaque cursor for the next page (null if has_more is false).
    pub next_cursor: String,

    pub webhooks: Vec<WebhookSubscription>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WebhookSubscription {
    /// Omitted when empty
    pub api_key_id: Option<String>,

    pub contract_id: String,

    pub created_at: String,

    pub id: String,

    pub network: String,

    /// Present while deliveries are paused
    pub paused_at: Option<String>,

    /// HMAC signing secret for deliveries; omitted when empty
    pub secret: Option<String>,

    pub target_url: String,

    /// Topic filter; omitted when unfiltered
    pub topic0: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LivenessResponse {
    /// Always "ok" while the process is up — no dependency checks.
    pub status: LivenessResponseStatus,
}

/// Always "ok" while the process is up — no dependency checks.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LivenessResponseStatus {
    #[serde(rename = "ok")]
    StatusOk,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReadyChecks {
    /// "ok" or "error: <message>"
    pub grpc_api: String,

    /// "ok" or "error: <message>"
    pub postgres: String,

    /// "ok" or "error: <message>"
    pub redis: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReadyResponse {
    pub checks: ReadyChecks,

    /// Ledgers behind chain tip, from system_state. Null when Postgres is unreachable or the
    /// chain-tip cache hasn't been populated yet.
    pub indexer_lag: i64,

    /// "degraded" when any dependency check in `checks` failed.
    pub status: ReadyResponseStatus,
}

/// "degraded" when any dependency check in `checks` failed.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ReadyResponseStatus {
    Degraded,

    #[serde(rename = "ok")]
    StatusOk,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TokenMetadataResponse {
    /// Soroban contract address
    pub contract_id: String,

    /// Token decimals, from decimals(). Null unless is_token is true.
    pub decimals: Option<i64>,

    /// True when the contract was resolved and implements the SEP-41 read interface. False for
    /// both "not yet resolved" and "resolved, not a token".
    pub is_token: bool,

    /// Token name, from name(). Null unless is_token is true.
    pub name: Option<String>,

    /// Network queried
    pub network: Network,

    /// When this contract was last resolved. Null if never resolved.
    pub resolved_at: Option<String>,

    /// Token symbol, from symbol(). Null unless is_token is true.
    pub symbol: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UsageResponse {
    pub api_key_id: String,

    /// Daily buckets from the maintained usage_rollup table, oldest first; empty when the window
    /// has no rollup rows.
    pub days: Vec<UsageRollupRow>,

    pub from: String,

    pub to: String,

    pub total_errors: i64,

    pub total_requests: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UsageRollupRow {
    pub avg_duration_ms: f64,

    pub error_count: i64,

    pub period_end: String,

    pub period_start: String,

    pub request_count: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VersionResponse {
    /// RFC 3339 build time, or "unknown" when not injected at build time. Not typed as date-time
    /// because of that sentinel.
    pub build_timestamp: String,

    /// Full git commit SHA the binary was built from, or "unknown" when not injected at build
    /// time.
    pub commit_sha: String,

    /// Highest applied migration version from _sqlx_migrations, as a string. Null when no
    /// migrations have been applied yet or when Postgres is unreachable — the endpoint still
    /// returns 200 in that case so build metadata stays available during an outage.
    pub schema_version: String,

    /// Semantic version tag of the running build, or "dev" for a binary built without release
    /// ldflags.
    pub version: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WebhookCreateRequest {
    pub contract_id: String,

    pub network: Option<String>,

    /// Delivery target; must be https with a publicly resolvable, non-private host
    pub target_url: String,

    /// Optional topic filter
    pub topic0: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WebhookCreateResponse {
    pub contract_id: String,

    pub id: String,

    pub network: String,

    /// HMAC signing secret — shown here and in the listing
    pub secret: String,

    pub target_url: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WebhookDelivery {
    pub attempt: i64,

    pub attempts: i64,

    pub delivered_at: String,

    pub event_id: String,

    pub id: i64,

    /// Omitted when empty
    pub response_body: Option<String>,

    pub status: String,

    /// HTTP status of the delivery attempt; omitted when none occurred
    pub status_code: Option<i64>,

    pub subscription_id: String,

    pub success: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WebhookReplayResponse {
    pub attempt: i64,

    /// Truncated to 500 characters
    pub response_body: String,

    pub status: WebhookReplayResponseStatus,

    /// 0 when no HTTP response occurred
    pub status_code: i64,

    pub success: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WebhookReplayResponseStatus {
    Failed,

    Success,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WebhookRotateSecretResponse {
    pub id: String,

    /// The demoted secret, now serving as secondary during the overlap window
    pub previous_secret: String,

    /// The new primary signing secret (whsec_ prefixed)
    pub secret: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WebhookStatusResponse {
    pub status: WebhookStatusResponseStatus,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WebhookStatusResponseStatus {
    Paused,

    Resumed,
}
