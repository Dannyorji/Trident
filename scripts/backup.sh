#!/usr/bin/env bash
# =============================================================================
# Trident — Automated PostgreSQL Backup Script
# =============================================================================
# Generates a compressed, custom-format PostgreSQL dump with SHA-256 checksum.
# Preserves table partitions, indexes, constraints, and sequences.
#
# Usage:
#   ./scripts/backup.sh [output_dir]
#
# Required Environment:
#   DATABASE_URL — Connection string to the source PostgreSQL instance
# =============================================================================

set -euo pipefail

OUTPUT_DIR="${1:-./backups}"
mkdir -p "$OUTPUT_DIR"

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "[-] ERROR: DATABASE_URL environment variable is required." >&2
  exit 1
fi

TIMESTAMP=$(date -u +"%Y%m%d_%H%M%SZ")
BACKUP_FILENAME="trident_db_backup_${TIMESTAMP}.dump"
BACKUP_PATH="${OUTPUT_DIR}/${BACKUP_FILENAME}"

echo "[+] Starting Trident PostgreSQL backup at ${TIMESTAMP}..."
echo "[+] Target output: ${BACKUP_PATH}"

START_TIME=$(date +%s)

# Use custom format (-Fc) with maximum compression (-Z 6)
pg_dump \
  --format=custom \
  --compress=6 \
  --verbose \
  --no-owner \
  --no-privileges \
  --dbname="$DATABASE_URL" \
  --file="$BACKUP_PATH"

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

# Generate SHA-256 Checksum
echo "[+] Computing SHA-256 checksum..."
sha256sum "$BACKUP_PATH" > "${BACKUP_PATH}.sha256"

SIZE=$(du -h "$BACKUP_PATH" | cut -f1)
echo "[+] Backup successfully completed in ${DURATION}s (${SIZE})."
echo "[+] Artifact: ${BACKUP_PATH}"
echo "[+] Checksum: $(cat "${BACKUP_PATH}.sha256")"
