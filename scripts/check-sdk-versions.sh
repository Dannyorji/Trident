#!/usr/bin/env bash
#
# Verify all five SDKs declare the same version, and that it matches the
# OpenAPI spec they are generated from (issue #429).
#
# Why this matters
# ----------------
# The SDKs are generated from api/openapi.yaml. If sdk/python ships 0.3.0 built
# from spec 1.1.0 while sdk/typescript ships 0.2.0 built from spec 1.0.0, a user
# has no way to tell which client speaks which API — the SDK version says
# nothing about the contract it implements. Spec churn has already forced
# regeneration twice (8fb139c, 7353e85).
#
# The rule enforced here: every SDK carries the spec's version, exactly. A
# breaking spec change bumps all five together, and a user reading
# `trident-sdk 1.2.0` knows it implements OpenAPI 1.2.0.
#
# Usage:
#   scripts/check-sdk-versions.sh          # verify
#   scripts/check-sdk-versions.sh --fix    # rewrite every SDK to the spec version

set -euo pipefail

FIX=0
[ "${1:-}" = "--fix" ] && FIX=1

spec_version="$(
    sed -n '/^info:/,/^[a-z]/p' api/openapi.yaml \
        | sed -n 's/^[[:space:]]*version:[[:space:]]*["'"'"']\{0,1\}\([0-9][^"'"'"' ]*\).*/\1/p' \
        | head -1
)"

if [ -z "$spec_version" ]; then
    echo "error: could not read info.version from api/openapi.yaml" >&2
    exit 2
fi

echo "OpenAPI spec version: $spec_version"
echo

failures=0

report() {
    local name="$1" actual="$2"
    if [ "$actual" = "$spec_version" ]; then
        printf '  %-12s %-10s ok\n' "$name" "$actual"
    else
        printf '  %-12s %-10s MISMATCH (expected %s)\n' "$name" "${actual:-<unset>}" "$spec_version"
        failures=$((failures + 1))
    fi
}

# --- TypeScript -------------------------------------------------------------
ts_version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' sdk/typescript/package.json | head -1)"
report "typescript" "$ts_version"

# --- React ------------------------------------------------------------------
react_version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' sdk/react/package.json | head -1)"
report "react" "$react_version"

# --- Python -----------------------------------------------------------------
py_version="$(sed -n 's/^version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' sdk/python/pyproject.toml | head -1)"
report "python" "$py_version"

# --- Rust -------------------------------------------------------------------
rust_version="$(sed -n '/^\[package\]/,/^\[/p' sdk/rust/Cargo.toml | sed -n 's/^version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
report "rust" "$rust_version"

# --- Go ---------------------------------------------------------------------
# Go modules carry no version field: the version is the git tag
# (sdk/go/vX.Y.Z), which the module proxy resolves. A VERSION file keeps the
# intended version visible in-tree and checkable here; the release process must
# tag to match.
go_version=""
[ -f sdk/go/VERSION ] && go_version="$(tr -d '[:space:]' < sdk/go/VERSION)"
report "go" "$go_version"

echo

if [ "$FIX" = "1" ]; then
    echo "Rewriting all SDKs to $spec_version"
    # npm version is authoritative for the two package.json files: it also
    # updates package-lock.json's own version field, which a sed would miss.
    (cd sdk/typescript && npm version "$spec_version" --no-git-tag-version --allow-same-version >/dev/null)
    (cd sdk/react      && npm version "$spec_version" --no-git-tag-version --allow-same-version >/dev/null)
    sed -i "0,/^version = \".*\"/s//version = \"$spec_version\"/" sdk/python/pyproject.toml
    sed -i "0,/^version = \".*\"/s//version = \"$spec_version\"/" sdk/rust/Cargo.toml
    printf '%s\n' "$spec_version" > sdk/go/VERSION
    echo "done — re-run without --fix to verify"
    exit 0
fi

if [ "$failures" -gt 0 ]; then
    cat >&2 <<EOF
FAIL: $failures SDK version(s) do not match the spec.

Every SDK must declare the version of the OpenAPI spec it was generated from,
so a published client's version identifies the API contract it implements.

Fix with:
  scripts/check-sdk-versions.sh --fix
EOF
    exit 1
fi

echo "OK: all five SDKs match the spec version."
