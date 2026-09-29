# Trident monitoring

`alerts.yml` is a standard Prometheus rule file. Load it via `rule_files:` in
your Prometheus config:

```yaml
# prometheus.yml
rule_files:
  - "monitoring/alerts.yml"

scrape_configs:
  - job_name: trident-indexer
    static_configs:
      - targets: ["indexer:9090"] # crates/indexer/src/metrics.rs, METRICS_PORT
  - job_name: trident-api
    static_configs:
      - targets: ["api:3000"] # GET /metrics, services/api
```

The `job=` labels above (`trident-indexer`, `trident-api`) must match what
`alerts.yml`'s `up{job="..."}` rules expect — rename both sides together if
you use different job names.

If you run the Prometheus Operator instead of vanilla Prometheus, convert
each `groups:` entry into a `PrometheusRule` CRD's `spec.groups` — the rule
syntax itself (`alert`, `expr`, `for`, `labels`, `annotations`) is unchanged.

## Validating changes

Before committing changes to `alerts.yml`, check it with `promtool` (ships
with Prometheus):

```bash
promtool check rules monitoring/alerts.yml
```

## Alert routing

`alertmanager.yml` routes `alerts.yml`'s alerts by `severity`/`service`
label to a receiver — load it via Alertmanager's `--config.file` flag, or
convert `route`/`receivers` into an `AlertmanagerConfig` CRD if running the
Prometheus Operator. Validate it with `amtool` (ships with Alertmanager):

```bash
amtool check-config monitoring/alertmanager.yml
```

The `on-call-critical`/`on-call-warning` receivers are wired into the
routing tree but have no delivery target configured yet — see the comments
in `alertmanager.yml` and [issue #445](https://github.com/Telocel-Labs/Trident/issues/445)
(naming an actual on-call owner and escalation path is a decision for the
project's operators, not something this file can invent).

## Verifying an alert actually fires

`../scripts/verify-indexer-silence-alerts.sh` runs a real Prometheus against
the real `alerts.yml`, kills a synthetic indexer target, and confirms one of
the silence-based alerts (`TridentIndexerHeartbeatStale`,
`TridentIndexerMetricsMissing`, `TridentIndexerProcessDown`) reaches
`state=firing` — the concrete proof behind issue #526's "killing the indexer
fires the alert" requirement. It can also point at a real staging
Prometheus (`SKIP_LOCAL_PROMETHEUS=1 PROMETHEUS_URL=...`) to verify the same
thing against a real deployment instead of the local synthetic target.

## Metrics catalog and runbook

- The full metrics catalog (every metric `alerts.yml` references, plus
  everything else Trident exports) lives in
  [`docs/metrics-catalog.md`](../docs/metrics-catalog.md).
- Per-alert runbooks (why the threshold, first steps when it fires) live in
  [`docs/runbooks/alerts.md`](../docs/runbooks/alerts.md).
