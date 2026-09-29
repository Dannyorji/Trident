//! Async Rust client for the Trident Soroban event indexer.
//!
//! # Examples
//!
//! ```no_run
//! # tokio_test::block_on(async {
//! use futures::StreamExt;
//! use trident_sdk::{ContractStatsQuery, QueryParams, TridentClient, TridentConfig};
//!
//! let client = TridentClient::new(TridentConfig {
//!     api_url: "https://trident-api.fly.dev".into(),
//!     api_key: "tk_your_key".into(),
//!     ..Default::default()
//! })?;
//!
//! let health = client.get_health().await?;
//! println!("status = {}", health.status);
//!
//! let indexer = client.get_indexer_stats().await?;
//! println!("indexer = {}", indexer.status);
//!
//! let contracts = client
//!     .get_contract_stats(ContractStatsQuery {
//!         limit: Some(5),
//!         ..Default::default()
//!     })
//!     .await?;
//! println!("top contracts = {}", contracts.contracts.len());
//!
//! let mut events = client.iter_events(QueryParams {
//!     contract_id: Some("CAAAA...".into()),
//!     first: Some(25),
//!     ..Default::default()
//! });
//! while let Some(event) = events.next().await {
//!     println!("{}", event?.id);
//! }
//! # Ok::<(), trident_sdk::TridentError>(())
//! # });
//! ```

mod client;
mod errors;
pub mod openapi_models_gen;
mod retry;
mod subscription;
mod types;
pub mod webhook;

pub use client::TridentClient;
pub use errors::TridentError;
pub use openapi_models_gen::OpenApiModels as OpenAPIModels;
pub use retry::RetryConfig;
pub use subscription::Subscription;
pub use types::{
    ContractStats, ContractStatsQuery, ContractStatsResponse, EventType, HealthChecks,
    HealthResponse, IndexerStatsResponse, Network, PaginatedEvents, QueryParams, SorobanEvent,
    TridentConfig,
};
pub use webhook::{
    compute_signature, verify_signature, WebhookVerificationError, DEFAULT_TOLERANCE_SECONDS,
};
