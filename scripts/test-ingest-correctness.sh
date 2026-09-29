#!/usr/bin/env bash
#
# End-to-end ingest correctness against a known testnet contract (issue #419).
#
# Verifies that over a wide real ledger range every event is present exactly
# once, decoded correctly, and in order — checked against the RPC's own
# server-assigned fields and an independent XDR decode path, not against our
# own decoder's output.
#
# The assertions live in Rust (crates/indexer/src/testnet_correctness.rs) so
# the same checks run unattended on a schedule; see
# .github/workflows/testnet-correctness.yml. This script is the operator-facing
# entry point for running them on demand.
#
# Usage:
#   scripts/test-ingest-correctness.sh [--rpc-url URL] [--contract-id ID]
#                                      [--ledger-span N]
#
# Defaults to the public Stellar testnet RPC, which needs no credentials.

set -euo pipefail

RPC_URL="${TESTNET_RPC_URL:-https://soroban-testnet.stellar.org}"
CONTRACT_ID="${TESTNET_CONTRACT_ID:-}"
LEDGER_SPAN="${TESTNET_LEDGER_SPAN:-400}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --rpc-url)     RPC_URL="$2"; shift 2 ;;
    --contract-id) CONTRACT_ID="$2"; shift 2 ;;
    --ledger-span) LEDGER_SPAN="$2"; shift 2 ;;
    -h|--help) sed -n '3,20p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

command -v curl >/dev/null 2>&1 || {
  echo "error: curl is required" >&2
  exit 1
}

echo "End-to-end ingest correctness"
echo "  rpc endpoint : $RPC_URL"
echo "  contract     : ${CONTRACT_ID:-<all contracts in range>}"
echo "  ledger span  : $LEDGER_SPAN"
echo

# Fail fast with a clear message if the endpoint is unreachable, rather than
# letting the Rust suite surface it as an opaque transport error several
# minutes in.
echo "==> Checking RPC reachability"
tip="$(curl -fsS --max-time 20 -X POST "$RPC_URL" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"getLatestLedger"}' \
  | sed -n 's/.*"sequence":\([0-9]*\).*/\1/p')"

if [[ -z "$tip" ]]; then
  echo "error: $RPC_URL did not return a usable getLatestLedger response" >&2
  exit 1
fi
echo "  chain tip: ledger $tip"

# Public testnet retains only a rolling window (roughly 120k ledgers) and is
# periodically reset, so a span anchored too far back fails with an
# out-of-range error rather than testing anything.
if (( LEDGER_SPAN > 100000 )); then
  echo "warning: a span of $LEDGER_SPAN may exceed the node's retention window" >&2
fi

echo
echo "==> Running correctness suite"
TESTNET_RPC_URL="$RPC_URL" \
TESTNET_CONTRACT_ID="$CONTRACT_ID" \
TESTNET_LEDGER_SPAN="$LEDGER_SPAN" \
REQUIRE_TESTNET_CORRECTNESS=1 \
cargo test -p trident-indexer testnet_correctness -- \
  --test-threads=1 --nocapture

echo
echo "PASS: ingest is correct over the verified range."
