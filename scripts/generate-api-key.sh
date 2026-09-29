#!/usr/bin/env bash
# Utility script to generate a cryptographically random 32-byte API key
# and compute its HMAC-SHA256 hash using API_KEY_SALT.
set -euo pipefail

cd "$(dirname "$0")/.."

SALT="${API_KEY_SALT:-${1:-}}"

(cd services/api && go run ./cmd/keygen ${SALT:+-salt "$SALT"})
