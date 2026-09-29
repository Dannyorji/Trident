#!/usr/bin/env bash
#
# Measure indexer catch-up throughput from a cold start (issue #420).
#
# Answers "how long does it take to recover from an N-ledger deficit", which is
# what sizes a testnet outage recovery and what tells a user whether "index my
# contract from ledger X" is a minutes or an hours answer.
#
# The figure is read from the indexer's own Prometheus metrics rather than
# timed externally, so the benchmark and production observability report the
# same number from the same source (`trident_indexer_catchup_ledgers_per_second`).
#
# Usage:
#   scripts/measure-catchup-throughput.sh [--deficit N] [--metrics-url URL]
#                                         [--timeout SECONDS] [--output FILE]
#
# Requires a running indexer with its metrics endpoint reachable and a database
# whose cursor is already behind the chain tip. `--deficit` records the deficit
# under test in the report; it does not itself rewind the cursor.

set -euo pipefail

METRICS_URL="${METRICS_URL:-http://localhost:9090/metrics}"
DEFICIT=""
TIMEOUT="${TIMEOUT:-1800}"
OUTPUT=""
SAMPLE_INTERVAL="${SAMPLE_INTERVAL:-5}"

usage() {
  sed -n '3,20p' "$0" | sed 's/^# \?//'
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --deficit)     DEFICIT="$2"; shift 2 ;;
    --metrics-url) METRICS_URL="$2"; shift 2 ;;
    --timeout)     TIMEOUT="$2"; shift 2 ;;
    --output)      OUTPUT="$2"; shift 2 ;;
    -h|--help)     usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 1 ;;
  esac
done

for cmd in curl awk; do
  command -v "$cmd" >/dev/null 2>&1 || {
    echo "error: $cmd is required but not installed" >&2
    exit 1
  }
done

# Scrape one gauge from the metrics endpoint. Prints nothing if absent — the
# catch-up gauges are only published while the indexer is actually behind.
scrape() {
  local metric="$1"
  curl -fsS --max-time 10 "$METRICS_URL" 2>/dev/null \
    | awk -v m="$metric" '$1 == m { print $2; exit }'
}

require_indexer() {
  if ! curl -fsS --max-time 10 "$METRICS_URL" >/dev/null 2>&1; then
    cat >&2 <<EOF
error: cannot reach the indexer metrics endpoint at $METRICS_URL

Start the indexer and ensure METRICS_PORT (default 9090) is reachable, or pass
--metrics-url. This script measures a running indexer; it does not start one.
EOF
    exit 1
  fi
}

require_indexer

start_lag="$(scrape trident_indexer_ledger_lag)"
start_lag="${start_lag:-0}"
start_epoch="$(date +%s)"

if [[ -z "$DEFICIT" ]]; then
  DEFICIT="${start_lag%.*}"
fi

echo "Measuring indexer catch-up throughput"
echo "  metrics endpoint : $METRICS_URL"
echo "  starting lag     : ${start_lag%.*} ledgers"
echo "  timeout          : ${TIMEOUT}s"
echo

if awk -v l="$start_lag" 'BEGIN { exit !(l + 0 < 1) }'; then
  cat >&2 <<EOF
error: the indexer is already caught up (lag ${start_lag%.*}).

There is no catch-up to measure. Rewind the cursor to create a deficit, e.g.
(the cursor key is namespaced per network — issue #600, replace <network>
with the one this indexer is configured for, e.g. testnet):

  psql "\$DATABASE_URL" -c \\
    "UPDATE system_state SET value = (value::bigint - 10000)::text \\
     WHERE key = 'latest_ledger_cursor:<network>'"

then restart the indexer and re-run this script.
EOF
  exit 1
fi

# Sample until the lag clears or the timeout expires. Peak and mean are both
# reported: the mean is the honest figure for capacity planning, while a peak
# far above it points at a constraint that only binds intermittently.
samples=0
rate_sum=0
rate_peak=0
events_sum=0
last_lag="$start_lag"

printf '%-10s %12s %14s %14s\n' "elapsed" "lag" "ledgers/s" "events/s"

while :; do
  now="$(date +%s)"
  elapsed=$(( now - start_epoch ))

  if (( elapsed >= TIMEOUT )); then
    echo
    echo "timeout after ${TIMEOUT}s with ${last_lag%.*} ledgers still outstanding" >&2
    break
  fi

  lag="$(scrape trident_indexer_ledger_lag)"
  lag="${lag:-$last_lag}"
  lps="$(scrape trident_indexer_catchup_ledgers_per_second)"
  eps="$(scrape trident_indexer_catchup_events_per_second)"

  if [[ -n "$lps" ]]; then
    samples=$(( samples + 1 ))
    rate_sum="$(awk -v s="$rate_sum" -v r="$lps" 'BEGIN { print s + r }')"
    events_sum="$(awk -v s="$events_sum" -v r="${eps:-0}" 'BEGIN { print s + r }')"
    rate_peak="$(awk -v p="$rate_peak" -v r="$lps" 'BEGIN { print (r > p) ? r : p }')"
    printf '%-10s %12s %14.1f %14.1f\n' "${elapsed}s" "${lag%.*}" "$lps" "${eps:-0}"
  fi

  last_lag="$lag"

  if awk -v l="$lag" 'BEGIN { exit !(l + 0 < 1) }'; then
    echo
    echo "caught up after ${elapsed}s"
    break
  fi

  sleep "$SAMPLE_INTERVAL"
done

total_elapsed=$(( $(date +%s) - start_epoch ))
ledgers_done="$(awk -v a="$start_lag" -v b="$last_lag" 'BEGIN { print a - b }')"

if (( samples == 0 )); then
  cat >&2 <<EOF

error: the indexer published no catch-up samples.

The catch-up gauges are only exported while the lag exceeds the threshold in
crates/indexer/src/metrics.rs (CATCHUP_LAG_THRESHOLD_LEDGERS). A run that
finishes faster than the first scrape produces no samples — re-run with a
larger deficit.
EOF
  exit 1
fi

mean_lps="$(awk -v s="$rate_sum" -v n="$samples" 'BEGIN { printf "%.2f", s / n }')"
mean_eps="$(awk -v s="$events_sum" -v n="$samples" 'BEGIN { printf "%.2f", s / n }')"
overall="$(awk -v l="$ledgers_done" -v t="$total_elapsed" \
  'BEGIN { printf "%.2f", (t > 0) ? l / t : 0 }')"

report() {
  cat <<EOF

Catch-up throughput
-------------------
deficit under test      : ${DEFICIT} ledgers
ledgers processed       : ${ledgers_done%.*}
wall clock              : ${total_elapsed}s
samples                 : ${samples}

ledgers/sec (mean)      : ${mean_lps}
ledgers/sec (peak)      : $(awk -v p="$rate_peak" 'BEGIN { printf "%.2f", p }')
ledgers/sec (overall)   : ${overall}
events/sec  (mean)      : ${mean_eps}

Projected catch-up time at the mean rate:
    1k ledgers : $(awk -v r="$mean_lps" 'BEGIN { printf "%.1fs", (r > 0) ? 1000 / r : 0 }')
   10k ledgers : $(awk -v r="$mean_lps" 'BEGIN { printf "%.1fs", (r > 0) ? 10000 / r : 0 }')
  100k ledgers : $(awk -v r="$mean_lps" 'BEGIN { printf "%.1fs", (r > 0) ? 100000 / r : 0 }')

Record these figures in docs/performance.md, along with the deployment shape
they were measured on — the numbers are meaningless without it.
EOF
}

report
if [[ -n "$OUTPUT" ]]; then
  report > "$OUTPUT"
  echo "written to $OUTPUT"
fi
