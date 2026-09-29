#!/usr/bin/env bash
# Verify that every metric name referenced in Prometheus alert rules actually
# exists in the live /metrics endpoints (issue #393).
#
# Usage:
#   ./scripts/verify-alert-metrics.sh <api-metrics-url> <indexer-metrics-url>
#
# Example (local docker compose):
#   ./scripts/verify-alert-metrics.sh \
#     http://localhost:3000/metrics \
#     http://localhost:9090/metrics
#
# Example (staging):
#   ./scripts/verify-alert-metrics.sh \
#     https://api-staging.trident.example/metrics \
#     https://indexer-staging.trident.example/metrics
#
# Exit codes:
#   0 - all referenced metrics exist
#   1 - one or more metrics are missing
#   2 - usage error or connectivity failure

set -euo pipefail

if [ $# -ne 2 ]; then
  echo "Usage: $0 <api-metrics-url> <indexer-metrics-url>" >&2
  echo "" >&2
  echo "Example:" >&2
  echo "  $0 http://localhost:3000/metrics http://localhost:9090/metrics" >&2
  exit 2
fi

API_METRICS_URL="$1"
INDEXER_METRICS_URL="$2"

echo "=== Fetching live metrics ==="
API_METRICS=$(curl --silent --fail --max-time 10 "$API_METRICS_URL" || {
  echo "ERROR: failed to fetch $API_METRICS_URL" >&2
  exit 2
})
INDEXER_METRICS=$(curl --silent --fail --max-time 10 "$INDEXER_METRICS_URL" || {
  echo "ERROR: failed to fetch $INDEXER_METRICS_URL" >&2
  exit 2
})

echo "✓ Fetched API metrics ($(echo "$API_METRICS" | wc -l) lines)"
echo "✓ Fetched indexer metrics ($(echo "$INDEXER_METRICS" | wc -l) lines)"

# Collect the metric names both endpoints know about.
#
# Both payloads must go through the same extraction. Without the braces the
# first `echo` is its own command and the API metrics reach EMITTED_METRICS
# raw — values and `# HELP`/`# TYPE` lines included — so nothing matches by
# name and the comparison below silently misbehaves.
#
# Two sources, deliberately:
#
#   sample lines  `metric_name{labels} value`  — a series with an observation
#   HELP/TYPE     `# TYPE metric_name counter` — the metric is declared
#
# A counter or histogram with no observations yet prints its HELP/TYPE header
# and no samples, so sample lines alone would report a perfectly good metric
# as missing purely because nothing had exercised it — which is the normal
# state for `trident_api_http_requests_total` on a freshly started API. We
# are checking that a name exists, not that traffic has happened, so a
# declaration counts.
#
# Histograms declare the base name but emit `_bucket`/`_sum`/`_count` series,
# so each declared name is expanded to those suffixes too.
DECLARED=$( { echo "$API_METRICS"; echo "$INDEXER_METRICS"; } \
  | grep -E '^# (HELP|TYPE) ' \
  | awk '{print $3}' \
  | sort -u)

DECLARED_SUFFIXED=$(
  printf '%s\n' "$DECLARED"
  printf '%s\n' "$DECLARED" | sed 's/$/_bucket/'
  printf '%s\n' "$DECLARED" | sed 's/$/_sum/'
  printf '%s\n' "$DECLARED" | sed 's/$/_count/'
)

SAMPLED=$( { echo "$API_METRICS"; echo "$INDEXER_METRICS"; } \
  | grep -v '^#' \
  | grep -v '^$' \
  | sed -E 's/^([a-zA-Z_:][a-zA-Z0-9_:]*).*/\1/')

EMITTED_METRICS=$(printf '%s\n%s\n' "$SAMPLED" "$DECLARED_SUFFIXED" | sort -u)

echo ""
echo "=== Extracting metric names from alert rules ==="

# Extract the metric names each rule file selects.
#
# This parses the YAML rather than grepping `expr:` lines, because most of our
# expressions are block scalars (`expr: >` / `expr: |`) whose body lives on the
# following lines — a line-oriented grep silently skips them. All four
# burn-rate recording rules are written that way.
#
# Within an expression we drop three things that are not metrics: PromQL
# keywords and functions, anything inside a `{...}` label selector (label keys
# and values), and any bare token with no `_` or `:` in it. That last rule is
# what keeps `GET`, `api`, `v1`, `up`, `job` and friends out — every metric we
# emit is namespaced (`trident_*`) or a recording rule (`trident:*`).
extract_metrics_from_alerts() {
  python3 - "$1" <<'PYEOF'
import re, sys, yaml

FUNCS = {
    "rate","irate","increase","sum","avg","min","max","count","stddev","stdvar",
    "topk","bottomk","quantile","histogram_quantile","time","bool","by","and",
    "or","unless","on","ignoring","group_left","group_right","offset","without",
    "avg_over_time","min_over_time","max_over_time","sum_over_time",
    "count_over_time","quantile_over_time","stddev_over_time","stdvar_over_time",
    "last_over_time","present_over_time","absent","absent_over_time","changes",
    "deriv","predict_linear","delta","idelta","resets","floor","ceil","round",
    "exp","ln","log2","log10","sqrt","abs","le","clamp","clamp_min","clamp_max",
    "vector","scalar","label_replace","label_join","count_values","sort",
    "sort_desc","timestamp","year","month","day_of_month","days_in_month",
}

# Prefixes owned by exporters we deploy alongside Trident rather than by
# Trident itself. A metric under one of these is verified by that exporter
# being present in the deployment, which is a different check from this one.
EXTERNAL_EXPORTER_PREFIXES = ("node_", "pg_", "redis_", "container_")

doc = yaml.safe_load(open(sys.argv[1], encoding="utf-8")) or {}
found = set()
for group in doc.get("groups") or []:
    for rule in group.get("rules") or []:
        expr = rule.get("expr")
        if not expr:
            continue
        expr = str(expr)
        # Drop label selectors — their keys and values are not metric names.
        expr = re.sub(r"\{[^}]*\}", " ", expr)
        # Drop range/offset windows, so `[5m:1m]` cannot yield a bare `m:1m`.
        expr = re.sub(r"\[[^\]]*\]", " ", expr)
        for tok in re.findall(r"[a-zA-Z_:][a-zA-Z0-9_:]*", expr):
            if tok in FUNCS:
                continue
            # Every metric we emit is namespaced or a recording rule.
            if "_" not in tok and ":" not in tok:
                continue
            # A recording rule is produced by Prometheus, not exported by a
            # service, so it will never appear on a /metrics endpoint. Its
            # own inputs are checked when its `record:` rule is scanned.
            if ":" in tok:
                continue
            # Metrics exported by a different agent, not by Trident. Disk
            # capacity alerts (issue #432) read node_exporter's filesystem
            # series, which will never appear on the API or indexer /metrics
            # endpoints — the indexer does not, and should not, report the
            # host's disk usage. Excluded by prefix rather than by name so a
            # new node_* alert does not have to update this script.
            if tok.startswith(EXTERNAL_EXPORTER_PREFIXES):
                continue
            found.add(tok)

for name in sorted(found):
    print(name)
PYEOF
}

ALERT_FILES=(
  "monitoring/alerts.yml"
  "observability/burn-rate-alerts.yml"
  "observability/rpc-alerts.yml"
)

REFERENCED_METRICS=()
for alert_file in "${ALERT_FILES[@]}"; do
  if [ ! -f "$alert_file" ]; then
    echo "WARNING: $alert_file not found, skipping" >&2
    continue
  fi
  echo "Extracting from $alert_file..."
  while IFS= read -r metric; do
    REFERENCED_METRICS+=("$metric")
  done < <(extract_metrics_from_alerts "$alert_file")
done

# Deduplicate
REFERENCED_METRICS=($(printf '%s\n' "${REFERENCED_METRICS[@]}" | sort -u))

echo "✓ Found ${#REFERENCED_METRICS[@]} unique metric names referenced in alerts"
echo ""

echo "=== Verifying metric existence ==="
MISSING_METRICS=()

for metric in "${REFERENCED_METRICS[@]}"; do
  # Check if this metric name exists in the emitted metrics
  # Use grep -F for fixed string match (no regex interpretation)
  # -x anchors to the whole line. Without it a referenced name matches any
  # emitted name that merely contains it, so a metric nothing exports still
  # passes as long as some longer series shares the prefix — which would let
  # exactly the drift this script exists to catch through.
  if printf '%s\n' "$EMITTED_METRICS" | grep -qxF "$metric"; then
    echo "✓ $metric"
  else
    echo "✗ $metric (NOT FOUND in live /metrics)"
    MISSING_METRICS+=("$metric")
  fi
done

echo ""
if [ ${#MISSING_METRICS[@]} -eq 0 ]; then
  echo "SUCCESS: All ${#REFERENCED_METRICS[@]} referenced metrics exist in live /metrics endpoints"
  exit 0
else
  echo "FAILURE: ${#MISSING_METRICS[@]} metric(s) referenced by alerts do not exist:" >&2
  for metric in "${MISSING_METRICS[@]}"; do
    echo "  - $metric" >&2
  done
  echo "" >&2
  echo "These metrics are queried by alert rules but are never emitted." >&2
  echo "The alerts referencing them will never fire (PromQL evaluates to empty)." >&2
  exit 1
fi
