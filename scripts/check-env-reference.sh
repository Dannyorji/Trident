#!/usr/bin/env bash
# Fails if the indexer, backfill, gRPC API, or Go API read an environment
# variable that isn't documented in docs/ENVIRONMENT.md (issue #312).
#
# This is a grep-based check, not a compiler plugin: it scans for
# std::env::var("X") / env::var("X") in Rust and os.Getenv("X") in Go, then
# checks each hit appears (as a whole word, wrapped in backticks) in
# docs/ENVIRONMENT.md. False positives are possible on ALL_CAPS string
# literals that aren't actually env var names — those are allow-listed
# below, not silently skipped.
set -euo pipefail

cd "$(dirname "$0")/.."

DOC="docs/ENVIRONMENT.md"

# Non-env-var ALL_CAPS identifiers that legitimately show up in
# env::var("...")-shaped test/example strings but are not real env vars, or
# are test-only fixtures already covered by inline comments rather than a
# reference table row. Keep this list small and justified.
ALLOWLIST=(
  TEST_POOL_UNSET
  TEST_POOL_VALID
  TEST_POOL_BAD
)

is_allowlisted() {
  local var="$1"
  for a in "${ALLOWLIST[@]}"; do
    [[ "$var" == "$a" ]] && return 0
  done
  return 1
}

# Collect candidate env var names: Rust (std::env::var / env::var) and
# Go (os.Getenv), from application source only (exclude tests, since a test
# fixture string can look like a var but not be one that a lint doc needs to
# cover — real vars read by non-test code are always also referenced from a
# collect_required/envInt/etc call reachable from main, which this still catches).
mapfile -t rust_vars < <(
  grep -rhoE '(std::)?env::var\("[A-Z][A-Z0-9_]*"\)' \
    crates/indexer/src crates/backfill/src crates/api/src crates/common/src 2>/dev/null \
    | grep -oE '"[A-Z][A-Z0-9_]*"' | tr -d '"' | sort -u
)

mapfile -t go_vars < <(
  grep -rhoE 'os\.Getenv\("[A-Z][A-Z0-9_]*"\)' services/api 2>/dev/null \
    | grep -oE '"[A-Z][A-Z0-9_]*"' | tr -d '"' | sort -u
)

all_vars=("${rust_vars[@]}" "${go_vars[@]}")

missing=()
for var in "${all_vars[@]}"; do
  is_allowlisted "$var" && continue
  if ! grep -qF "\`$var\`" "$DOC"; then
    missing+=("$var")
  fi
done

if [ "${#missing[@]}" -gt 0 ]; then
  echo "The following env vars are read by the code but missing from $DOC:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  echo "" >&2
  echo "Add a row for each (service, default, required/optional, description) to $DOC." >&2
  exit 1
fi

# ── Effective-behaviour check (issue #737) ────────────────────────────────
# Documenting a knob is not enough: it must actually change behaviour. Every
# field on `Config` originates in an env var, and a field read only inside
# config.rs (parsed, then logged) was dead — that is exactly how
# MAX_REORG_REWIND_DEPTH, RPC_FAILOVER_THRESHOLD and RPC_ENDPOINT_COOLDOWN_MS
# shipped: validated, logged, and never read by the running indexer. Require
# every field to be *read* (`.<field>`) somewhere outside config.rs.
#
# `.field` rather than a bare name on purpose: struct literals (including the
# test fixtures that populate every field) would otherwise make a dead field
# look used.
config_fields=$(awk '/^pub struct Config \{/{inside=1; next} inside && /^\}/{exit} inside' \
  crates/indexer/src/config.rs \
  | grep -oE '^    pub [a-z_0-9]+:' | sed -E 's/^    pub //; s/:$//')

if [ -z "$config_fields" ]; then
  echo "check-env-reference: could not read Config fields from crates/indexer/src/config.rs" >&2
  exit 1
fi

dead_fields=()
for field in $config_fields; do
  if ! grep -rqE "\.${field}\b" crates/indexer/src --include='*.rs' \
    --exclude='config.rs' 2>/dev/null; then
    dead_fields+=("$field")
  fi
done

if [ "${#dead_fields[@]}" -gt 0 ]; then
  echo "These Config fields (env-var knobs) are never read outside config.rs," >&2
  echo "so setting them changes nothing at runtime:" >&2
  printf '  %s\n' "${dead_fields[@]}" >&2
  echo "" >&2
  echo "Wire the knob into the running indexer, or remove the field and fail" >&2
  echo "startup if the env var is set (see issue #737)." >&2
  exit 1
fi

echo "OK: every env var read by indexer/backfill/grpc-api/go-api is documented in $DOC"
echo "OK: every Config field is read outside config.rs (no dead knobs)"
