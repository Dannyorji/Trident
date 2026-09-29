# Fly.io Deployment Target (Unmaintained)

> [!WARNING]
> The Fly.io deployment path (`fly/api.toml`, `fly/grpc-api.toml`, `fly/indexer.toml`) is **unmaintained** and not supported for the testnet or mainnet launch.

## Selected Production Target

The official production deployment target for Trident is **Kubernetes via Helm** (`helm/trident/`).

### Decision Rationale:
1. **High Availability & Autoscaling**: Kubernetes Horizontal Pod Autoscaler (`HPA`) allows independent scaling of Go REST API and gRPC API replicas based on CPU and memory utilization.
2. **Stateful Ingestion & Schema Migrations**: Pre-upgrade Kubernetes Jobs (`migration-job.yaml`) guarantee zero schema drift before new pods accept live indexing traffic.
3. **Graceful Termination & Drain**: Kubernetes pre-stop lifecycle hooks and readiness probe removal prevent dropped WebSocket/SSE client streams during rolling deployments.

For production setup and runbooks, refer to [`docs/deployment.md`](../docs/deployment.md) and [`docs/kubernetes.md`](../docs/kubernetes.md).
