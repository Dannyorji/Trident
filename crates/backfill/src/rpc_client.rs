use crate::parser::RawEvent;
use serde::{Deserialize, Serialize};
use trident_common::TridentError;

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
struct GetEventsParams {
    #[serde(rename = "startLedger", skip_serializing_if = "Option::is_none")]
    start_ledger: Option<u64>,
    filters: Vec<serde_json::Value>,
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

pub struct RpcClient {
    http: reqwest::Client,
    url: String,
    /// Provider auth headers (issue #735). Values are credentials, so this
    /// struct deliberately does not derive `Debug`.
    headers: reqwest::header::HeaderMap,
}

impl RpcClient {
    /// Build a client for `url`, applying `STELLAR_RPC_HEADERS` (and
    /// `STELLAR_RPC_HEADERS_0`, since backfill talks to a single endpoint).
    ///
    /// Provider keys are read from the environment rather than a CLI flag so
    /// they never appear in `ps` output or shell history. A malformed spec
    /// fails startup naming the variable and header, never the value.
    pub fn new(url: String) -> Result<Self, TridentError> {
        let global = match std::env::var("STELLAR_RPC_HEADERS") {
            Ok(spec) => trident_common::rpc_headers::parse(&spec)
                .map_err(|e| TridentError::config(anyhow::anyhow!("STELLAR_RPC_HEADERS: {e}")))?,
            Err(_) => Vec::new(),
        };
        let per_endpoint = match std::env::var("STELLAR_RPC_HEADERS_0") {
            Ok(spec) => trident_common::rpc_headers::parse(&spec)
                .map_err(|e| TridentError::config(anyhow::anyhow!("STELLAR_RPC_HEADERS_0: {e}")))?,
            Err(_) => Vec::new(),
        };
        Self::with_headers(
            url,
            trident_common::rpc_headers::merge(global, per_endpoint),
        )
    }

    /// Build a client with an explicit header set (used by tests and by
    /// callers that source credentials somewhere other than the environment).
    pub fn with_headers(url: String, headers: Vec<(String, String)>) -> Result<Self, TridentError> {
        let mut map = reqwest::header::HeaderMap::new();
        for (name, value) in headers {
            let header_name =
                reqwest::header::HeaderName::from_bytes(name.as_bytes()).map_err(|_| {
                    TridentError::config(anyhow::anyhow!(
                        "RPC header name {name:?} is not a valid HTTP token"
                    ))
                })?;
            let header_value = reqwest::header::HeaderValue::from_str(&value).map_err(|_| {
                TridentError::config(anyhow::anyhow!(
                    "RPC header {name:?} has a value that is not a valid HTTP header value"
                ))
            })?;
            map.insert(header_name, header_value);
        }
        Ok(Self {
            http: reqwest::Client::new(),
            url,
            headers: map,
        })
    }

    pub async fn get_events(
        &self,
        start_ledger: Option<u64>,
        cursor: Option<String>,
    ) -> Result<crate::parser::EventsPage, TridentError> {
        let params = GetEventsParams {
            start_ledger,
            filters: vec![],
            pagination: Pagination { limit: 200, cursor },
        };

        let req = JsonRpcRequest {
            jsonrpc: "2.0",
            id: 1,
            method: "getEvents",
            params,
        };

        let resp = self
            .http
            .post(&self.url)
            .headers(self.headers.clone())
            .json(&req)
            .send()
            .await
            .map_err(|e| TridentError::rpc(anyhow::Error::new(e).context("HTTP request failed")))?;

        let body: JsonRpcResponse<GetEventsResult> = resp.json().await.map_err(|e| {
            TridentError::rpc(anyhow::Error::new(e).context("Failed to decode RPC response"))
        })?;

        if let Some(err) = body.error {
            return Err(TridentError::rpc(anyhow::anyhow!(
                "RPC error {}: {}",
                err.code,
                err.message
            )));
        }

        let result = body
            .result
            .ok_or_else(|| TridentError::rpc(anyhow::anyhow!("Empty result in RPC response")))?;

        Ok(crate::parser::EventsPage {
            events: result.events,
            latest_ledger: result.latest_ledger,
        })
    }
}
