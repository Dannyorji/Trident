#!/usr/bin/env bash
#
# Exercise every `.down.sql` rollback file against a real database (issue #602).
#
# Why this exists
# ----------------
# `lint-migrations.sh` rule 5 only checks that a `.down.sql` file (or a
# `lint:allow-no-rollback` waiver) exists next to every forward migration. It
# never runs the SQL, so a down migration with a typo, a wrong table/column
# name, or a wrong drop order relative to its neighbours would still pass
# lint and only be discovered the first time someone actually needed to roll
# something back, under incident pressure, which is exactly the scenario
# issue #602 exists to prevent. This script closes that gap by applying every
# down migration for real, in the reverse order an operator would use.
#
# What it does
# ------------
# 1. Applies the full forward chain (every `*.sql` that is not `*.down.sql`),
#    same as `check-schema-drift.sh` and CI's own migration step.
# 2. Walks the chain backwards from the highest version down to the lowest,
#    and for every migration that has a `<version>_<name>.down.sql` file,
#    applies it against the live cumulative schema — i.e. it reverts 0033,
#    then 0032, then (0031/0030/... whichever have a down file), etc. This
#    mirrors how `sqlx migrate revert` walks the chain and, for the two
#    inter-dependent soroban_events migrations, means 0026's down (which
#    drops the six restored indexes) runs before 0025's down (which drops
#    the unique constraint from an earlier migration) — the reverse of their
#    forward order, same as revert always is.
# 3. Migrations with no `.down.sql` (a documented `lint:allow-no-rollback`
#    waiver) are skipped: reverting past one deliberately stops the
#    automated chain, same as it would for `sqlx migrate revert` walking
#    into a gap. This script only proves that the down migrations which
#    exist apply cleanly, not that skipping a waived one is itself safe to
#    automate — that is an operational decision for whoever is rolling back.
#
# What this does NOT check
# -------------------------
# It does not assert that the schema after reverting migration N matches the
# schema as it existed right before N was first applied (a byte-for-byte
# before/after diff). That would need a per-step schema snapshot and this
# repo has no existing harness for that; see the discussion on issue #602.
# What it does prove is weaker but still real: every rollback file is valid
# SQL that runs to completion, in the order it would actually be used, against
# a database carrying every later migration's objects — which is exactly the
# condition (constraints, indexes, columns added afterwards) most likely to
# make a hand-written down migration fail in practice.
#
# Usage:
#   DATABASE_URL=postgres://... scripts/check-migration-rollback.sh
#
# Requires DATABASE_URL pointing at a scratch database this script may DROP
# and recreate objects in. Intended for CI; running it against anything you
# care about will destroy data.

set -euo pipefail

DB_URL="${DATABASE_URL:-}"
MIGRATIONS_DIR="${MIGRATIONS_DIR:-database/migrations}"

if [ -z "$DB_URL" ]; then
    echo "error: DATABASE_URL must be set to a scratch database" >&2
    exit 2
fi

command -v psql >/dev/null 2>&1 || {
    echo "error: psql is required" >&2
    exit 2
}

run_sql_file() {
    psql -d "$DB_URL" -X -q -v ON_ERROR_STOP=1 -f "$1"
}

echo "==> Resetting scratch schema"
psql -d "$DB_URL" -X -q -v ON_ERROR_STOP=1 \
    -c "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;" \
    >/dev/null

echo "==> Applying forward migration chain"
forward_files=$(find "$MIGRATIONS_DIR" -maxdepth 1 -name '*.sql' ! -name '*.down.sql' | sort)
for f in $forward_files; do
    echo "  applying $(basename "$f")"
    if ! run_sql_file "$f" >/dev/null 2>&1; then
        echo "error: forward migration $(basename "$f") failed to apply" >&2
        run_sql_file "$f"
        exit 1
    fi
done

echo "==> Reverting every available .down.sql, newest first"
# Reverse-sorted list of forward migration basenames (without .sql), so the
# walk-back order matches how sqlx migrate revert would visit them.
versions_desc=$(
    for f in $forward_files; do
        basename "$f" .sql
    done | sort -rn
)

reverted=0
skipped=0
for stem in $versions_desc; do
    down_file="$MIGRATIONS_DIR/${stem}.down.sql"
    if [ ! -f "$down_file" ]; then
        echo "  skip  $stem (no .down.sql — documented lint:allow-no-rollback waiver)"
        skipped=$((skipped + 1))
        continue
    fi
    echo "  revert $(basename "$down_file")"
    if ! run_sql_file "$down_file" >/dev/null 2>&1; then
        echo "error: rollback $(basename "$down_file") failed to apply" >&2
        run_sql_file "$down_file"
        exit 1
    fi
    reverted=$((reverted + 1))
done

echo
echo "OK: $reverted rollback file(s) applied cleanly, $skipped waived migration(s) skipped."
