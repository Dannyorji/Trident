#!/usr/bin/env bash
#
# Lint the migration chain for the failure modes that have actually bitten this
# repo (issues #436, #246).
#
# Rules, each traceable to a real incident or a concrete production risk:
#
#   1. Sequential numbering with no gaps or duplicates. sqlx keys
#      _sqlx_migrations by the numeric prefix, so a duplicate aborts a run
#      part-way. (Duplicates are also checked by check-migration-versions.sh,
#      which predates this script; the gap check is new.)
#
#   2. Unguarded destructive statements. Migration 0017 ran a bare
#      `DROP TABLE soroban_events_legacy`, which cascaded away six indexes that
#      earlier `CREATE INDEX IF NOT EXISTS` statements had silently failed to
#      recreate — the #437 bug. A DROP must either carry IF EXISTS or an
#      explicit `-- lint:allow-destructive <reason>` waiver, so removing data
#      is always a decision someone wrote down.
#
#   3. Missing idempotency guards on CREATE. Re-running a partially-applied
#      migration must not fail on an object that already exists.
#
#   4. Long-lock patterns on large tables. `CREATE INDEX` without CONCURRENTLY
#      takes an ACCESS EXCLUSIVE-adjacent lock that blocks writes for the whole
#      build; on soroban_events at production size that is an outage. Likewise
#      `ALTER TABLE ... ADD COLUMN ... NOT NULL` without a DEFAULT rewrites the
#      table. Both require a waiver comment naming why it is safe here.
#
# Usage:
#   scripts/lint-migrations.sh [migrations-dir]
#
# Waivers: put `-- lint:allow-<rule> <reason>` on the line immediately above
# the statement, or anywhere in the file for a file-wide waiver. Rules are
# `destructive`, `no-guard`, and `long-lock`.

set -euo pipefail

MIGRATIONS_DIR="${1:-database/migrations}"

if [ ! -d "$MIGRATIONS_DIR" ]; then
    echo "No migrations directory at $MIGRATIONS_DIR" >&2
    exit 2
fi

failures=0

fail() {
    printf '  %s\n' "$1"
    failures=$((failures + 1))
}

# --- Rule 1: sequential numbering ------------------------------------------
echo "==> Checking migration numbering"

versions=$(
    find "$MIGRATIONS_DIR" -maxdepth 1 -name '*.sql' -printf '%f\n' \
        | sed -n 's/^\([0-9]\{1,\}\)_.*/\1/p' \
        | sort -n
)

if [ -z "$versions" ]; then
    echo "  no numbered migrations found in $MIGRATIONS_DIR" >&2
    exit 2
fi

duplicates=$(echo "$versions" | uniq -d)
if [ -n "$duplicates" ]; then
    while IFS= read -r v; do
        [ -z "$v" ] && continue
        fail "duplicate version $v:"
        find "$MIGRATIONS_DIR" -maxdepth 1 -name "${v}_*.sql" -printf '    %f\n' | sort
    done <<< "$duplicates"
fi

# Gaps: sqlx tolerates them, but a gap almost always means a migration was
# dropped from a branch during a rebase and the chain no longer reproduces
# what shipped.
prev=""
while IFS= read -r v; do
    [ -z "$v" ] && continue
    n=$((10#$v))
    if [ -n "$prev" ] && [ "$n" -ne "$((prev + 1))" ] && [ "$n" -ne "$prev" ]; then
        fail "gap in numbering: $(printf '%04d' "$prev") -> $(printf '%04d' "$n")"
    fi
    prev="$n"
done <<< "$(echo "$versions" | uniq)"

# --- Per-file content rules -------------------------------------------------
echo "==> Checking migration contents"

# True when the file grants `-- lint:allow-<rule>` anywhere, or on the line
# immediately preceding $lineno.
has_waiver() {
    local file="$1" rule="$2" lineno="$3"
    if grep -qiE -- "--[[:space:]]*lint:allow-${rule}\b" "$file"; then
        return 0
    fi
    if [ "$lineno" -gt 1 ] \
        && sed -n "$((lineno - 1))p" "$file" \
        | grep -qiE -- "--[[:space:]]*lint:allow-${rule}\b"; then
        return 0
    fi
    return 1
}

# Strip comments and string literals so a rule never fires on prose. Keeps line
# numbering intact by blanking rather than deleting.
strip_noise() {
    sed -e "s/--.*$//" -e "s/'[^']*'/''/g" "$1"
}

for file in $(find "$MIGRATIONS_DIR" -maxdepth 1 -name '*.sql' | sort); do
    name=$(basename "$file")
    stripped=$(strip_noise "$file")

    # Rule 2: destructive statements without IF EXISTS.
    while IFS=: read -r lineno text; do
        [ -z "$lineno" ] && continue
        if ! echo "$text" | grep -qiE '\bIF[[:space:]]+EXISTS\b' \
            && ! has_waiver "$file" "destructive" "$lineno"; then
            fail "$name:$lineno destructive statement without IF EXISTS or a waiver:"
            fail "    $(echo "$text" | sed 's/^[[:space:]]*//' | cut -c1-90)"
        fi
    done <<< "$(echo "$stripped" | grep -inE '\b(DROP[[:space:]]+(TABLE|COLUMN|INDEX|CONSTRAINT|TYPE|VIEW|SCHEMA)|TRUNCATE)\b' || true)"

    # Rule 3: CREATE without an idempotency guard.
    while IFS=: read -r lineno text; do
        [ -z "$lineno" ] && continue
        if ! echo "$text" | grep -qiE '\bIF[[:space:]]+NOT[[:space:]]+EXISTS\b' \
            && ! echo "$text" | grep -qiE '\bCREATE[[:space:]]+OR[[:space:]]+REPLACE\b' \
            && ! has_waiver "$file" "no-guard" "$lineno"; then
            fail "$name:$lineno CREATE without IF NOT EXISTS or a waiver:"
            fail "    $(echo "$text" | sed 's/^[[:space:]]*//' | cut -c1-90)"
        fi
    done <<< "$(echo "$stripped" | grep -inE '\bCREATE[[:space:]]+(UNIQUE[[:space:]]+)?(TABLE|INDEX|TYPE|VIEW|SCHEMA)\b' || true)"

    # Rule 4a: non-CONCURRENT index builds hold a write-blocking lock.
    #
    # Only enforced for tables large enough to matter. A CREATE INDEX on a
    # small lookup table finishes instantly and does not need the ceremony
    # (CONCURRENTLY cannot run inside a transaction, so demanding it
    # everywhere would be actively worse).
    #
    # This repo's own style writes the table name on the line after
    # `CREATE INDEX ... ON`, not on the same line (see any migration in
    # database/migrations). Matching the whole statement up to its `;`
    # instead of a single line means this rule fires on that style too
    # (#642) — a same-line-only match never matched a real migration here.
    #
    # Migrations 0001-0025 predate this script (added in c263d4e, after they
    # were already applied to real databases) and were never checked against
    # this corrected match. Editing their SQL to add a waiver comment would
    # change the file bytes sqlx checksums against `_sqlx_migrations`, which
    # breaks `migrate run` on any database where they are already applied —
    # a worse outcome than the lock these builds already took once, long ago.
    # Grandfathered here; the rule is fully enforced from 0026 forward, which
    # is the exact migration #642 exists to catch.
    version=$(echo "$name" | sed -n 's/^\([0-9]\{1,\}\)_.*/\1/p')
    grandfathered=false
    if [ -n "$version" ] && [ "$((10#$version))" -le 25 ]; then
        grandfathered=true
    fi
    while IFS=$'\x01' read -r lineno text; do
        [ -z "$lineno" ] && continue
        if [ "$grandfathered" = false ] \
            && echo "$text" | grep -qiE '\b(soroban_events|token_events|audit_log|event_outbox|contract_invocation_metrics)\b' \
            && ! echo "$text" | grep -qiE '\bCONCURRENTLY\b' \
            && ! has_waiver "$file" "long-lock" "$lineno"; then
            fail "$name:$lineno index build on a large table without CONCURRENTLY or a waiver:"
            fail "    $(echo "$text" | sed 's/^[[:space:]]*//' | cut -c1-90)"
        fi
    done <<< "$(awk '
        BEGIN { stmt = ""; startline = 0 }
        /CREATE[ \t]+(UNIQUE[ \t]+)?INDEX/ && startline == 0 { startline = NR }
        startline > 0 {
            stmt = stmt " " $0
            if ($0 ~ /;/) {
                print startline "\x01" stmt
                stmt = ""
                startline = 0
            }
        }
    ' <<< "$stripped")"

    # Rule 4b: ADD COLUMN NOT NULL without DEFAULT rewrites the whole table.
    while IFS=: read -r lineno text; do
        [ -z "$lineno" ] && continue
        if echo "$text" | grep -qiE '\bNOT[[:space:]]+NULL\b' \
            && ! echo "$text" | grep -qiE '\bDEFAULT\b' \
            && ! has_waiver "$file" "long-lock" "$lineno"; then
            fail "$name:$lineno ADD COLUMN NOT NULL without DEFAULT rewrites the table:"
            fail "    $(echo "$text" | sed 's/^[[:space:]]*//' | cut -c1-90)"
        fi
    done <<< "$(echo "$stripped" | grep -inE '\bADD[[:space:]]+COLUMN\b' || true)"
done

echo
if [ "$failures" -gt 0 ]; then
    cat >&2 <<EOF
FAIL: $failures migration lint issue(s).

Each finding is either a real risk or needs an explicit waiver. To waive one,
put a comment on the line above the statement naming the rule and the reason:

  -- lint:allow-destructive the legacy table is empty by this point (see step 4)
  DROP TABLE soroban_events_legacy;

Rules: destructive, no-guard, long-lock.
EOF
    exit 1
fi

echo "OK: $(echo "$versions" | uniq | wc -l | tr -d ' ') migrations pass lint."
