#!/usr/bin/env bash
# =============================================================================
# Trident — Automated PostgreSQL Restore & Drill Script
# =============================================================================
# Verifies SHA-256 checksum and restores a custom-format dump into target DB.
# Validates partition table definitions and sync cursor state.
#
# Usage:
#   ./scripts/restore.sh <backup_file.dump> [target_database_url]
# =============================================================================

set -euo pipefail

BACKUP_FILE="${1:-}"
TARGET_URL="${2:-${TARGET_DATABASE_URL:-${DATABASE_URL:-}}}"

if [[ -z "$BACKUP_FILE" || ! -f "$BACKUP_FILE" ]]; then
  echo "[-] ERROR: Please specify a valid backup .dump file." >&2
  echo "    Usage: ./scripts/restore.sh <backup_file.dump> [target_database_url]" >&2
  exit 1
fi

if [[ -z "$TARGET_URL" ]]; then
  echo "[-] ERROR: TARGET_DATABASE_URL or DATABASE_URL must be provided." >&2
  exit 1
fi

# 1. Verify Checksum if .sha256 file exists
CHECKSUM_FILE="${BACKUP_FILE}.sha256"
if [[ -f "$CHECKSUM_FILE" ]]; then
  echo "[+] Verifying SHA-256 integrity..."
  sha256sum -c "$CHECKSUM_FILE"
  echo "[+] Checksum verification passed."
else
  echo "[!] WARNING: Checksum file not found at ${CHECKSUM_FILE}. Proceeding with restore..."
fi

echo "[+] Initiating restore into target database..."
START_TIME=$(date +%s)

# Restore with clean mode. Deliberately NOT `|| true`: a failed pg_restore
# must abort this script, not fall through to validation queries that would
# either crash on missing tables or silently report zero rows against a
# database that was never actually restored.
pg_restore \
  --clean \
  --if-exists \
  --no-owner \
  --no-privileges \
  --verbose \
  --dbname="$TARGET_URL" \
  "$BACKUP_FILE"

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

echo "[+] Database restore completed in ${DURATION}s."

# 2. Validation Queries
#
# Table names verified against database/migrations/0001_init.sql and
# 0010_token_events.sql: the real tables are indexed_contracts and
# token_events, not "contracts"/"token_transfers" (neither of which exists
# anywhere in the schema) — the previous query crashed here on every run
# before reaching any of the checks below it.
echo "[+] Validating partitioned tables and row counts..."
psql "$TARGET_URL" -t -A -c "
SELECT 'soroban_events count: ' || count(*) FROM soroban_events;
SELECT 'token_events count: ' || count(*) FROM token_events;
SELECT 'indexed_contracts count: ' || count(*) FROM indexed_contracts;
SELECT 'api_keys count: ' || count(*) FROM api_keys;
"

echo "[+] Validating partitions on soroban_events:"
psql "$TARGET_URL" -c "
SELECT inhrelid::regclass AS partition_name
FROM pg_inherits
WHERE inhparent = 'soroban_events'::regclass;
"

echo "[+] Validating latest indexed ledger sequence:"
psql "$TARGET_URL" -c "
SELECT MAX(ledger_sequence) AS latest_synced_ledger FROM soroban_events;
"

echo "[+] ✅ All restore validations passed successfully."
