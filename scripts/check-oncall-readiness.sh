#!/usr/bin/env bash
#
# Mechanically check whether the on-call/alert-routing documents still carry
# their unexecuted-template placeholders (issue #620).
#
# Why this exists
# ----------------
# docs/runbooks/incident-response.md and docs/runbooks/alert-routing.md read
# as finished runbooks, but as of this script's introduction every one of
# them is still a template: no on-call contact is named, the PagerDuty/Slack
# integration keys are the literal example placeholders, and the five-row
# pre-launch routing test table (alert-routing.md) has never been run — no
# Tester, Date, or Result recorded on any row. alert-routing.md's own text
# states routing test table must show `pass` on all five rows before launch,
# and docs/LAUNCH_CHECKLIST.md row 1 and row 8 already gate launch generically
# on any blank Pass/Fail cell — but nothing made the underlying documents
# themselves objectively checkable the way scripts/check-launch-gate.sh does
# for the checklist table. This closes that gap the same way
# scripts/check-runbook-urls.sh did for alert runbook_url drift: a
# placeholder sitting unfilled is now a mechanically detectable fact, not
# something that depends on a reviewer reading the whole document closely.
#
# What this deliberately does NOT do
# -----------------------------------
# It cannot name a real on-call rotation, provision a real PagerDuty/Slack
# integration key, or fire a real test alert at a real device — all of that
# requires actual infrastructure access and a real team, which is exactly
# the same limitation docs/LAUNCH_CHECKLIST.md's own header already
# documents for the rest of the launch gate ("real work that needs to happen
# with actual infrastructure access and team availability, which this pass
# doesn't have"). Filling in real values and actually running the test is
# what turns this from a failing check into a passing one; this script only
# makes the current "not yet done" state loud instead of silent.
#
# Usage:
#   scripts/check-oncall-readiness.sh
#   scripts/check-oncall-readiness.sh <incident-response.md> <alert-routing.md>
#
# The two-argument form points the check at alternate files — used by
# scripts/test-check-oncall-readiness.sh to exercise both the "still a
# template" and "actually filled in" cases against synthetic fixtures
# without touching the real docs (which this script must never fabricate
# values into).
#
# Exit codes:
#   0 - no known placeholder found (either genuinely filled in, or the
#       placeholder text itself was edited without being replaced with a
#       real value, in which case this check has gone blind and should be
#       re-examined)
#   1 - at least one known placeholder is still present
#   2 - usage error / expected file missing

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

INCIDENT_RESPONSE="${1:-docs/runbooks/incident-response.md}"
ALERT_ROUTING="${2:-docs/runbooks/alert-routing.md}"

for f in "$INCIDENT_RESPONSE" "$ALERT_ROUTING"; do
    if [ ! -f "$f" ]; then
        echo "error: $f not found" >&2
        exit 2
    fi
done

failures=0

check_pattern() {
    local file="$1" pattern="$2" desc="$3"
    if grep -qF "$pattern" "$file"; then
        echo "NOT READY: $file — $desc"
        failures=$((failures + 1))
    fi
}

check_pattern "$INCIDENT_RESPONSE" "[FILL IN: name, GitHub handle, mobile number or pager" \
    "primary/secondary on-call contact still unfilled"

check_pattern "$ALERT_ROUTING" '<PAGERDUTY_SERVICE_INTEGRATION_KEY>' \
    "PagerDuty integration key is still the example placeholder"

check_pattern "$ALERT_ROUTING" '<SLACK_WEBHOOK_URL>' \
    "Slack webhook URL is still the example placeholder"

# Pre-launch routing test table (5 rows): every row must have Tester, Date
# and Result all filled in with a real (non-"pass / fail" literal) result.
# The unexecuted template's rows look like "| 1 — config validation | | | pass / fail |"
# — three blank cells and the literal instructional text as the fourth.
table_start=$(grep -n '^| Step | Tester | Date | Result |$' "$ALERT_ROUTING" | head -1 | cut -d: -f1 || true)
if [ -z "$table_start" ]; then
    echo "error: could not find the pre-launch routing test table header in $ALERT_ROUTING — table format may have changed, update this script's match" >&2
    exit 2
fi

unexecuted_rows=0
# The 5 data rows immediately follow the header and its "|---|---|---|---|" separator.
while IFS= read -r row; do
    if [[ "$row" == *"pass / fail"* ]] || [[ "$row" =~ ^\|[[:space:]]*[0-9]+.*\|[[:space:]]*\|[[:space:]]*\|[[:space:]]*\|[[:space:]]*$ ]]; then
        unexecuted_rows=$((unexecuted_rows + 1))
    fi
done < <(tail -n +$((table_start + 2)) "$ALERT_ROUTING" | head -5)

if [ "$unexecuted_rows" -gt 0 ]; then
    echo "NOT READY: $ALERT_ROUTING — $unexecuted_rows of 5 pre-launch routing test row(s) have no recorded Tester/Date/Result"
    failures=$((failures + 1))
fi

echo ""
if [ "$failures" -gt 0 ]; then
    echo "RESULT: on-call/alert-routing documentation is not launch-ready ($failures gap(s) found)."
    echo "This mirrors docs/LAUNCH_CHECKLIST.md rows 1 and 8, which already block launch on this."
    exit 1
fi

echo "RESULT: no known on-call/alert-routing placeholders found."
exit 0
