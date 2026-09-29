#!/usr/bin/env bash
#
# Enforce the MVP go/no-go launch gate defined in docs/LAUNCH_CHECKLIST.md
# (issue #503).
#
# Why this exists
# ----------------
# docs/LAUNCH_CHECKLIST.md enumerates the blocking set for testnet launch as a
# markdown table, but a markdown table is only ever a statement of intent —
# nothing stopped it from being merged with blank rows, or from silently
# drifting out of sync with reality after someone filled it in once. Without a
# mechanical check, "are we ready" stays a matter of who last read the file
# closely. This script makes the gate objectively checkable: it parses the
# checklist table and fails if any row is unresolved, so "go" requires the
# table to actually say so rather than requiring nobody to notice it doesn't.
#
# What this checks (mirrors the "No-go criteria" section of the checklist)
# --------------------------------------------------------------------------
#   1. Every gate row has a non-blank Pass/Fail column.
#   2. No gate row is marked Fail.
#   3. The rollback rehearsal row (row 9, "Rollback rehearsed") has evidence
#      recorded with a date, and that date is within the last 30 days.
#
# What this deliberately does NOT check
# --------------------------------------
#   - Whether the "Evidence" text is actually true. This script trusts the
#     table; it cannot re-run a soak test or re-verify an alert fired. It only
#     catches the case where the table itself is incomplete or self-reports
#     failure, and the 30-day staleness of the one row (rollback) that already
#     encodes a doc-level expiry.
#   - Whether a P1/P2 incident is currently open (no-go criterion #2 in the
#     checklist). That lives in an incident tracker this script has no access
#     to; it is intentionally left as a manual check at go/no-go time.
#
# Usage
# -----
#   scripts/check-launch-gate.sh [path-to-checklist]
#
# Defaults to docs/LAUNCH_CHECKLIST.md relative to the repo root. Intended to
# be run locally before a launch decision, and is safe to wire into CI
# (workflow_dispatch, same pattern as load-tests.yml) once the checklist is
# actually being kept current — wiring it in while the checklist is still the
# unexecuted template it ships as today would just make every run fail on
# every blank row, which is correct but not yet useful signal.
#
# Exit codes:
#   0 - every gate passes, launch may proceed
#   1 - one or more gates are unresolved or failing (no-go)
#   2 - usage error, missing file, or the table could not be parsed
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHECKLIST="${1:-${REPO_ROOT}/docs/LAUNCH_CHECKLIST.md}"

if [ ! -f "$CHECKLIST" ]; then
  echo "ERROR: checklist not found at $CHECKLIST" >&2
  exit 2
fi

# Extract table rows: lines starting with "| <digit>" are gate rows; the
# header and separator rows start with "| #" and "|---" respectively and are
# excluded by requiring the first cell to be numeric.
# `mapfile` is bash 4+; macOS still ships bash 3.2 and this script is
# documented for local pre-launch runs, so read the rows portably instead.
GATE_ROWS=()
while IFS= read -r _gate_row; do
  GATE_ROWS+=("$_gate_row")
done < <(command grep -E '^\| *[0-9]+ *\|' "$CHECKLIST" || true)

if [ "${#GATE_ROWS[@]}" -eq 0 ]; then
  echo "ERROR: no gate rows found in $CHECKLIST — table format may have changed" >&2
  echo "Expected rows of the form: | # | Gate | Pass/Fail | Evidence | Signed off by |" >&2
  exit 2
fi

echo "=== Trident launch gate check ==="
echo "Checklist: $CHECKLIST"
echo "Gate rows found: ${#GATE_ROWS[@]}"
echo ""

FAILURES=0
ROLLBACK_ROW=""
ROLLBACK_ROW_FOUND=0

# Split a markdown table row into its cells (fields between the pipes),
# trimming surrounding whitespace from each.
split_row() {
  local row="$1"
  # Strip leading/trailing pipe, then split on remaining pipes.
  row="${row#|}"
  row="${row%|}"
  # A markdown-escaped `\|` is literal cell text, not a column separator.
  # Splitting on it would shift every later column by one and misreport a
  # valid row (e.g. a gate description containing a pipe). Swap escaped pipes
  # for a sentinel before splitting, then restore them per cell.
  local sentinel=$'\001'
  row="${row//\\|/$sentinel}"
  IFS='|' read -r -a CELLS <<< "$row"
  local i
  for i in "${!CELLS[@]}"; do
    # Trim leading/trailing whitespace from each cell, then restore pipes.
    CELLS[i]="$(printf '%s' "${CELLS[i]}" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    CELLS[i]="${CELLS[i]//$sentinel/|}"
  done
}

for row in "${GATE_ROWS[@]}"; do
  split_row "$row"
  num="${CELLS[0]:-}"
  gate="${CELLS[1]:-}"
  status="${CELLS[2]:-}"
  evidence="${CELLS[3]:-}"
  signoff="${CELLS[4]:-}"

  label="Row ${num}: ${gate}"

  # Track the rollback rehearsal row (matched by name, not a hardcoded row
  # number, so reordering the table doesn't silently stop checking staleness).
  # Done before the status checks below because those `continue` on a blank
  # Pass/Fail cell — which is exactly the state of an unexecuted checklist,
  # and would otherwise report the row as missing entirely.
  if [[ "$gate" == *"Rollback rehearsed"* ]]; then
    ROLLBACK_ROW_FOUND=1
    ROLLBACK_ROW="$evidence"
  fi

  if [ -z "$status" ]; then
    echo "NO-GO: ${label} — Pass/Fail column is blank"
    FAILURES=$((FAILURES + 1))
    continue
  fi

  case "$status" in
    Pass|PASS|pass)
      if [ -z "$evidence" ] || [ -z "$signoff" ]; then
        echo "NO-GO: ${label} — marked Pass but missing Evidence or Signed off by"
        FAILURES=$((FAILURES + 1))
      else
        echo "GO:    ${label} — Pass (${signoff})"
      fi
      ;;
    Fail|FAIL|fail)
      echo "NO-GO: ${label} — marked Fail"
      FAILURES=$((FAILURES + 1))
      ;;
    *)
      echo "NO-GO: ${label} — unrecognized Pass/Fail value '${status}'"
      FAILURES=$((FAILURES + 1))
      ;;
  esac

done

echo ""
echo "=== Rollback rehearsal staleness ==="
if [ "$ROLLBACK_ROW_FOUND" -eq 0 ]; then
  # Distinguished from "row present but blank": the staleness check binds to
  # the table by the gate's name, so a renamed row would otherwise be reported
  # as missing evidence and send someone hunting for the wrong problem.
  echo "NO-GO: no gate row matching 'Rollback rehearsed' found — if it was renamed, update the match in this script"
  FAILURES=$((FAILURES + 1))
elif [ -z "$ROLLBACK_ROW" ]; then
  echo "NO-GO: rollback rehearsal row has no evidence recorded (cannot verify it happened within 30 days)"
  FAILURES=$((FAILURES + 1))
else
  # Look for an ISO date (YYYY-MM-DD) anywhere in the evidence cell.
  if [[ "$ROLLBACK_ROW" =~ ([0-9]{4}-[0-9]{2}-[0-9]{2}) ]]; then
    REHEARSAL_DATE="${BASH_REMATCH[1]}"
    REHEARSAL_EPOCH=$(date -j -f "%Y-%m-%d" "$REHEARSAL_DATE" +%s 2>/dev/null \
      || date -d "$REHEARSAL_DATE" +%s 2>/dev/null \
      || echo "")
    NOW_EPOCH=$(date +%s)
    if [ -z "$REHEARSAL_EPOCH" ]; then
      echo "NO-GO: could not parse rollback rehearsal date '${REHEARSAL_DATE}'"
      FAILURES=$((FAILURES + 1))
    else
      AGE_DAYS=$(( (NOW_EPOCH - REHEARSAL_EPOCH) / 86400 ))
      if [ "$AGE_DAYS" -gt 30 ]; then
        echo "NO-GO: rollback rehearsal (${REHEARSAL_DATE}) is ${AGE_DAYS} days old, exceeds the 30-day limit"
        FAILURES=$((FAILURES + 1))
      elif [ "$AGE_DAYS" -lt 0 ]; then
        echo "NO-GO: rollback rehearsal date (${REHEARSAL_DATE}) is in the future — check for a typo"
        FAILURES=$((FAILURES + 1))
      else
        echo "GO:    rollback rehearsal (${REHEARSAL_DATE}) is ${AGE_DAYS} days old, within the 30-day limit"
      fi
    fi
  else
    echo "NO-GO: rollback rehearsal evidence has no parseable date (YYYY-MM-DD): '${ROLLBACK_ROW}'"
    FAILURES=$((FAILURES + 1))
  fi
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
  echo "RESULT: GO — all ${#GATE_ROWS[@]} gates pass and the rollback rehearsal is current."
  exit 0
else
  echo "RESULT: NO-GO — ${FAILURES} gate(s) failed or unresolved. See above." >&2
  echo "Remember: a P1/P2 incident open on a launch-critical path is also a" >&2
  echo "no-go per docs/LAUNCH_CHECKLIST.md, and is not checked by this script." >&2
  exit 1
fi
