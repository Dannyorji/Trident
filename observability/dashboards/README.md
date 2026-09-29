# Trident Grafana Dashboards

This directory contains Grafana dashboard JSON files for monitoring Trident infrastructure.

## Available Dashboards

### launch-health.json (Issue #398)

**Purpose**: Single-pane-of-glass health dashboard for testnet launch monitoring.

**URL**: Once imported, access via Grafana at `/d/trident-launch-health`

**What it shows**:
- **System Health Status**: Overall health of Indexer, API, Postgres, and Redis
- **Indexer Ledger Lag**: How far behind the indexer is from the network tip
- **Events Ingested Per Minute**: Rate of event ingestion from Stellar
- **API Request Rate**: Requests per second by endpoint
- **API Error Ratio**: 4xx and 5xx error rates
- **API Request Latency (p95)**: 95th percentile response times
- **RPC Failover State**: Current active RPC endpoint and failover status
- **Redis Stream Length**: Backlog in the event stream
- **DB Pool Saturation**: Connection pool utilization percentage
- **Webhook Delivery Success Ratio**: Percentage of successful webhook deliveries
- **Additional panels**: DB pool details, webhook delivery rates, Redis connection status, event processing lag, rate limit rejections, and database query errors

**Use case**: During launch day, this is the one URL to watch. It answers "is Trident healthy right now?" for operators who didn't build the system.

**Auto-refresh**: 10 seconds (configurable in dashboard settings)

**Time range**: Last 1 hour (default, adjustable)

---

### rpc-health.json

**Purpose**: Detailed monitoring of Stellar RPC provider health and failover behavior.

**What it shows**:
- RPC call latency percentiles (p50, p95, p99) by method
- Call volume by method and endpoint
- Error rates by type
- Active endpoint and failover counts
- Timeout and retry rates

**Use case**: Deep-dive diagnostics when RPC issues are suspected.

---

## Importing Dashboards

### Option 1: Grafana UI
1. Log in to Grafana
2. Navigate to **Dashboards** → **Import**
3. Upload the JSON file or paste its contents
4. Select your Prometheus datasource
5. Click **Import**

### Option 2: Provisioning (Kubernetes/Helm)
If you're deploying with the Helm chart, dashboards in this directory are automatically provisioned when you enable Grafana in `values.yaml`:

```yaml
grafana:
  enabled: true
  dashboards:
    default:
      trident-launch-health:
        file: dashboards/launch-health.json
      trident-rpc-health:
        file: dashboards/rpc-health.json
```

### Option 3: Grafana API
```bash
curl -X POST \
  -H "Authorization: Bearer ${GRAFANA_API_KEY}" \
  -H "Content-Type: application/json" \
  -d @launch-health.json \
  "${GRAFANA_URL}/api/dashboards/db"
```

---

## Required Prometheus Metrics

These dashboards expect the following Prometheus metrics to be exported:

### Indexer Metrics
- `trident_indexer_ledger_lag` — ledger gap between network tip and last processed
- `trident_indexer_events_ingested_total` — counter of ingested events
- `trident_indexer_event_processing_lag_seconds` — time lag in event processing
- `trident_indexer_rpc_active_endpoint` — current active RPC endpoint index
- `trident_indexer_rpc_call_duration_seconds_*` — RPC call latency histogram
- `trident_indexer_rpc_errors_total` — RPC error counter
- `trident_indexer_rpc_failovers_total` — RPC failover event counter

### API Metrics
- `trident_api_http_requests_total` — counter of HTTP requests by endpoint, status
- `trident_api_http_request_duration_seconds_bucket` — request latency histogram
- `trident_api_rate_limit_rejections_total` — rate limit rejection counter

### Database Metrics
- `trident_db_pool_connections_acquired` — active DB connections
- `trident_db_pool_connections_idle` — idle connections in pool
- `trident_db_pool_connections_max` — max pool size
- `trident_db_query_errors_total` — database query error counter

### Redis Metrics
- `trident_redis_stream_length` — event stream backlog length
- `redis_up` — Redis availability (1 = up, 0 = down)

### Webhook Metrics
- `trident_webhook_deliveries_total` — webhook delivery counter by status

### Service Availability
- `up` — standard Prometheus up metric for all jobs

---

## Customization

All dashboards are editable. Common customizations:

1. **Time range**: Change default from `now-1h` to `now-6h` or `now-24h`
2. **Refresh interval**: Adjust from `10s` to `30s` or `1m` depending on load
3. **Thresholds**: Modify color thresholds for your SLOs (e.g., error ratio yellow at 1%, red at 5%)
4. **Alerts**: Add alert rules to panels via **Panel settings** → **Alert**

---

## Troubleshooting

**No data showing up?**
- Verify your Prometheus datasource is configured and reachable
- Check that the Trident services are exporting metrics on `/metrics` endpoints
- Confirm metric names match (they may have changed if you're using a custom build)

**"Template variable not found" error?**
- Ensure `DS_PROMETHEUS` variable is set to your Prometheus datasource UID
- Re-import the dashboard and select the datasource during import

**Panels showing "N/A"?**
- The metric may not be implemented yet (check the metrics list above)
- The service exporting that metric may be down
- The metric name may have changed (check Prometheus `/api/v1/label/__name__/values`)

---

## Contributing

When adding new dashboards:
1. Use schema version 39 or later
2. Include `DS_PROMETHEUS` as a templating variable
3. Add descriptive titles and panel descriptions
4. Document required metrics in this README
5. Use semantic panel IDs (increment from existing max)
6. Tag appropriately (`trident`, `indexer`, `api`, etc.)
