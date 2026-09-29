#!/usr/bin/env bash
set -euo pipefail

SCRATCH_DB_URL="${SCRATCH_DB_URL:-postgres://trident:password@localhost:5432/trident_scratch}"
BACKUP_FILE="${1:-}"

if [ -z "${BACKUP_FILE}" ]; then
    echo "Usage: $0 <path-to-backup.sql.gz>" >&2
    exit 1
fi

echo "Starting end-to-end restore drill into scratch database..."
START_TIME=$(date +%s)

echo "Recreating scratch database..."
psql "${SCRATCH_DB_URL%.*}/postgres" -c "DROP DATABASE IF EXISTS trident_scratch;"
psql "${SCRATCH_DB_URL%.*}/postgres" -c "CREATE DATABASE trident_scratch;"

echo "Restoring database dump..."
gunzip -c "${BACKUP_FILE}" | pg_restore --dbname="${SCRATCH_DB_URL}" --clean --if-exists --no-owner

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

echo "SUCCESS: Restore drill completed in ${DURATION} seconds (wall-clock time)."
exit 0
