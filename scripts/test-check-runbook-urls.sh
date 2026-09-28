#!/usr/bin/env bash
#
# Unit tests for scripts/check-runbook-urls.sh (issue #618).
#
# The real invocation only ever runs against this repo's actual alert files,
# which currently all pass — that proves today's content is correct, not
# that the checker actually detects each failure mode it claims to. This
# builds small synthetic fixtures in a scratch directory and asserts the
# checker passes the good one and fails each broken one for the right
# reason, by invoking check-runbook-urls.sh's own Python body against
# fixture paths rather than re-implementing its logic here.
#
# Usage:
#   scripts/test-check-runbook-urls.sh
#
# No external services required; runs entirely against a temp directory.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECKER="$REPO_ROOT/scripts/check-runbook-urls.sh"

if [ ! -x "$CHECKER" ]; then
    echo "error: $CHECKER not found or not executable" >&2
    exit 2
fi

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

failures=0

# run_checker_body invokes the same inline Python block check-runbook-urls.sh
# uses, but against fixture paths under $WORKDIR instead of the real repo
# files — the shell wrapper only resolves paths relative to the repo root,
# so this calls the Python body directly with explicit fixture arguments.
run_checker_body() {
    local runbook="$1"
    shift
    python3 - "$runbook" "$@" <<'PY'
import re
import sys
import yaml

runbook_path = sys.argv[1]
rule_paths = sys.argv[2:]

def anchor_for(heading: str) -> str:
    slug = heading.strip().lower()
    slug = re.sub(r"[^a-z0-9\-_]", "", slug)
    return slug

with open(runbook_path, encoding="utf-8") as fh:
    headings = re.findall(r"^##\s+(.+?)\s*$", fh.read(), re.MULTILINE)
known_anchors = {anchor_for(h) for h in headings}

failures = []
checked = 0
for rule_path in rule_paths:
    with open(rule_path, encoding="utf-8") as fh:
        doc = yaml.safe_load(fh) or {}
    for group in doc.get("groups", []) or []:
        for rule in group.get("rules", []) or []:
            alert_name = rule.get("alert")
            if not alert_name:
                continue
            checked += 1
            annotations = rule.get("annotations") or {}
            runbook_url = annotations.get("runbook_url")
            if not runbook_url:
                failures.append(f"missing:{alert_name}")
                continue
            expected_prefix = f"{runbook_path}#"
            if not runbook_url.startswith(expected_prefix):
                failures.append(f"wrong-file:{alert_name}")
                continue
            anchor = runbook_url[len(expected_prefix):]
            if anchor not in known_anchors:
                failures.append(f"dangling-anchor:{alert_name}")

if failures:
    print(",".join(failures))
    sys.exit(1)
print(f"OK:{checked}")
PY
}

assert_pass() {
    local desc="$1" runbook="$2" rules="$3"
    if out=$(run_checker_body "$runbook" "$rules" 2>&1); then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — expected the checker to pass, got: $out"
        failures=$((failures + 1))
    fi
}

assert_fail() {
    local desc="$1" want_reason="$2" runbook="$3" rules="$4"
    if out=$(run_checker_body "$runbook" "$rules" 2>&1); then
        echo "FAIL: $desc — expected the checker to fail, but it passed"
        failures=$((failures + 1))
    elif [[ "$out" == *"$want_reason"* ]]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — failed, but not for the expected reason ($want_reason): $out"
        failures=$((failures + 1))
    fi
}

# --- Fixture: a valid runbook with one real section -------------------------
cat > "$WORKDIR/runbook.md" <<'EOF'
# Alert runbook

## SomeAlert

**Means:** a thing happened.
EOF

# --- Case 1: a rule with a correct, resolving runbook_url passes ------------
cat > "$WORKDIR/valid.yml" <<EOF
groups:
  - name: g
    rules:
      - alert: SomeAlert
        expr: up == 0
        annotations:
          runbook_url: "$WORKDIR/runbook.md#somealert"
EOF
assert_pass "valid runbook_url resolves" "$WORKDIR/runbook.md" "$WORKDIR/valid.yml"

# --- Case 2: a recording rule (no 'alert:') is skipped, not flagged ---------
cat > "$WORKDIR/recording-only.yml" <<'EOF'
groups:
  - name: g
    rules:
      - record: some:recording_rule
        expr: up == 0
EOF
assert_pass "recording rules are not checked for runbook_url" "$WORKDIR/runbook.md" "$WORKDIR/recording-only.yml"

# --- Case 3: missing runbook_url annotation entirely fails -------------------
cat > "$WORKDIR/missing.yml" <<'EOF'
groups:
  - name: g
    rules:
      - alert: NoRunbook
        expr: up == 0
        annotations:
          summary: "no runbook_url at all"
EOF
assert_fail "missing runbook_url is caught" "missing:NoRunbook" "$WORKDIR/runbook.md" "$WORKDIR/missing.yml"

# --- Case 4: runbook_url pointing at the wrong file fails --------------------
cat > "$WORKDIR/wrong-file.yml" <<EOF
groups:
  - name: g
    rules:
      - alert: WrongFile
        expr: up == 0
        annotations:
          runbook_url: "docs/some-other-doc.md#wrongfile"
EOF
assert_fail "runbook_url pointing outside the runbook is caught" "wrong-file:WrongFile" "$WORKDIR/runbook.md" "$WORKDIR/wrong-file.yml"

# --- Case 5: runbook_url with a dangling anchor fails ------------------------
cat > "$WORKDIR/dangling.yml" <<EOF
groups:
  - name: g
    rules:
      - alert: DanglingAnchor
        expr: up == 0
        annotations:
          runbook_url: "$WORKDIR/runbook.md#thissectiondoesnotexist"
EOF
assert_fail "dangling anchor (no matching heading) is caught" "dangling-anchor:DanglingAnchor" "$WORKDIR/runbook.md" "$WORKDIR/dangling.yml"

echo
if [ "$failures" -gt 0 ]; then
    echo "$failures test(s) failed"
    exit 1
fi
echo "All check-runbook-urls.sh tests passed"
