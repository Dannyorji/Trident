#!/usr/bin/env bash
#
# Unit tests for scripts/check-oncall-readiness.sh (issue #620).
#
# Runs the checker against synthetic fixtures (never the real docs, which
# this script must not fabricate values into) to prove it actually detects
# each known placeholder and actually passes once a fixture is filled in
# with realistic (fixture) values.
#
# Usage:
#   scripts/test-check-oncall-readiness.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECKER="$REPO_ROOT/scripts/check-oncall-readiness.sh"

if [ ! -x "$CHECKER" ]; then
    echo "error: $CHECKER not found or not executable" >&2
    exit 2
fi

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

failures=0

assert_fails_with() {
    local desc="$1" want="$2" incident="$3" routing="$4"
    if out=$(bash "$CHECKER" "$incident" "$routing" 2>&1); then
        echo "FAIL: $desc — expected the checker to report NOT READY, but it passed"
        failures=$((failures + 1))
    elif [[ "$out" == *"$want"* ]]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — failed, but didn't mention '$want': $out"
        failures=$((failures + 1))
    fi
}

assert_passes() {
    local desc="$1" incident="$2" routing="$3"
    if out=$(bash "$CHECKER" "$incident" "$routing" 2>&1); then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — expected the checker to pass, got: $out"
        failures=$((failures + 1))
    fi
}

# --- Fully unexecuted template (mirrors the real docs today) ----------------
cat > "$WORKDIR/incident-unfilled.md" <<'EOF'
## On-call owner — launch week

**Primary on-call:** [FILL IN: name, GitHub handle, mobile number or pager
handle — e.g. `@alice`, +1-555-0100, PagerDuty target `alice-trident`]

**Secondary / escalation:** [FILL IN: name, GitHub handle, mobile number or
pager handle — e.g. `@bob`, +1-555-0101, PagerDuty target `bob-trident`]
EOF

cat > "$WORKDIR/routing-unfilled.md" <<'EOF'
      - service_key: "<PAGERDUTY_SERVICE_INTEGRATION_KEY>"

      - api_url: "<SLACK_WEBHOOK_URL>"

| Step | Tester | Date | Result |
|---|---|---|---|
| 1 — config validation | | | pass / fail |
| 2 — SEV-1 page delivered to primary device | | | pass / fail |
| 3 — warning to notification channel only | | | pass / fail |
| 4 — inhibit rules suppress duplicate alerts | | | pass / fail |
| 5 — escalation to secondary after 15 min | | | pass / fail |
EOF

assert_fails_with "unfilled on-call contact is caught" \
    "on-call contact still unfilled" \
    "$WORKDIR/incident-unfilled.md" "$WORKDIR/routing-unfilled.md"
assert_fails_with "placeholder PagerDuty key is caught" \
    "PagerDuty integration key is still the example placeholder" \
    "$WORKDIR/incident-unfilled.md" "$WORKDIR/routing-unfilled.md"
assert_fails_with "placeholder Slack webhook is caught" \
    "Slack webhook URL is still the example placeholder" \
    "$WORKDIR/incident-unfilled.md" "$WORKDIR/routing-unfilled.md"
assert_fails_with "unexecuted routing test rows are caught" \
    "5 of 5 pre-launch routing test row(s)" \
    "$WORKDIR/incident-unfilled.md" "$WORKDIR/routing-unfilled.md"

# --- Fully filled-in fixture (what a real launch-ready doc looks like) ------
cat > "$WORKDIR/incident-filled.md" <<'EOF'
## On-call owner — launch week

**Primary on-call:** Jane Doe, @jdoe, PagerDuty target `jdoe-trident`

**Secondary / escalation:** John Smith, @jsmith, PagerDuty target `jsmith-trident`
EOF

cat > "$WORKDIR/routing-filled.md" <<'EOF'
      - service_key: "R0ABC123REALKEYNOTAPLACEHOLDER"

      - api_url: "https://hooks.slack.example/services/T00/B00/real"

| Step | Tester | Date | Result |
|---|---|---|---|
| 1 — config validation | Jane Doe | 2026-09-01 | pass |
| 2 — SEV-1 page delivered to primary device | Jane Doe | 2026-09-01 | pass |
| 3 — warning to notification channel only | Jane Doe | 2026-09-01 | pass |
| 4 — inhibit rules suppress duplicate alerts | John Smith | 2026-09-01 | pass |
| 5 — escalation to secondary after 15 min | John Smith | 2026-09-01 | pass |
EOF

assert_passes "fully filled-in fixture passes" \
    "$WORKDIR/incident-filled.md" "$WORKDIR/routing-filled.md"

# --- Partially filled: contacts done, routing test still has one blank row --
cat > "$WORKDIR/routing-partial.md" <<'EOF'
      - service_key: "R0ABC123REALKEYNOTAPLACEHOLDER"

      - api_url: "https://hooks.slack.example/services/T00/B00/real"

| Step | Tester | Date | Result |
|---|---|---|---|
| 1 — config validation | Jane Doe | 2026-09-01 | pass |
| 2 — SEV-1 page delivered to primary device | Jane Doe | 2026-09-01 | pass |
| 3 — warning to notification channel only | Jane Doe | 2026-09-01 | pass |
| 4 — inhibit rules suppress duplicate alerts | John Smith | 2026-09-01 | pass |
| 5 — escalation to secondary after 15 min | | | pass / fail |
EOF

assert_fails_with "one still-unexecuted routing test row is caught even when contacts are filled in" \
    "1 of 5 pre-launch routing test row(s)" \
    "$WORKDIR/incident-filled.md" "$WORKDIR/routing-partial.md"

echo
if [ "$failures" -gt 0 ]; then
    echo "$failures test(s) failed"
    exit 1
fi
echo "All check-oncall-readiness.sh tests passed"
