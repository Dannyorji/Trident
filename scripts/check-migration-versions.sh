#!/usr/bin/env bash
#
# Fails if two migrations share a version prefix (issue #371).
#
# sqlx derives a migration's version from the leading digits of its filename
# and stores it as the primary key of _sqlx_migrations. Two files with the
# same prefix therefore collide on insert, and `sqlx migrate run` aborts
# part-way through — leaving the database on whichever of the two happened to
# apply first. This is easy to introduce by accident: contributors branch from
# an older dev and pick "the next number", which someone else has also picked.
#
# CI's integration job applies migrations with a `for f in *.sql` psql loop
# rather than the migrator, so it does NOT catch this. Hence the explicit check.
#
# `.down.sql` rollback files (issue #602) intentionally share a version prefix
# with their forward migration (e.g. 0002_x.sql / 0002_x.down.sql) — that pairing
# is the point of the naming convention, not a collision. They are excluded from
# every check and count below.

set -euo pipefail

MIGRATIONS_DIR="${1:-database/migrations}"

if [ ! -d "$MIGRATIONS_DIR" ]; then
    echo "No migrations directory at $MIGRATIONS_DIR" >&2
    exit 1
fi

duplicates=$(
    find "$MIGRATIONS_DIR" -maxdepth 1 -name '*.sql' ! -name '*.down.sql' -printf '%f\n' \
        | sed -n 's/^\([0-9]\{1,\}\)_.*/\1/p' \
        | sort \
        | uniq -d
)

if [ -n "$duplicates" ]; then
    echo "Duplicate migration versions found in $MIGRATIONS_DIR:"
    echo
    while IFS= read -r version; do
        [ -z "$version" ] && continue
        echo "  version $version is used by:"
        find "$MIGRATIONS_DIR" -maxdepth 1 -name "${version}_*.sql" ! -name '*.down.sql' -printf '    %f\n' | sort
    done <<< "$duplicates"
    echo
    echo "sqlx keys _sqlx_migrations by this prefix, so these collide on the"
    echo "primary key and 'sqlx migrate run' fails against a real database."
    echo
    echo "Renumber the newer file to the next unused version. If it may already"
    echo "have been applied somewhere under the old number, keep it forward-only:"
    echo "guard every statement with IF NOT EXISTS so re-applying is a no-op."
    exit 1
fi

count=$(find "$MIGRATIONS_DIR" -maxdepth 1 -name '*.sql' ! -name '*.down.sql' | wc -l)
echo "OK: $count migrations, no duplicate versions."
