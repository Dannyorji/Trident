#!/usr/bin/env bash
#
# End-to-end backup/restore drill, run automatically rather than only by
# hand (issue #619).
#
# Verifies scripts/backup.sh and scripts/restore.sh actually work together:
# builds a scratch database from the real migration chain, seeds it with
# known data, backs it up, drops it (simulating total loss), restores into a
# fresh database, and asserts the restored row counts and max ledger
# sequence match what was seeded. A prior version of restore.sh's validation
# step referenced two tables that do not exist anywhere in the schema
# (token_transfers, contracts) and crashed on every real restore before this
# test existed to catch it.
#
# Usage:
#   DATABASE_URL=postgres://user:pass@localhost:5432/trident_backup_test \
#   RESTORE_DATABASE_URL=postgres://user:pass@localhost:5432/trident_restore_test \
#     scripts/test-backup-restore.sh
#
# Requires DATABASE_URL and RESTORE_DATABASE_URL to point at two scratch
# databases this script may drop, recreate, and destroy the contents of.
# Intended for CI; running it against anything you care about will destroy
# data in both.

set -euo pipefail

DB_URL="${DATABASE_URL:-}"
RESTORE_DB_URL="${RESTORE_DATABASE_URL:-}"
MIGRATIONS_DIR="${MIGRATIONS_DIR:-database/migrations}"

if [ -z "$DB_URL" ] || [ -z "$RESTORE_DB_URL" ]; then
  echo "error: both DATABASE_URL and RESTORE_DATABASE_URL must be set to scratch databases" >&2
  exit 2
fi

command -v psql >/dev/null 2>&1 || { echo "error: psql is required" >&2; exit 2; }
command -v pg_dump >/dev/null 2>&1 || { echo "error: pg_dump is required" >&2; exit 2; }
command -v pg_restore >/dev/null 2>&1 || { echo "error: pg_restore is required" >&2; exit 2; }

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

echo "=== Applying migration chain to scratch database ==="
for f in "$MIGRATIONS_DIR"/*.sql; do
  psql -d "$DB_URL" -X -q -v ON_ERROR_STOP=1 -f "$f" >"$WORKDIR/mig.log" 2>&1 || {
    echo "error: migration $f failed" >&2
    cat "$WORKDIR/mig.log" >&2
    exit 1
  }
done

echo "=== Seeding known data ==="
SEED_ROWS=500
psql -d "$DB_URL" -X -q -v ON_ERROR_STOP=1 -c "
INSERT INTO soroban_events (contract_id, ledger_sequence, ledger_timestamp, transaction_hash, event_index, event_type, network, topics, data)
SELECT
  'CA' || lpad(to_hex(i), 55, '0'),
  1000 + i,
  NOW() - (interval '1 second' * i),
  encode(gen_random_bytes(32), 'hex'),
  0,
  'contract',
  'testnet',
  '[\"transfer\"]'::jsonb,
  '{}'::jsonb
FROM generate_series(1, $SEED_ROWS) AS i;
"

EXPECTED_MAX_LEDGER=$((1000 + SEED_ROWS))

echo "=== Backing up ==="
BACKUP_DIR="$WORKDIR/backups"
BACKUP_OUTPUT=$(DATABASE_URL="$DB_URL" bash scripts/backup.sh "$BACKUP_DIR")
BACKUP_FILE=$(find "$BACKUP_DIR" -name '*.dump' | head -1)
if [ -z "$BACKUP_FILE" ]; then
  echo "error: backup.sh did not produce a .dump file" >&2
  echo "$BACKUP_OUTPUT" >&2
  exit 1
fi

echo "=== Simulating total loss: dropping and recreating the restore target ==="
RESTORE_DB_NAME=$(echo "$RESTORE_DB_URL" | sed -E 's#.*/([^/?]+).*#\1#')
RESTORE_ADMIN_URL=$(echo "$RESTORE_DB_URL" | sed -E "s#/${RESTORE_DB_NAME}(\?.*)?\$#/postgres#")
psql -d "$RESTORE_ADMIN_URL" -X -q -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS \"$RESTORE_DB_NAME\";"
psql -d "$RESTORE_ADMIN_URL" -X -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"$RESTORE_DB_NAME\";"

echo "=== Restoring ==="
bash scripts/restore.sh "$BACKUP_FILE" "$RESTORE_DB_URL"

echo "=== Asserting restored data matches what was seeded ==="
RESTORED_COUNT=$(psql -d "$RESTORE_DB_URL" -t -A -c "SELECT count(*) FROM soroban_events;")
RESTORED_MAX_LEDGER=$(psql -d "$RESTORE_DB_URL" -t -A -c "SELECT MAX(ledger_sequence) FROM soroban_events;")

FAILED=0
if [ "$RESTORED_COUNT" != "$SEED_ROWS" ]; then
  echo "FAIL: restored soroban_events count = $RESTORED_COUNT, want $SEED_ROWS" >&2
  FAILED=1
fi
if [ "$RESTORED_MAX_LEDGER" != "$EXPECTED_MAX_LEDGER" ]; then
  echo "FAIL: restored max ledger_sequence = $RESTORED_MAX_LEDGER, want $EXPECTED_MAX_LEDGER" >&2
  FAILED=1
fi

if [ "$FAILED" -ne 0 ]; then
  exit 1
fi

echo "=== PASS: backup/restore drill round-tripped $SEED_ROWS rows correctly ==="
