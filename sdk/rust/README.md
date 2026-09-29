# trident-sdk

Rust client for the [Trident](https://github.com/Telocel-Labs/Trident) Soroban event indexer.

## Installation

Add to `Cargo.toml`:

```toml
[dependencies]
trident-sdk = "0.1"
tokio = { version = "1", features = ["full"] }
futures = "0.3"
```

## Quick start

```rust
use trident_sdk::{ContractStatsQuery, TridentClient, TridentConfig, QueryParams, Network};
use futures::StreamExt;

#[tokio::main]
async fn main() -> Result<(), trident_sdk::TridentError> {
    let client = TridentClient::new(TridentConfig {
        api_url: "https://trident-api.fly.dev".into(),
        api_key: "tk_your_key".into(),
        network: Network::Testnet,
        ..Default::default()
    })?;

    // Query historical events
    let page = client.query_events(QueryParams {
        contract_id: Some("CAAAA...".into()),
        topic_0: Some("transfer".into()),
        first: Some(50),
        ..Default::default()
    }).await?;

    println!("Found {} events", page.events.len());

    // Paginate
    if let Some(cursor) = page.next_cursor {
        let next_page = client.query_events(QueryParams {
            after: Some(cursor),
            ..Default::default()
        }).await?;
        println!("{} more events", next_page.events.len());
    }

    // Fetch single event
    let event = client.get_event_by_id("550e8400-e29b-41d4-a716-446655440000").await?;
    println!("Event: {:?}", event);

    // Service health + contract analytics
    let health = client.get_health().await?;
    let stats = client.get_indexer_stats().await?;
    let contracts = client.get_contract_stats(ContractStatsQuery {
        limit: Some(5),
        ..Default::default()
    }).await?;
    println!("{} / {} / {}", health.status, stats.status, contracts.contracts.len());

    // Stream every historical event across pages
    let mut history = client.iter_events(QueryParams {
        contract_id: Some("CAAAA...".into()),
        first: Some(25),
        ..Default::default()
    });
    while let Some(event) = history.next().await {
        println!("historical: {}", event?.id);
    }

    // Real-time subscription
    let mut sub = client.subscribe_to_contract("CAAAA...", Some("transfer")).await?;
    while let Some(result) = sub.next().await {
        println!("{:?}", result?);
    }

    Ok(())
}
```

## Configuration

`api_url` and `api_key` can be left empty in `TridentConfig`; `TridentClient::new`
falls back to the `TRIDENT_BASE_URL` / `TRIDENT_API_KEY` environment
variables, with an explicit field always taking precedence:

```rust
use trident_sdk::{TridentClient, TridentConfig};

// Reads TRIDENT_API_KEY / TRIDENT_BASE_URL from the environment.
let client = TridentClient::new(TridentConfig::default())?;
```

If no API key is available from either source, `TridentClient::new` returns
`Err(TridentError::MissingApiKey)`. The key is never logged: `TridentConfig`'s
`Debug` implementation always redacts it (e.g. `***a1b2`).

## Publishing

```bash
cargo publish --package trident-sdk
```

Dry-run check (runs in CI):

```bash
cargo publish --dry-run --package trident-sdk
```

## Regenerating OpenAPI models

See [docs/sdk-regeneration.md](../../docs/sdk-regeneration.md) for the full cross-SDK procedure (regenerating all SDKs together, version consistency, testing after regeneration). Quick version — install the generator dependency once with `python3 -m pip install PyYAML`, then run:

```bash
python3 scripts/generate_sdk_models.py --language rust
```

## Examples

Compile or run the bundled example:

```bash
cargo run --example events_and_stats
```
