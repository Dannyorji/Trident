pub mod endpoints;
pub mod health;

use std::collections::hash_map::DefaultHasher;
use std::hash::{Hash, Hasher};
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use tokio_retry::strategy::ExponentialBackoff;
use trident_common::TridentError;

pub mod filters;

pub use filters::{EventFilter, FilterPlan};

use crate::metrics;
use health::RpcHealthScorer;

/// Applies full jitter to a backoff duration (issue #197, #659): without it,
/// multiple indexer replicas (or a restart storm) computing the same
/// `ExponentialBackoff` schedule retry in lockstep against the same RPC
/// endpoint, turning a transient blip into a synchronised thundering herd.
///
/// Deliberately dependency-free rather than pulling in `rand`: seeds a small
/// xorshift generator from process-local sources that vary call to call
/// (the current instant relative to an epoch fixed at first use, a memory
/// address, and the duration being jittered), which is enough entropy to
/// decorrelate concurrent processes without adding a crate whose only other
/// use in this binary would be here. Scales the input duration by a factor
/// drawn uniformly from [0.5, 1.0] — "full jitter" per the AWS
/// backoff-jitter algorithms writeup, which caps the added randomness at the
/// base delay itself rather than compounding it past `max_delay`.
pub(crate) fn jitter(duration: Duration) -> Duration {
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::OnceLock;
    static EPOCH: OnceLock<Instant> = OnceLock::new();
    static CALL_COUNTER: AtomicU64 = AtomicU64::new(0);
    let epoch = *EPOCH.get_or_init(Instant::now);

    let mut hasher = DefaultHasher::new();
    Instant::now().duration_since(epoch).hash(&mut hasher);
    // A monotonic per-process counter guarantees the seed changes even if two
    // calls land on the same clock tick (coarse timer resolution on some
    // platforms) or the same stack address (tail-call/inlining).
    CALL_COUNTER
        .fetch_add(1, Ordering::Relaxed)
        .hash(&mut hasher);
    // A stack address is effectively unpredictable ASLR noise and differs
    // across concurrent tasks/processes even when called at the same instant.
    let stack_marker = &hasher as *const _ as usize;
    stack_marker.hash(&mut hasher);
    duration.hash(&mut hasher);
    let seed = hasher.finish();

    // xorshift64* — fast, deterministic given a seed, good enough dispersion
    // for jitter (this is not security-sensitive).
    let mut x = seed | 1; // must be non-zero
    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;
    let unit = (x >> 11) as f64 / (1u64 << 53) as f64; // in [0, 1)

    let factor = 0.5 + unit * 0.5; // in [0.5, 1.0)
    duration.mul_f64(factor)
}

/// Shared retry schedule for RPC calls: full-jittered exponential backoff,
/// starting at 200ms, capped at 2s, up to 5 attempts (issues #197, #659,
/// #660). One definition so every RPC method retries identically instead of
/// only `getEvents` having retry coverage.
pub(crate) fn retry_strategy() -> impl Iterator<Item = Duration> + Clone {
    ExponentialBackoff::from_millis(200)
        .max_delay(Duration::from_secs(2))
        .map(jitter)
        .take(5)
}

/// Simple async token-bucket rate limiter for outbound RPC calls (issue
/// #661). Without this, a catch-up poll cycle (large paging loop, or one
/// `getTransaction`/`getLedgerEntries` call per transaction/contract in a
/// busy page) can burst hundreds of calls with zero delay between them,
/// since `poll_interval` only sleeps between cycles, not within one.
///
/// Tokens refill continuously at `rate_per_sec`, capped at `rate_per_sec` (a
/// one-second burst capacity). `acquire()` waits until a token is available
/// rather than rejecting the call outright — the goal is to smooth bursts,
/// not to shed load.
pub struct RateLimiter {
    rate_per_sec: f64,
    state: tokio::sync::Mutex<RateLimiterState>,
}

struct RateLimiterState {
    tokens: f64,
    last_refill: Instant,
}

impl RateLimiter {
    pub fn new(rate_per_sec: u32) -> Self {
        let rate_per_sec = rate_per_sec.max(1) as f64;
        Self {
            rate_per_sec,
            state: tokio::sync::Mutex::new(RateLimiterState {
                tokens: rate_per_sec,
                last_refill: Instant::now(),
            }),
        }
    }

    /// Wait until a token is available, then consume it.
    pub async fn acquire(&self) {
        loop {
            let wait = {
                let mut state = self.state.lock().await;
                let now = Instant::now();
                let elapsed = now.duration_since(state.last_refill).as_secs_f64();
                state.tokens = (state.tokens + elapsed * self.rate_per_sec).min(self.rate_per_sec);
                state.last_refill = now;

                if state.tokens >= 1.0 {
                    state.tokens -= 1.0;
                    None
                } else {
                    let deficit = 1.0 - state.tokens;
                    Some(Duration::from_secs_f64(deficit / self.rate_per_sec))
                }
            };

            match wait {
                None => return,
                Some(d) => tokio::time::sleep(d).await,
            }
        }
    }
}

/// Deserialize a field that the RPC may send as either a JSON string or a
/// JSON number, normalising both to `String`.
///
/// `getEvents` is inconsistent across Soroban RPC versions: older releases
/// quote `ledger` (`"7"`), current ones send a bare integer (`7`). Typing the
/// field as `String` therefore failed the whole page with
/// `invalid type: integer 7, expected a string`, so no events were ever
/// ingested against a modern RPC (issue #388). Accepting both keeps the
/// existing `String` contract for callers while tolerating either wire shape.
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

/// A single raw event as returned by the Stellar RPC `getEvents` method.
/// Topics and data are base64-encoded XDR strings; the parser decodes them.
#[derive(Debug, Deserialize)]
pub struct RawEvent {
    #[serde(rename = "type")]
    pub event_type: String,
    /// Ledger sequence number. Kept as a string because the RPC has sent it
    /// both quoted and unquoted across versions; see [`string_or_number`].
    #[serde(deserialize_with = "string_or_number")]
    pub ledger: String,
    #[serde(rename = "ledgerClosedAt")]
    pub ledger_closed_at: String,
    #[serde(rename = "contractId")]
    pub contract_id: Option<String>,
    pub id: String,
    /// Removed from the RPC response in stellar-rpc v22 (stellar-rpc#382):
    /// `id` identifies an individual event and the top-level `cursor` drives
    /// pagination. Older servers still send it, so it stays optional rather
    /// than failing the whole page on a modern one. Prefer
    /// [`RawEvent::page_cursor`] over reading this directly.
    #[serde(rename = "pagingToken", default)]
    pub paging_token: Option<String>,
    #[serde(rename = "txHash")]
    pub tx_hash: String,
    /// Operation index within the transaction, added in stellar-rpc#383.
    /// Absent on older servers, where the index was encoded in `id` instead.
    #[serde(rename = "operationIndex", default)]
    pub operation_index: Option<u32>,
    /// Ordered list of base64 XDR-encoded ScVal topic values.
    pub topic: Vec<String>,
    /// Base64 XDR-encoded ScVal event body.
    pub value: String,
    /// Deprecated upstream and slated for removal (stellar-rpc#4590), so a
    /// missing value must not fail the page. Absent means the event was not
    /// filtered out, hence the `true` default.
    #[serde(rename = "inSuccessfulContractCall", default = "default_true")]
    pub in_successful_contract_call: bool,
}

fn default_true() -> bool {
    true
}

impl RawEvent {
    /// The token to resume paging from after this event.
    ///
    /// Uses `pagingToken` when the server still sends it and falls back to
    /// `id`, which stellar-rpc#382 designates as its replacement. Both are
    /// accepted by the `cursor` request parameter.
    pub fn page_cursor(&self) -> String {
        self.paging_token.clone().unwrap_or_else(|| self.id.clone())
    }
}

#[derive(Debug)]
pub struct EventsPage {
    pub events: Vec<RawEvent>,
    pub latest_ledger: u64,
}

// ---------------------------------------------------------------------------
// JSON-RPC wire types
// ---------------------------------------------------------------------------

#[derive(Serialize)]
struct JsonRpcRequest<'a, P: Serialize> {
    jsonrpc: &'static str,
    id: u64,
    method: &'a str,
    params: P,
}

#[derive(Deserialize)]
struct JsonRpcResponse<R> {
    result: Option<R>,
    error: Option<JsonRpcError>,
}

#[derive(Deserialize)]
struct JsonRpcError {
    code: i64,
    message: String,
}

#[derive(Serialize)]
struct GetLedgersParams {
    #[serde(rename = "startLedger")]
    start_ledger: u64,
    pagination: LedgerPagination,
}

#[derive(Serialize)]
struct LedgerPagination {
    limit: u32,
}

#[derive(Deserialize)]
struct GetLedgersResult {
    ledgers: Vec<LedgerSummary>,
}

/// `getLatestLedger` takes no parameters, but the JSON-RPC envelope this client
/// builds always serialises a `params` member.
///
/// Wire types for [`RpcClient::get_latest_ledger`] (un-gated with it for the
/// reconciliation loop, issue #511).
#[derive(Serialize)]
struct EmptyParams {}

#[derive(Deserialize)]
struct GetLatestLedgerResult {
    sequence: u64,
}

#[derive(Deserialize)]
struct LedgerSummary {
    hash: String,
}

#[derive(Serialize)]
struct GetEventsParams<'a> {
    #[serde(rename = "startLedger", skip_serializing_if = "Option::is_none")]
    start_ledger: Option<u64>,
    /// Server-side narrowing (issue #203). Always serialised — an empty array is
    /// the RPC's "no filter" form and is what index-all mode sends.
    filters: &'a [EventFilter],
    pagination: Pagination,
}

#[derive(Serialize)]
struct Pagination {
    limit: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    cursor: Option<String>,
}

#[derive(Deserialize)]
struct GetEventsResult {
    events: Vec<RawEvent>,
    #[serde(rename = "latestLedger")]
    latest_ledger: u64,
}

#[derive(Serialize)]
struct GetTransactionParams<'a> {
    hash: &'a str,
}

#[derive(Serialize)]
struct GetLedgerEntriesParams<'a> {
    keys: &'a [String],
}

#[derive(Deserialize)]
struct GetLedgerEntriesResult {
    entries: Option<Vec<LedgerEntryResult>>,
}

/// One entry returned by `getLedgerEntries` (issue #260 / #270). `key` and
/// `xdr` are base64-encoded `LedgerKey` / `LedgerEntryData` XDR respectively;
/// decoding them is owned by the caller (`crate::spec`, `crate::storage`).
#[derive(Debug, Deserialize)]
pub struct LedgerEntryResult {
    pub key: String,
    pub xdr: String,
}

/// Result of the Soroban RPC `getTransaction` call (issue #266).
///
/// `envelope_xdr` / `result_xdr` are only present when `status` is not
/// `"NOT_FOUND"`. Decoding them is owned by
/// `crate::parser::invocation_metrics`, not this transport layer.
#[derive(Debug, Deserialize)]
pub struct GetTransactionResult {
    pub status: String,
    #[serde(rename = "envelopeXdr")]
    pub envelope_xdr: Option<String>,
    #[serde(rename = "resultXdr")]
    pub result_xdr: Option<String>,
}

#[derive(Serialize)]
struct SimulateTransactionParams<'a> {
    transaction: &'a str,
}

/// One entry of `simulateTransaction`'s `results` array — the base64 XDR
/// `ScVal` a read-only host function call returned.
#[derive(Debug, Deserialize)]
pub struct SimulateHostFunctionResult {
    pub xdr: String,
}

/// Result of the Soroban RPC `simulateTransaction` call (issue #263).
///
/// `error` is set (and `results` empty) when the simulated invocation failed,
/// e.g. the contract has no function by that name — the caller treats that as
/// "not a token" rather than a transport failure. Decoding `results[].xdr` is
/// owned by `crate::token_metadata`.
#[derive(Debug, Deserialize)]
pub struct SimulateTransactionResult {
    #[serde(default)]
    pub error: Option<String>,
    #[serde(default)]
    pub results: Vec<SimulateHostFunctionResult>,
}

// ---------------------------------------------------------------------------
// RPC client
// ---------------------------------------------------------------------------

/// HTTP transport settings for the RPC client (issue #214).
///
/// A default `reqwest::Client` has no request timeout at all, so a stalled
/// response blocks the poll loop forever — the retry wrapper only sees returned
/// errors, never a call that never returns. Every field here is derived from
/// `Config` so operators can tune it per environment.
#[derive(Debug, Clone)]
pub struct RpcHttpSettings {
    pub connect_timeout: Duration,
    pub request_timeout: Duration,
    pub pool_idle_timeout: Duration,
    pub pool_max_idle_per_host: usize,
    pub tcp_keepalive: Duration,
    /// Maximum outbound RPC calls per second, self-imposed independently of
    /// `poll_interval` (issue #661). Applied to every call through `execute`,
    /// so it bounds `getEvents` paging as well as the per-transaction /
    /// per-contract fan-out in `fetch_invocation_metrics` and storage
    /// snapshotting.
    pub max_calls_per_sec: u32,
}

impl Default for RpcHttpSettings {
    fn default() -> Self {
        Self {
            connect_timeout: Duration::from_secs(5),
            request_timeout: Duration::from_secs(30),
            pool_idle_timeout: Duration::from_secs(90),
            pool_max_idle_per_host: 8,
            tcp_keepalive: Duration::from_secs(60),
            max_calls_per_sec: 50,
        }
    }
}

impl RpcHttpSettings {
    /// Build the shared `reqwest::Client`: bounded connect/request timeouts plus
    /// keep-alive and idle-pool tuning so successive polls reuse connections
    /// instead of paying a fresh TCP + TLS handshake each time.
    fn build_client(&self) -> Result<reqwest::Client, TridentError> {
        reqwest::Client::builder()
            .connect_timeout(self.connect_timeout)
            .timeout(self.request_timeout)
            .pool_idle_timeout(self.pool_idle_timeout)
            .pool_max_idle_per_host(self.pool_max_idle_per_host)
            .tcp_keepalive(self.tcp_keepalive)
            .build()
            .map_err(|e| {
                TridentError::config(
                    anyhow::Error::new(e).context("failed to build RPC HTTP client"),
                )
            })
    }
}

/// Convert a `reqwest` transport failure into a retryable [`TridentError`],
/// tagging timeouts explicitly so they are visible in logs and metrics.
///
/// `RpcError` is already classified `Severity::Retryable`, which is what makes
/// the backoff wrapper and the poll loop treat a timeout as a transient failure
/// rather than a poison input (issue #214).
fn rpc_transport_error(err: reqwest::Error, context: &'static str) -> TridentError {
    if err.is_timeout() {
        metrics::record_rpc_timeout();
        metrics::record_rpc_error(context, "timeout");
        return TridentError::rpc(anyhow::Error::new(err).context(format!("{context} timed out")));
    }
    metrics::record_rpc_error(context, "transport");
    TridentError::rpc(anyhow::Error::new(err).context(context))
}

/// Coarse error-type label for a non-2xx RPC HTTP response (issue #294).
fn classify_http_status(status: reqwest::StatusCode) -> &'static str {
    match status.as_u16() {
        429 => "rate_limited",
        400..=499 => "http_4xx",
        500..=599 => "http_5xx",
        _ => "other",
    }
}

pub struct RpcClient {
    http: reqwest::Client,
    /// Health scorer for multi-RPC failover with dynamic scoring.
    scorer: Arc<RpcHealthScorer>,
    /// Configured endpoints in priority order. The scorer keys endpoints by
    /// URL in a HashMap, which has no stable ordering, so this list is what
    /// gives each endpoint the positional index used to label per-endpoint
    /// latency metrics (issue #294).
    endpoints: Vec<String>,
    /// Self-imposed outbound rate limit, shared across every call this
    /// client makes (issue #661).
    rate_limiter: RateLimiter,
}

impl RpcClient {
    /// Build a single-endpoint client whose transport honours the configured
    /// timeouts and connection-pool settings (issue #214).
    #[cfg(test)]
    pub fn with_settings(url: String, settings: &RpcHttpSettings) -> Result<Self, TridentError> {
        Self::with_endpoints(vec![url], settings)
    }

    /// Build a client over a prioritised endpoint list with health-based
    /// failover using dynamic scoring (multi-RPC failover).
    pub fn with_endpoints(
        urls: Vec<String>,
        settings: &RpcHttpSettings,
    ) -> Result<Self, TridentError> {
        let scorer = Arc::new(RpcHealthScorer::new(urls.clone())?);

        Ok(Self {
            http: settings.build_client()?,
            scorer,
            endpoints: urls,
            rate_limiter: RateLimiter::new(settings.max_calls_per_sec),
        })
    }

    /// Get the health scorer for testing and monitoring purposes.
    pub fn health_scorer(&self) -> &Arc<RpcHealthScorer> {
        &self.scorer
    }

    /// Select the healthiest endpoint for the next request. Also returns the
    /// endpoint's position in the configured list, used to label per-endpoint
    /// latency metrics (issue #294).
    fn select_endpoint(&self) -> (String, usize) {
        let url = self.scorer.select_best_endpoint();
        let index = self
            .endpoints
            .iter()
            .position(|candidate| candidate == &url)
            .unwrap_or(0);
        metrics::set_rpc_active_endpoint(index);
        (url, index)
    }

    /// Record a successful response from the given endpoint.
    fn record_success(&self, url: &str, ledger: Option<u64>) {
        self.scorer.record_success(url, ledger);
    }

    /// Record a timeout error from the given endpoint.
    fn record_timeout(&self, url: &str) {
        self.scorer.record_timeout(url);
    }

    /// Record a non-200 HTTP response from the given endpoint.
    fn record_non_200(&self, url: &str) {
        self.scorer.record_non_200(url);
    }

    /// Record a JSON-RPC error from the given endpoint.
    fn record_rpc_error(&self, url: &str) {
        self.scorer.record_rpc_error(url);
    }

    /// Run one JSON-RPC call against the active endpoint, updating endpoint
    /// health from the outcome.
    async fn call<P, R>(
        &self,
        method: &str,
        id: u64,
        params: P,
        context: &'static str,
    ) -> Result<R, TridentError>
    where
        P: Serialize,
        R: serde::de::DeserializeOwned,
    {
        let (url, endpoint_index) = self.select_endpoint();
        let req = JsonRpcRequest {
            jsonrpc: "2.0",
            id,
            method,
            params,
        };

        // Timed regardless of outcome: a provider that's getting slower but
        // not yet erroring is exactly what this metric exists to catch
        // (issue #294).
        let started = Instant::now();
        let result = self.execute(&url, &req, context).await;
        metrics::record_rpc_call_duration(context, endpoint_index, started.elapsed().as_secs_f64());

        match &result {
            Ok(_) => self.record_success(&url, None),
            Err(e) => self.record_error(&url, e),
        }
        result
    }

    /// Record an error based on its type.
    fn record_error(&self, url: &str, error: &TridentError) {
        let error_str = error.to_string();
        if error_str.contains("timed out") {
            self.record_timeout(url);
        } else if error_str.contains("rate limited") {
            self.record_rate_limited(url);
        } else if error_str.contains("HTTP 4") || error_str.contains("HTTP 5") {
            self.record_non_200(url);
        } else if error_str.contains("RPC error") {
            self.record_rpc_error(url);
        } else if error_str.contains("connection refused") {
            self.record_connection_refused(url);
        }
    }

    /// Record a connection refused error from the given endpoint.
    fn record_connection_refused(&self, url: &str) {
        self.scorer.record_connection_refused(url);
    }

    /// Record a rate-limit (HTTP 429) response from the given endpoint.
    fn record_rate_limited(&self, url: &str) {
        self.scorer.record_rate_limited(url);
    }

    async fn execute<P, R>(
        &self,
        url: &str,
        req: &JsonRpcRequest<'_, P>,
        context: &'static str,
    ) -> Result<R, TridentError>
    where
        P: Serialize,
        R: serde::de::DeserializeOwned,
    {
        // Self-imposed outbound rate limit (issue #661): every RPC call goes
        // through this one choke point, so gating here bounds getEvents
        // paging and the per-transaction/per-contract fan-out alike, without
        // each call site needing its own limiter.
        self.rate_limiter.acquire().await;

        let resp = self
            .http
            .post(url)
            .json(req)
            .send()
            .await
            .map_err(|e| rpc_transport_error(e, context))?;

        // A non-2xx response (rate limit, 5xx) is an endpoint failure, not a
        // decode failure — surface it as such so failover can react.
        if !resp.status().is_success() {
            let status = resp.status();
            metrics::record_rpc_error(context, classify_http_status(status));
            let kind = if status.as_u16() == 429 {
                "rate limited"
            } else {
                "non-200"
            };
            return Err(TridentError::rpc(anyhow::anyhow!(
                "{context}: endpoint {url} returned HTTP {} ({kind})",
                status
            )));
        }

        let body: JsonRpcResponse<R> = resp
            .json()
            .await
            .map_err(|e| rpc_transport_error(e, context))?;

        if let Some(err) = body.error {
            // The RPC has no dedicated error code for an out-of-range cursor
            // (issue #294 asks it be distinguishable from other JSON-RPC
            // errors); its message is the only signal available.
            let error_type = if err.message.to_lowercase().contains("cursor") {
                "invalid_cursor"
            } else {
                "rpc_error"
            };
            metrics::record_rpc_error(context, error_type);
            return Err(TridentError::rpc(anyhow::anyhow!(
                "{context}: RPC error {}: {}",
                err.code,
                err.message
            )));
        }

        body.result.ok_or_else(|| {
            metrics::record_rpc_error(context, "empty_result");
            TridentError::rpc(anyhow::anyhow!("{context}: empty result"))
        })
    }

    /// Fetch the current chain tip via `getLatestLedger`.
    ///
    /// `getEvents` also reports `latestLedger`, but only on a request that
    /// already carries a valid in-range `startLedger` — which is precisely what
    /// a caller who does not yet know the tip cannot supply. This method has no
    /// such precondition.
    ///
    /// The poll loop learns the tip from the `getEvents` responses it already
    /// makes, so it never calls this. The testnet correctness suite (issue
    /// #419) and the reconciliation loop (issue #511) both need to choose a
    /// settled ledger window before they can request anything, which is
    /// exactly the case this method exists for. (Previously `#[cfg(test)]`
    /// with a note to remove the gate when a production caller appeared —
    /// the reconciler is that caller.)
    pub async fn get_latest_ledger(&self) -> Result<u64, TridentError> {
        let result: GetLatestLedgerResult = self
            .call("getLatestLedger", 3, EmptyParams {}, "getLatestLedger")
            .await?;
        Ok(result.sequence)
    }

    /// Fetch the ledger hash for a given sequence number via `getLedgers`.
    /// Returns `None` if the RPC does not know about that ledger yet.
    pub async fn get_ledger(&self, sequence: u64) -> Result<Option<String>, TridentError> {
        let params = GetLedgersParams {
            start_ledger: sequence,
            pagination: LedgerPagination { limit: 1 },
        };

        let result: GetLedgersResult = self.call("getLedgers", 2, params, "getLedgers").await?;

        Ok(result.ledgers.into_iter().next().map(|l| l.hash))
    }

    /// Fetch a page of events from the Stellar RPC node.
    ///
    /// Pass `start_ledger` on the first call to anchor the scan position.
    /// On subsequent calls pass `cursor` (the `paging_token` from the last
    /// event received) to continue pagination. Only one of the two should be
    /// set at a time — the RPC rejects requests that supply both.
    ///
    /// `limit` controls the page size; callers should pass `config.max_events_per_poll`.
    ///
    /// `filters` narrows the result set server-side (issue #203). Pass an empty
    /// slice to index every contract.
    pub async fn get_events(
        &self,
        start_ledger: Option<u64>,
        cursor: Option<String>,
        limit: u32,
        filters: &[EventFilter],
    ) -> Result<EventsPage, TridentError> {
        let (url, _endpoint_index) = self.select_endpoint();
        let params = GetEventsParams {
            start_ledger,
            filters,
            pagination: Pagination { limit, cursor },
        };

        let req = JsonRpcRequest {
            jsonrpc: "2.0",
            id: 1,
            method: "getEvents",
            params: &params,
        };

        let result: Result<GetEventsResult, TridentError> =
            self.execute(&url, &req, "getEvents").await;
        match &result {
            Ok(r) => {
                self.record_success(&url, Some(r.latest_ledger));
                // Check for stale ledger
                self.scorer.check_and_record_stale(&url);
            }
            Err(e) => self.record_error(&url, e),
        }

        let result = result?;
        Ok(EventsPage {
            events: result.events,
            latest_ledger: result.latest_ledger,
        })
    }

    /// Fetch a single transaction's envelope + result XDR via `getTransaction`
    /// (issue #266). Used to derive per-invocation fee and declared resource
    /// metering for tracked contracts — see
    /// `crate::parser::invocation_metrics`.
    ///
    /// Retried with the same full-jittered backoff as `getEvents` (issue
    /// #660): previously a single transient failure here permanently dropped
    /// that transaction's invocation metrics for the whole poll cycle,
    /// since the caller treats any error as "skip this cycle".
    pub async fn get_transaction(&self, hash: &str) -> Result<GetTransactionResult, TridentError> {
        tokio_retry::Retry::start(retry_strategy(), || async {
            self.call(
                "getTransaction",
                3,
                GetTransactionParams { hash },
                "getTransaction",
            )
            .await
        })
        .await
    }

    /// Run a read-only host function call through `simulateTransaction`
    /// (issue #263), used by `crate::token_metadata` to read SEP-41 token
    /// metadata without ever signing or submitting a transaction.
    ///
    /// A simulation that the node rejects (e.g. the contract exposes no such
    /// function) comes back as `Ok` with `error` set and `results` empty — that
    /// is a normal "not a token" answer, not a transport failure, so it is left
    /// for the caller to interpret. Only genuine RPC/transport failures are
    /// returned as `Err`.
    ///
    /// Retried with the same full-jittered backoff as `getEvents` (issue
    /// #660): a transient failure here previously permanently dropped token
    /// metadata resolution for that contract on this cycle.
    pub async fn simulate_transaction(
        &self,
        envelope_xdr: &str,
    ) -> Result<SimulateTransactionResult, TridentError> {
        tokio_retry::Retry::start(retry_strategy(), || async {
            let params = SimulateTransactionParams {
                transaction: envelope_xdr,
            };
            self.call("simulateTransaction", 6, params, "simulateTransaction")
                .await
        })
        .await
    }

    /// Fetch a batch of ledger entries (contract instance, contract code, or
    /// contract data) via `getLedgerEntries` (issues #260, #270). Keys not
    /// present on-chain (e.g. never written, or archived) are simply absent
    /// from the returned list rather than erroring.
    ///
    /// Retried with the same full-jittered backoff as `getEvents` (issue
    /// #660): a transient failure here previously permanently dropped the
    /// balance-snapshot write for every token contract touched in the page.
    pub async fn get_ledger_entries(
        &self,
        keys: &[String],
    ) -> Result<Vec<LedgerEntryResult>, TridentError> {
        if keys.is_empty() {
            return Ok(Vec::new());
        }
        tokio_retry::Retry::start(retry_strategy(), || async {
            let params = GetLedgerEntriesParams { keys };
            let result: GetLedgerEntriesResult = self
                .call("getLedgerEntries", 5, params, "getLedgerEntries")
                .await?;
            Ok(result.entries.unwrap_or_default())
        })
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Instant;
    use trident_common::Severity;
    use wiremock::matchers::method;
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn fast_timeout_settings() -> RpcHttpSettings {
        RpcHttpSettings {
            connect_timeout: Duration::from_millis(300),
            request_timeout: Duration::from_millis(300),
            ..RpcHttpSettings::default()
        }
    }

    /// A deliberately slow endpoint must abort within the configured request
    /// timeout instead of hanging the caller (issue #214).
    #[tokio::test]
    async fn slow_endpoint_aborts_within_request_timeout() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(
                ResponseTemplate::new(200)
                    .set_delay(Duration::from_secs(10))
                    .set_body_string("{}"),
            )
            .mount(&server)
            .await;

        let client = RpcClient::with_settings(server.uri(), &fast_timeout_settings()).unwrap();

        let started = Instant::now();
        let err = client
            .get_events(Some(1), None, 10, &[])
            .await
            .expect_err("slow endpoint must not succeed");
        let elapsed = started.elapsed();

        assert!(
            elapsed < Duration::from_secs(5),
            "call should abort at the timeout, took {elapsed:?}"
        );
        assert!(
            err.to_string().contains("timed out"),
            "timeout should be reported as such, got: {err}"
        );
    }

    /// A timeout must stay classified as retryable so the backoff wrapper and
    /// the circuit breaker engage rather than the poll cycle being skipped.
    #[tokio::test]
    async fn timeout_is_classified_retryable() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(
                ResponseTemplate::new(200)
                    .set_delay(Duration::from_secs(10))
                    .set_body_string("{}"),
            )
            .mount(&server)
            .await;

        let client = RpcClient::with_settings(server.uri(), &fast_timeout_settings()).unwrap();
        let err = client.get_ledger(42).await.expect_err("must time out");

        assert_eq!(err.severity(), Severity::Retryable);
        assert!(err.retryable());
    }

    /// The settings are applied to a real client build — a bad combination is a
    /// config error surfaced at startup, not a silent default.
    #[test]
    fn settings_build_a_client() {
        assert!(RpcHttpSettings::default().build_client().is_ok());
    }

    fn raw_event_json(ledger: serde_json::Value) -> serde_json::Value {
        serde_json::json!({
            "type": "contract",
            "ledger": ledger,
            "ledgerClosedAt": "2026-08-09T18:59:36Z",
            "contractId": "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
            // Deliberately different so tests can prove which one is used.
            "id": "0000000000000000007-0000000001",
            "pagingToken": "0000000000000000000-0000000000",
            "txHash": "aabb",
            "topic": ["AAAADwAAAARtaW50"],
            "value": "AAAACgAAAAAAAAAAAAAAAAAAAGQ=",
            "inSuccessfulContractCall": true
        })
    }

    #[test]
    fn raw_event_accepts_an_unquoted_ledger_number() {
        // Regression (#388): current Soroban RPC sends `"ledger": 7` as a bare
        // integer. Typing the field as String failed the entire page with
        // `invalid type: integer 7, expected a string`, so nothing was indexed.
        let ev: RawEvent = serde_json::from_value(raw_event_json(serde_json::json!(7)))
            .expect("unquoted ledger must deserialize");
        assert_eq!(ev.ledger, "7");
    }

    #[test]
    fn raw_event_still_accepts_a_quoted_ledger_string() {
        // Older RPC releases quote it; both shapes must keep working.
        let ev: RawEvent = serde_json::from_value(raw_event_json(serde_json::json!("7")))
            .expect("quoted ledger must deserialize");
        assert_eq!(ev.ledger, "7");
    }

    #[test]
    fn raw_event_parses_without_paging_token_and_falls_back_to_id() {
        // Regression (#388): stellar-rpc#382 removed pagingToken in favour of
        // `id`. Requiring it failed the whole page against a current server.
        let mut json = raw_event_json(serde_json::json!(7));
        json.as_object_mut().unwrap().remove("pagingToken");
        let ev: RawEvent =
            serde_json::from_value(json).expect("missing pagingToken must deserialize");
        assert_eq!(ev.paging_token, None);
        assert_eq!(ev.page_cursor(), ev.id, "paging must fall back to id");
    }

    #[test]
    fn raw_event_prefers_paging_token_when_the_server_sends_one() {
        // Older servers still send it; it must win over id so paging behaviour
        // against them is unchanged.
        let ev: RawEvent = serde_json::from_value(raw_event_json(serde_json::json!(7)))
            .expect("event must deserialize");
        assert_eq!(ev.page_cursor(), "0000000000000000000-0000000000");
        assert_ne!(ev.page_cursor(), ev.id);
    }

    #[test]
    fn raw_event_defaults_in_successful_contract_call_when_absent() {
        // Deprecated upstream (stellar-rpc#4590) and due for removal, so its
        // absence must not fail the page or silently drop the event.
        let mut json = raw_event_json(serde_json::json!(7));
        json.as_object_mut()
            .unwrap()
            .remove("inSuccessfulContractCall");
        let ev: RawEvent = serde_json::from_value(json)
            .expect("missing inSuccessfulContractCall must deserialize");
        assert!(ev.in_successful_contract_call);
    }

    /// Mount an endpoint that always answers `getEvents` with an empty page.
    async fn mount_healthy(server: &MockServer) {
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 1,
                "result": { "events": [], "latestLedger": 100 }
            })))
            .mount(server)
            .await;
    }

    /// A primary that starts failing must hand traffic to the secondary, and
    /// the pool must return to the primary once its cooldown elapses (#213).
    #[tokio::test]
    async fn failover_moves_to_secondary_then_recovers_to_primary() {
        let primary = MockServer::start().await;
        let secondary = MockServer::start().await;

        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(503))
            .mount(&primary)
            .await;
        mount_healthy(&secondary).await;

        let client = RpcClient::with_endpoints(
            vec![primary.uri(), secondary.uri()],
            &fast_timeout_settings(),
        )
        .unwrap();

        // One 503 costs the primary 15 points (100 -> 85), which is already
        // enough to put the untouched secondary ahead. Scoring fails over on
        // the first failure, unlike the consecutive-failure threshold the
        // superseded endpoint pool used.
        assert!(client.get_events(Some(1), None, 10, &[]).await.is_err());

        // The health scorer should now prefer the secondary endpoint.
        let best_endpoint = client.health_scorer().select_best_endpoint();
        assert_eq!(best_endpoint, secondary.uri());

        // The secondary now serves traffic successfully.
        let page = client
            .get_events(Some(1), None, 10, &[])
            .await
            .expect("secondary must serve the request");
        assert_eq!(page.latest_ledger, 100);
    }

    /// A single healthy endpoint never fails over, and non-2xx responses from
    /// it are surfaced as retryable RPC errors.
    #[tokio::test]
    async fn non_success_status_is_a_retryable_rpc_error() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(429))
            .mount(&server)
            .await;

        let client = RpcClient::with_settings(server.uri(), &fast_timeout_settings()).unwrap();
        let err = client.get_events(Some(1), None, 10, &[]).await.unwrap_err();

        assert!(err.to_string().contains("429"), "got: {err}");
        assert_eq!(err.severity(), Severity::Retryable);
    }

    /// A 429 must be scored as a rate limit (-5), not a generic non-200
    /// (-15), and a lone 429 must not be enough to trigger failover to a
    /// backup that could just as easily be rate-limited itself (issue #656).
    #[tokio::test]
    async fn rate_limit_response_scores_less_severely_than_a_hard_failure() {
        let primary = MockServer::start().await;
        let secondary = MockServer::start().await;

        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(429))
            .mount(&primary)
            .await;
        mount_healthy(&secondary).await;

        let client = RpcClient::with_endpoints(
            vec![primary.uri(), secondary.uri()],
            &fast_timeout_settings(),
        )
        .unwrap();

        assert!(client.get_events(Some(1), None, 10, &[]).await.is_err());

        assert_eq!(client.health_scorer().get_score(&primary.uri()), 95);
        assert_eq!(
            client.health_scorer().select_best_endpoint(),
            primary.uri(),
            "a single 429 must not fail over to a backup that could be rate-limited too"
        );
    }

    /// Requests are served by the primary while it is healthy.
    #[tokio::test]
    async fn healthy_primary_keeps_serving() {
        let primary = MockServer::start().await;
        let secondary = MockServer::start().await;
        mount_healthy(&primary).await;
        mount_healthy(&secondary).await;

        let client = RpcClient::with_endpoints(
            vec![primary.uri(), secondary.uri()],
            &fast_timeout_settings(),
        )
        .unwrap();

        for _ in 0..3 {
            client.get_events(Some(1), None, 10, &[]).await.unwrap();
        }
        // Primary should still be preferred (both at 100, primary was first)
        let best_endpoint = client.health_scorer().select_best_endpoint();
        assert_eq!(best_endpoint, primary.uri());
        assert_eq!(secondary.received_requests().await.unwrap().len(), 0);
    }

    /// A read-only `simulateTransaction` call decodes the result envelope's
    /// XDR return value (issue #263).
    #[tokio::test]
    async fn simulate_transaction_returns_decoded_result() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 4,
                "result": { "results": [{ "xdr": "AAAAAwAAAAo=" }] }
            })))
            .mount(&server)
            .await;

        let client = RpcClient::with_settings(server.uri(), &fast_timeout_settings()).unwrap();
        let result = client.simulate_transaction("deadbeef").await.unwrap();

        assert!(result.error.is_none());
        assert_eq!(result.results.len(), 1);
        assert_eq!(result.results[0].xdr, "AAAAAwAAAAo=");
    }

    /// A simulation error (e.g. the contract has no such function) is
    /// surfaced on the result rather than as a transport error, so the
    /// caller can treat it as "not a token" (issue #263).
    #[tokio::test]
    async fn simulate_transaction_surfaces_simulation_error() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 4,
                "result": { "error": "HostError: Error(Value, MissingValue)", "results": [] }
            })))
            .mount(&server)
            .await;

        let client = RpcClient::with_settings(server.uri(), &fast_timeout_settings()).unwrap();
        let result = client.simulate_transaction("deadbeef").await.unwrap();

        assert!(result.error.is_some());
        assert!(result.results.is_empty());
    }

    /// A transient failure on `getTransaction` must be retried and recover,
    /// rather than permanently dropping invocation metrics for that
    /// transaction (issue #660). The mock fails the first two attempts, then
    /// succeeds — this only passes if `get_transaction` retries internally.
    #[tokio::test]
    async fn get_transaction_retries_and_recovers_from_transient_failure() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(503))
            .up_to_n_times(2)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 3,
                "result": { "status": "SUCCESS", "envelopeXdr": "AAAA", "resultXdr": "AAAA" }
            })))
            .mount(&server)
            .await;

        let client = RpcClient::with_settings(server.uri(), &fast_timeout_settings()).unwrap();
        let result = client
            .get_transaction("deadbeef")
            .await
            .expect("must recover after retrying past the transient failures");
        assert_eq!(result.status, "SUCCESS");
    }

    /// A transient failure on `getLedgerEntries` must be retried and recover,
    /// rather than permanently dropping that page's storage snapshots (issue
    /// #660).
    #[tokio::test]
    async fn get_ledger_entries_retries_and_recovers_from_transient_failure() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(503))
            .up_to_n_times(2)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "jsonrpc": "2.0",
                "id": 5,
                "result": { "entries": [] }
            })))
            .mount(&server)
            .await;

        let client = RpcClient::with_settings(server.uri(), &fast_timeout_settings()).unwrap();
        let result = client
            .get_ledger_entries(&["some-key".to_string()])
            .await
            .expect("must recover after retrying past the transient failures");
        assert!(result.is_empty());
    }

    /// The self-imposed outbound rate limiter (issue #661) measurably spreads
    /// calls in time rather than letting them all fire back-to-back: with a
    /// 5 calls/sec budget, 10 sequential calls must take at least ~1 second
    /// (the second batch of 5 waiting for tokens to refill), not a few
    /// milliseconds.
    #[tokio::test]
    async fn rate_limiter_spreads_bursts_in_time() {
        let server = MockServer::start().await;
        mount_healthy(&server).await;

        let settings = RpcHttpSettings {
            max_calls_per_sec: 5,
            ..fast_timeout_settings()
        };
        let client = RpcClient::with_settings(server.uri(), &settings).unwrap();

        let start = Instant::now();
        for _ in 0..10 {
            client.get_events(Some(1), None, 10, &[]).await.unwrap();
        }
        let elapsed = start.elapsed();

        assert!(
            elapsed >= Duration::from_millis(900),
            "10 calls at 5/sec should take >= ~1s, took {elapsed:?}"
        );
    }
}
