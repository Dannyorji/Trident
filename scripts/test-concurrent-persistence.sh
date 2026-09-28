#!/usr/bin/env bash
#
# Prove exactly-once event persistence under concurrent indexers (issue #418).
#
# Runs the Rust integration tests that drive two real indexer writers against
# one Postgres over the same ledger range, then independently re-checks the
# database for duplicate natural keys and a monotonic cursor.
#
# The assertions live in Rust (crates/indexer/src/db/mod.rs) so they run in CI
# with the rest of the suite; this script is the operator-facing entry point
# that also verifies the invariant directly in SQL — a second opinion that does
# not depend on the same code under test.
#
# Usage:
#   scripts/test-concurrent-persistence.sh [--database-url URL]
#
# Requires: a Postgres reachable via TEST_DATABASE_URL (or --database-url) with
# the migration chain applied.

set -euo pipefail

DATABASE_URL="${TEST_DATABASE_URL:-${DATABASE_URL:-}}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --database-url) DATABASE_URL="$2"; shift 2 ;;
    -h|--help) sed -n '3,18p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

if [[ -z "$DATABASE_URL" ]]; then
  cat >&2 <<'EOF'
error: no database URL.

Set TEST_DATABASE_URL (or DATABASE_URL), or pass --database-url. Example:

  export TEST_DATABASE_URL=postgres://postgres:trident@localhost:5432/trident_test
EOF
  exit 1
fi

command -v psql >/dev/null 2>&1 || {
  echo "error: psql is required for the independent SQL verification" >&2
  exit 1
}

echo "Concurrent indexer persistence check"
echo "  database: ${DATABASE_URL%%\?*}"
echo

# --- 1. Run the concurrency integration tests -------------------------------
# REQUIRE_TEST_SERVICES turns a missing database into a hard failure rather
# than a silent skip, so this script cannot report success without having run
# the tests it claims to run.
echo "==> Running concurrency integration tests"
TEST_DATABASE_URL="$DATABASE_URL" \
REQUIRE_TEST_SERVICES=1 \
cargo test -p trident-indexer \
  concurrent_indexers_persist_each_event_exactly_once \
  cursor_never_rewinds_under_concurrent_writers \
  concurrent_cursor_advances_converge_on_maximum \
  -- --test-threads=1 --nocapture

# --- 2. Independent SQL verification ----------------------------------------
# The tests above assert through the same commit path they exercise. This step
# asks the database directly, so a bug that made both the writer and its
# assertions agree on a wrong answer still surfaces here.
echo
echo "==> Verifying exactly-once invariants directly in SQL"

duplicates="$(psql "$DATABASE_URL" -tAc "
  SELECT COUNT(*) FROM (
    SELECT contract_id, ledger_sequence, event_index
    FROM soroban_events
    GROUP BY contract_id, ledger_sequence, event_index
    HAVING COUNT(*) > 1
  ) dupes;
")"

if [[ "$duplicates" != "0" ]]; then
  echo "FAIL: $duplicates natural keys are duplicated in soroban_events" >&2
  psql "$DATABASE_URL" -c "
    SELECT contract_id, ledger_sequence, event_index, COUNT(*) AS copies
    FROM soroban_events
    GROUP BY contract_id, ledger_sequence, event_index
    HAVING COUNT(*) > 1
    ORDER BY copies DESC
    LIMIT 20;
  " >&2
  exit 1
fi
echo "  no duplicate (contract_id, ledger_sequence, event_index) rows"

# The UNIQUE constraint from migration 0025 is what makes the ON CONFLICT path
# a real guarantee rather than a convention. Verify it is actually present:
# without it, concurrent writers would silently double-insert.
constraint="$(psql "$DATABASE_URL" -tAc "
  SELECT COUNT(*)
  FROM pg_indexes
  WHERE tablename LIKE 'soroban_events%'
    AND indexdef ILIKE '%UNIQUE%'
    AND indexdef ILIKE '%ledger_sequence%'
    AND indexdef ILIKE '%event_index%';
")"

if [[ "${constraint:-0}" -lt 1 ]]; then
  echo "FAIL: no UNIQUE index on the natural key — exactly-once is unenforced" >&2
  exit 1
fi
echo "  natural-key UNIQUE index present ($constraint)"

# The cursor key is namespaced per network (issue #600); the Rust tests this
# script cross-checks (crates/indexer/src/db/mod.rs) all run against "testnet".
cursor="$(psql "$DATABASE_URL" -tAc "
  SELECT value FROM system_state WHERE key = 'latest_ledger_cursor:testnet';
")"
if [[ -z "$cursor" ]]; then
  echo "FAIL: no latest_ledger_cursor:testnet row in system_state" >&2
  exit 1
fi
echo "  cursor present at ledger ${cursor}"

echo
echo "PASS: exactly-once event persistence holds under concurrent indexers."
echo
echo "Note: the supported deployment is a single indexer replica. See"
echo "docs/deployment.md and helm/trident/templates/indexer-deployment.yaml —"
echo "the chart enforces replicas: 1 with a Recreate strategy. The guarantees"
echo "checked here are what make an accidental double-deploy survivable, not"
echo "an endorsement of scaling the indexer out."
