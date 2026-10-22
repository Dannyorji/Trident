use std::net::SocketAddr;
use std::str::FromStr;

use opentelemetry_otlp::WithExportConfig;
use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::EnvFilter;

pub mod trident {
    tonic::include_proto!("trident");
}

mod config;
mod services;

/// Read-heavy service with moderate concurrency, so a moderate pool is correct.
const DEFAULT_DB_POOL_SIZE: u32 = 10;

fn init_tracer() -> Option<opentelemetry_sdk::trace::Tracer> {
    let endpoint = std::env::var("OTEL_EXPORTER_OTLP_ENDPOINT").ok()?;
    let sampling_ratio = std::env::var("OTEL_SAMPLING_RATIO")
        .ok()
        .and_then(|s| {
            let parsed = s.parse::<f64>().ok();
            if !matches!(parsed, Some(v) if (0.0..=1.0).contains(&v)) {
                eprintln!(
                    "invalid OTEL_SAMPLING_RATIO {s:?}, must be a float in [0,1]; falling back to default"
                );
                return None;
            }
            parsed
        })
        .unwrap_or(0.1);

    opentelemetry::global::set_text_map_propagator(
        opentelemetry_sdk::propagation::TraceContextPropagator::new(),
    );

    // Issue #762: traces include request paths, contract ids, key ids and
    // error messages, so the exporter connection defaults to TLS. Insecure
    // (plaintext) requires an explicit opt-in via the standard OTel env var
    // — e.g. for a local collector sidecar in development. Collector auth
    // headers (OTEL_EXPORTER_OTLP_HEADERS) are already read automatically by
    // opentelemetry-otlp's tonic exporter, so no extra wiring is needed here.
    let insecure = std::env::var("OTEL_EXPORTER_OTLP_INSECURE")
        .ok()
        .map(|v| v.eq_ignore_ascii_case("true") || v == "1")
        .unwrap_or(false);

    let mut tonic_exporter = opentelemetry_otlp::new_exporter()
        .tonic()
        .with_endpoint(endpoint);
    if !insecure {
        tonic_exporter =
            tonic_exporter.with_tls_config(tonic::transport::ClientTlsConfig::new());
    }

    match opentelemetry_otlp::new_pipeline()
        .tracing()
        .with_exporter(tonic_exporter)
        .with_trace_config(
            opentelemetry_sdk::trace::Config::default()
                .with_sampler(opentelemetry_sdk::trace::Sampler::TraceIdRatioBased(
                    sampling_ratio,
                ))
                .with_resource(opentelemetry_sdk::Resource::new(vec![
                    opentelemetry::KeyValue::new("service.name", "trident-grpc-api"),
                ])),
        )
        .install_batch(opentelemetry_sdk::runtime::Tokio)
    {
        Ok(tracer) => Some(tracer),
        Err(e) => {
            eprintln!("Failed to initialise OpenTelemetry tracer: {e}");
            None
        }
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let tracer = init_tracer();
    tracing_subscriber::registry()
        .with(EnvFilter::from_default_env())
        .with(tracing_subscriber::fmt::layer())
        .with(tracer.map(|t| tracing_opentelemetry::layer().with_tracer(t)))
        .init();

    let cfg = config::Config::from_env().unwrap_or_else(|e| {
        eprintln!("{e}");
        std::process::exit(1);
    });

    let pool_size = std::env::var("GRPC_API_DB_POOL_SIZE")
        .ok()
        .and_then(|s| s.parse::<u32>().ok())
        .filter(|&n| n > 0)
        .unwrap_or(DEFAULT_DB_POOL_SIZE);

    // statement_cache_capacity(0) disables named prepared statements so the pool
    // is safe behind PgBouncer in transaction mode. See docs/deployment.md (#87).
    let connect_options =
        PgConnectOptions::from_str(&cfg.database_url)?.statement_cache_capacity(0);
    let db_pool = PgPoolOptions::new()
        .max_connections(pool_size)
        .connect_with(connect_options)
        .await?;
    tracing::info!(pool_size, "Database connected via pool");

    let redis_url = std::env::var("REDIS_URL").expect("REDIS_URL is required");
    let redis_manager = redis::Client::open(redis_url)?
        .get_connection_manager()
        .await?;
    tracing::info!("Redis connected");

    let addr: SocketAddr = cfg.grpc_addr.parse()?;

    tracing::info!(%addr, "Trident gRPC server listening");

    let events_service = services::events::EventsServiceImpl::new(db_pool, redis_manager);

    let mut server_builder = tonic::transport::Server::builder();

    if let Some(mtls) = &cfg.mtls {
        // mTLS behind a flag (issue #320): require and verify a client
        // certificate signed by GRPC_MTLS_CA_CERT before accepting any RPC.
        // Cert/key files are mounted from a Kubernetes Secret, never baked
        // into the image — see docs/kubernetes.md#internal-mtls.
        let cert = std::fs::read(&mtls.server_cert_path)?;
        let key = std::fs::read(&mtls.server_key_path)?;
        let ca = std::fs::read(&mtls.ca_cert_path)?;

        let identity = tonic::transport::Identity::from_pem(cert, key);
        let client_ca = tonic::transport::Certificate::from_pem(ca);
        let tls_config = tonic::transport::ServerTlsConfig::new()
            .identity(identity)
            .client_ca_root(client_ca);

        server_builder = server_builder.tls_config(tls_config)?;
        tracing::info!("internal gRPC mTLS enabled (GRPC_MTLS_ENABLED=true)");
    } else {
        tracing::info!("internal gRPC mTLS disabled — plaintext within the cluster network");
    }

    server_builder
        .add_service(trident::events_server::EventsServer::new(events_service))
        .serve_with_shutdown(addr, async {
            tokio::signal::ctrl_c().await.ok();
            tracing::info!("Received shutdown signal");
        })
        .await?;

    // Flush OpenTelemetry spans on shutdown (issue #733)
    opentelemetry::global::shutdown_tracer_provider();

    Ok(())
}
