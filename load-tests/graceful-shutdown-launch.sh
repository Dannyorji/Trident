#!/usr/bin/env bash
# Rolling shutdown verification harness for issue #442.
#
# Runs API and SSE load while terminating the API and indexer services with
# SIGTERM. The resulting logs make it clear whether readiness recovers, requests
# fail, SSE clients reconnect cleanly, and the indexer exits without an
# ambiguous cursor.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
OUT_DIR="${SCRIPT_DIR}/shutdown-results/${RUN_ID}"
mkdir -p "$OUT_DIR"

BASE_URL="${BASE_URL:-http://localhost:3000}"
COMPOSE_FILE="${COMPOSE_FILE:-docker/docker-compose.yml}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-}"
API_SERVICE="${API_SERVICE:-api}"
INDEXER_SERVICE="${INDEXER_SERVICE:-indexer}"
DRAIN_SECONDS="${DRAIN_SECONDS:-30}"
RECOVERY_SECONDS="${RECOVERY_SECONDS:-45}"
API_LOAD_DURATION="${API_LOAD_DURATION:-2m}"
CONCURRENT_STREAMS="${CONCURRENT_STREAMS:-20}"
HOLD_SECONDS="${HOLD_SECONDS:-90}"
API_KEY="${API_KEY:-}"

export BASE_URL API_KEY

compose() {
  if [ -n "$COMPOSE_PROJECT" ]; then
    docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" "$@"
  else
    docker compose -f "$COMPOSE_FILE" "$@"
  fi
}

FAILURES=0

probe_ready() {
  local label="$1"
  local status
  status="$(curl -sS -o "${OUT_DIR}/${label}.body" -w '%{http_code}' --max-time 10 "${BASE_URL}/v1/ready" || echo "000")"
  PROBE_STATUS="$status"
  printf '%s,%s,%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$label" "$status" \
    | tee -a "${OUT_DIR}/ready.csv"
}

pass() {
  echo "PASS: $1" | tee -a "${OUT_DIR}/summary.txt"
}

fail() {
  echo "FAIL: $1" | tee -a "${OUT_DIR}/summary.txt"
  FAILURES=$((FAILURES + 1))
}

start_load() {
  env LIST_VUS=10 GET_VUS=5 DURATION="$API_LOAD_DURATION" \
    k6 run "${SCRIPT_DIR}/events-load.js" > "${OUT_DIR}/events-load.log" 2>&1 &
  echo $! > "${OUT_DIR}/events-load.pid"

  env CONCURRENT_STREAMS="$CONCURRENT_STREAMS" HOLD_SECONDS="$HOLD_SECONDS" \
    k6 run "${SCRIPT_DIR}/stream-load.js" > "${OUT_DIR}/stream-load.log" 2>&1 &
  echo $! > "${OUT_DIR}/stream-load.pid"
}

# During SIGTERM the expectation differs per service:
#   api     - the process is going away, so an unreachable endpoint (000) or an
#             intentional 503 are both acceptable; a 200 means it never drained.
#   indexer - the API is untouched, so readiness must stay 200 throughout.
terminate_service() {
  local scenario="$1"
  local service="$2"
  local expectation="$3"

  echo "=== ${scenario}: SIGTERM ${service} ===" | tee -a "${OUT_DIR}/summary.txt"

  probe_ready "${scenario}-before"
  if [ "$PROBE_STATUS" = "200" ]; then
    pass "${scenario}: /v1/ready was 200 before SIGTERM"
  else
    fail "${scenario}: /v1/ready was ${PROBE_STATUS} before SIGTERM (environment was not healthy to begin with)"
  fi

  compose kill -s SIGTERM "$service" >> "${OUT_DIR}/${scenario}.log" 2>&1 || true
  sleep "$DRAIN_SECONDS"
  probe_ready "${scenario}-during-drain"

  case "$expectation" in
    unreachable-or-degraded)
      case "$PROBE_STATUS" in
        000|503) pass "${scenario}: /v1/ready reported ${PROBE_STATUS} while draining" ;;
        200)     fail "${scenario}: /v1/ready still returned 200 after SIGTERM (traffic would keep arriving mid-drain)" ;;
        *)       fail "${scenario}: /v1/ready returned unexpected status ${PROBE_STATUS} while draining" ;;
      esac
      ;;
    stay-healthy)
      if [ "$PROBE_STATUS" = "200" ]; then
        pass "${scenario}: API stayed ready while ${service} shut down"
      else
        fail "${scenario}: API readiness dropped to ${PROBE_STATUS} when only ${service} was terminated"
      fi
      ;;
  esac

  compose up -d "$service" >> "${OUT_DIR}/${scenario}.log" 2>&1 || true
  sleep "$RECOVERY_SECONDS"
  probe_ready "${scenario}-after-recovery"
  if [ "$PROBE_STATUS" = "200" ]; then
    pass "${scenario}: /v1/ready recovered to 200 after restart"
  else
    fail "${scenario}: /v1/ready did not recover (status ${PROBE_STATUS}) within ${RECOVERY_SECONDS}s"
  fi

  compose logs --no-color --tail=200 "$service" > "${OUT_DIR}/${scenario}.service.log" 2>&1 || true
}

echo "timestamp,label,status" > "${OUT_DIR}/ready.csv"
echo "Trident rolling shutdown run ${RUN_ID}" > "${OUT_DIR}/summary.txt"
echo "BASE_URL=${BASE_URL}" >> "${OUT_DIR}/summary.txt"
echo "COMPOSE_FILE=${COMPOSE_FILE}" >> "${OUT_DIR}/summary.txt"

start_load
sleep 5
terminate_service api-shutdown "$API_SERVICE" unreachable-or-degraded
terminate_service indexer-shutdown "$INDEXER_SERVICE" stay-healthy

status=0
for pid_file in "${OUT_DIR}"/*.pid; do
  [ -e "$pid_file" ] || continue
  name="$(basename "$pid_file" .pid)"
  pid="$(cat "$pid_file")"
  if wait "$pid"; then
    echo "${name}: completed" | tee -a "${OUT_DIR}/summary.txt"
  else
    echo "${name}: failed during shutdown run" | tee -a "${OUT_DIR}/summary.txt"
    status=1
  fi
done

cat <<EOF | tee -a "${OUT_DIR}/summary.txt"

Review checklist:
- Confirm in-flight API requests drained or returned intentional 503s during SIGTERM.
- Confirm SSE clients did not hang silently and can reconnect with Last-Event-ID.
- Confirm indexer logs show cursor commit or an explicit safe retry point before exit.
- Confirm Kubernetes terminationGracePeriodSeconds/preStop settings exceed measured drain time.

Assertion failures: ${FAILURES}
EOF

echo "Shutdown verification complete. Results: ${OUT_DIR}"

if [ "$FAILURES" -gt 0 ]; then
  echo "Shutdown verification FAILED with ${FAILURES} assertion failure(s)." >&2
  status=1
fi

exit "$status"
