#!/usr/bin/env bash
# Launch chaos verification harness for issue #439.
#
# Exercises the major degradation assumptions by inducing RPC, Postgres, and
# Redis faults against a running compose-backed environment, then checking that
# the API reports a degraded state during the fault and recovers afterwards.
#
# This script intentionally records observations instead of hiding them behind a
# single pass/fail assertion. Surprises should be promoted into follow-up issues.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
OUT_DIR="${SCRIPT_DIR}/chaos-results/${RUN_ID}"
mkdir -p "$OUT_DIR"

BASE_URL="${BASE_URL:-http://localhost:3000}"
COMPOSE_FILE="${COMPOSE_FILE:-docker/docker-compose.yml}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-}"
POSTGRES_SERVICE="${POSTGRES_SERVICE:-postgres}"
REDIS_SERVICE="${REDIS_SERVICE:-redis}"
RPC_SERVICE="${RPC_SERVICE:-}"
FAULT_SECONDS="${FAULT_SECONDS:-30}"
RECOVERY_SECONDS="${RECOVERY_SECONDS:-45}"

compose() {
  if [ -n "$COMPOSE_PROJECT" ]; then
    docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" "$@"
  else
    docker compose -f "$COMPOSE_FILE" "$@"
  fi
}

FAILURES=0

# Records a probe and returns the observed HTTP status via PROBE_STATUS.
probe() {
  local label="$1"
  local path="${2:-/v1/ready}"
  local body="${OUT_DIR}/${label}.body"
  local status
  status="$(curl -sS -o "$body" -w '%{http_code}' --max-time 10 "${BASE_URL}${path}" || echo "000")"
  PROBE_STATUS="$status"
  printf '%s,%s,%s,%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$label" "$path" "$status" \
    | tee -a "${OUT_DIR}/probes.csv"
}

pass() {
  echo "PASS: $1" | tee -a "${OUT_DIR}/summary.txt"
}

fail() {
  echo "FAIL: $1" | tee -a "${OUT_DIR}/summary.txt"
  FAILURES=$((FAILURES + 1))
}

# /v1/ready returns 200 only when Postgres, Redis, and the gRPC backend all
# pass, and 503 when any dependency check fails (services/api/handlers/health.go).
# A dependency outage must therefore surface as 503, not as a hang (000) or a
# misleading 200.
expect_degraded() {
  local scenario="$1"
  case "$PROBE_STATUS" in
    503) pass "${scenario}: /v1/ready reported 503 while the dependency was down" ;;
    000) fail "${scenario}: /v1/ready did not respond within 10s (hang or connection failure)" ;;
    200) fail "${scenario}: /v1/ready still returned 200 while the dependency was down" ;;
    *)   fail "${scenario}: /v1/ready returned unexpected status ${PROBE_STATUS} during the fault" ;;
  esac
}

expect_healthy() {
  local scenario="$1"
  if [ "$PROBE_STATUS" = "200" ]; then
    pass "${scenario}: /v1/ready recovered to 200"
  else
    fail "${scenario}: /v1/ready did not recover (status ${PROBE_STATUS}) within ${RECOVERY_SECONDS}s"
  fi
}

record_ps() {
  local scenario="$1"
  compose ps >> "${OUT_DIR}/${scenario}.log" 2>&1 || true
}

run_stop_scenario() {
  local scenario="$1"
  local service="$2"
  echo "=== ${scenario} ===" | tee -a "${OUT_DIR}/summary.txt"
  probe "${scenario}-before"
  echo "[$scenario] stopping ${service}" | tee -a "${OUT_DIR}/${scenario}.log"
  compose stop "$service" >> "${OUT_DIR}/${scenario}.log" 2>&1 || true
  sleep 5
  probe "${scenario}-during"
  expect_degraded "$scenario"
  sleep "$FAULT_SECONDS"
  echo "[$scenario] starting ${service}" | tee -a "${OUT_DIR}/${scenario}.log"
  compose start "$service" >> "${OUT_DIR}/${scenario}.log" 2>&1 || true
  sleep "$RECOVERY_SECONDS"
  probe "${scenario}-after"
  expect_healthy "$scenario"
  record_ps "$scenario"
}

run_pause_scenario() {
  local scenario="$1"
  local service="$2"
  echo "=== ${scenario} ===" | tee -a "${OUT_DIR}/summary.txt"
  probe "${scenario}-before"
  echo "[$scenario] pausing ${service}" | tee -a "${OUT_DIR}/${scenario}.log"
  compose pause "$service" >> "${OUT_DIR}/${scenario}.log" 2>&1 || true
  sleep 5
  probe "${scenario}-during"
  expect_degraded "$scenario"
  sleep "$FAULT_SECONDS"
  echo "[$scenario] unpausing ${service}" | tee -a "${OUT_DIR}/${scenario}.log"
  compose unpause "$service" >> "${OUT_DIR}/${scenario}.log" 2>&1 || true
  sleep "$RECOVERY_SECONDS"
  probe "${scenario}-after"
  expect_healthy "$scenario"
  record_ps "$scenario"
}

run_redis_evicting() {
  local scenario="redis-evicting"
  echo "=== ${scenario} ===" | tee -a "${OUT_DIR}/summary.txt"
  probe "${scenario}-before"
  echo "[$scenario] flushing Redis DB" | tee -a "${OUT_DIR}/${scenario}.log"
  compose exec -T "$REDIS_SERVICE" redis-cli FLUSHDB >> "${OUT_DIR}/${scenario}.log" 2>&1 || true
  probe "${scenario}-during"
  # An emptied cache is a miss, not an outage: readiness must stay 200.
  expect_healthy "${scenario} (during flush)"
  sleep "$RECOVERY_SECONDS"
  probe "${scenario}-after"
  expect_healthy "$scenario"
  record_ps "$scenario"
}

echo "timestamp,label,path,status" > "${OUT_DIR}/probes.csv"
echo "Trident launch chaos run ${RUN_ID}" > "${OUT_DIR}/summary.txt"
echo "BASE_URL=${BASE_URL}" >> "${OUT_DIR}/summary.txt"
echo "COMPOSE_FILE=${COMPOSE_FILE}" >> "${OUT_DIR}/summary.txt"

run_stop_scenario postgres-down "$POSTGRES_SERVICE"
run_pause_scenario postgres-slow "$POSTGRES_SERVICE"
run_stop_scenario redis-down "$REDIS_SERVICE"
run_redis_evicting

if [ -n "$RPC_SERVICE" ]; then
  run_stop_scenario rpc-down "$RPC_SERVICE"
  run_pause_scenario rpc-slow "$RPC_SERVICE"
else
  cat <<'EOF' | tee -a "${OUT_DIR}/summary.txt"
RPC_SERVICE is not set, so rpc-down and rpc-slow were not induced automatically.
Set RPC_SERVICE to the compose service name for a local RPC container, or run the
same before/during/after probes while applying the fault at the network/provider layer.
EOF
fi

cat <<EOF | tee -a "${OUT_DIR}/summary.txt"

Review checklist:
- Check API and indexer logs for cursor corruption, data loss, or unbounded retries.
- Promote every unexpected behavior into its own follow-up issue.

Assertion failures: ${FAILURES}
EOF

echo "Chaos run complete. Results: ${OUT_DIR}"

if [ "$FAILURES" -gt 0 ]; then
  echo "Chaos verification FAILED with ${FAILURES} assertion failure(s)." >&2
  exit 1
fi

echo "Chaos verification passed all readiness assertions."
