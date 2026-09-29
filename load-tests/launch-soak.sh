#!/usr/bin/env bash
# Combined launch soak harness for issue #440.
#
# Runs the existing Trident ingest, API read, batch, stats, and SSE stream load
# scripts together for a projected-launch soak window. Defaults to 24 hours so
# the command line matches the launch acceptance criteria, while still allowing
# shorter dry runs through SOAK_DURATION.
#
# Required tools: bash, k6, docker, stellar CLI, and the local/staging Trident
# stack described in load-tests/README.md.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="$(date -u +%Y%m%d-%H%M%S)"
OUT_DIR="${SCRIPT_DIR}/launch-soak-results/${RUN_ID}"
mkdir -p "$OUT_DIR"

BASE_URL="${BASE_URL:-http://localhost:3000}"
API_KEY="${API_KEY:-}"
SOAK_DURATION="${SOAK_DURATION:-24h}"
INGEST_SOAK_DURATION_SECONDS="${INGEST_SOAK_DURATION_SECONDS:-86400}"
CONCURRENT_STREAMS="${CONCURRENT_STREAMS:-50}"
HOLD_SECONDS="${HOLD_SECONDS:-60}"
LIST_VUS="${LIST_VUS:-40}"
GET_VUS="${GET_VUS:-20}"
BATCH_VUS="${BATCH_VUS:-10}"
STATS_VUS="${STATS_VUS:-10}"
PGB_VUS="${PGB_VUS:-100}"
PGB_REQS="${PGB_REQS:-10}"

export BASE_URL API_KEY

# Convert the k6-style SOAK_DURATION (e.g. 24h, 90m, 300s) into seconds so the
# stream relaunch loop knows when the soak window closes.
duration_to_seconds() {
  local value="$1"
  local number="${value%[smh]}"
  case "$value" in
    *h) echo $(( number * 3600 )) ;;
    *m) echo $(( number * 60 )) ;;
    *s) echo "$number" ;;
    *)  echo "$value" ;;
  esac
}

SOAK_SECONDS="$(duration_to_seconds "$SOAK_DURATION")"
if ! [ "$SOAK_SECONDS" -gt 0 ] 2>/dev/null; then
  echo "SOAK_DURATION must be a positive duration like 24h, 90m, or 300s (got: ${SOAK_DURATION})" >&2
  exit 1
fi


echo "Trident launch soak starting"
echo "  run id:              ${RUN_ID}"
echo "  base url:            ${BASE_URL}"
echo "  k6 duration:         ${SOAK_DURATION}"
echo "  ingest duration:     ${INGEST_SOAK_DURATION_SECONDS}s"
echo "  output dir:          ${OUT_DIR}"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Required command not found: $1" >&2
    exit 1
  fi
}

require_command k6
require_command docker

run_k6_background() {
  local name="$1"
  shift
  echo "Starting ${name}..."
  (
    set -o pipefail
    "$@" 2>&1 | tee "${OUT_DIR}/${name}.log"
  ) &
  echo $! > "${OUT_DIR}/${name}.pid"
}

run_k6_background events \
  env LIST_VUS="$LIST_VUS" GET_VUS="$GET_VUS" DURATION="$SOAK_DURATION" \
  k6 run "${SCRIPT_DIR}/events-load.js"

run_k6_background batch \
  env VUS="$BATCH_VUS" DURATION="$SOAK_DURATION" \
  k6 run "${SCRIPT_DIR}/batch-load.js"

run_k6_background stats \
  env VUS="$STATS_VUS" DURATION="$SOAK_DURATION" \
  k6 run "${SCRIPT_DIR}/stats-load.js"

# stream-load.js runs a single connect-and-hold iteration per VU, so one
# invocation only covers HOLD_SECONDS. Relaunch it for the whole soak window,
# otherwise SSE stops being exercised after the first cycle.
run_stream_soak() {
  local deadline=$(( $(date +%s) + SOAK_SECONDS ))
  local cycle=0
  local rc=0
  while [ "$(date +%s)" -lt "$deadline" ]; do
    cycle=$((cycle + 1))
    if ! env CONCURRENT_STREAMS="$CONCURRENT_STREAMS" HOLD_SECONDS="$HOLD_SECONDS" \
      k6 run "${SCRIPT_DIR}/stream-load.js" >> "${OUT_DIR}/stream.log" 2>&1; then
      echo "stream cycle ${cycle} failed" >> "${OUT_DIR}/stream.log"
      rc=1
    fi
  done
  return "$rc"
}

echo "Starting stream..."
run_stream_soak &
echo $! > "${OUT_DIR}/stream.pid"

run_k6_background pgbouncer \
  env VUS="$PGB_VUS" REQS="$PGB_REQS" \
  k6 run "${SCRIPT_DIR}/pgbouncer-validation.js"

if [ "${RUN_INGEST_SOAK:-1}" = "1" ]; then
  echo "Starting ingest soak..."
  (
    set -o pipefail
    SOAK_DURATION_SECONDS="$INGEST_SOAK_DURATION_SECONDS" \
      "${SCRIPT_DIR}/ingest-soak.sh" 2>&1 | tee "${OUT_DIR}/ingest-soak.log"
  ) &
  echo $! > "${OUT_DIR}/ingest-soak.pid"
else
  echo "Skipping ingest soak because RUN_INGEST_SOAK=${RUN_INGEST_SOAK}."
fi

status=0
for pid_file in "${OUT_DIR}"/*.pid; do
  [ -e "$pid_file" ] || continue
  name="$(basename "$pid_file" .pid)"
  pid="$(cat "$pid_file")"
  if wait "$pid"; then
    echo "${name}: passed" | tee -a "${OUT_DIR}/summary.txt"
  else
    echo "${name}: failed" | tee -a "${OUT_DIR}/summary.txt"
    status=1
  fi
done

cat > "${OUT_DIR}/run-metadata.env" <<EOF
BASE_URL=${BASE_URL}
SOAK_DURATION=${SOAK_DURATION}
INGEST_SOAK_DURATION_SECONDS=${INGEST_SOAK_DURATION_SECONDS}
CONCURRENT_STREAMS=${CONCURRENT_STREAMS}
HOLD_SECONDS=${HOLD_SECONDS}
LIST_VUS=${LIST_VUS}
GET_VUS=${GET_VUS}
BATCH_VUS=${BATCH_VUS}
STATS_VUS=${STATS_VUS}
PGB_VUS=${PGB_VUS}
PGB_REQS=${PGB_REQS}
RUN_ID=${RUN_ID}
EOF

echo "Launch soak finished with status ${status}. Results: ${OUT_DIR}"
exit "$status"