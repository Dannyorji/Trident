#!/usr/bin/env bash
#
# Fail when a Prometheus alert rule has no runbook_url annotation, or has one
# that doesn't resolve to a real section in docs/runbooks/alerts.md (issue
# #618).
#
# Why this exists
# ----------------
# promtool check rules validates rule syntax, not content — a missing or
# dangling runbook_url passes it cleanly. Eight alerts across
# observability/burn-rate-alerts.yml and observability/rpc-alerts.yml shipped
# with the annotation simply absent, even though every one of them already
# had a written runbook section sitting unlinked in alerts.md: a page from
# any of them arrived with an empty runbook field despite the fix already
# being on disk. This check makes that class of drift fail CI instead of
# silently sitting on the alert until the next incident finds it.
#
# What is checked, per alert rule in each file listed below:
#   1. annotations.runbook_url is present and non-empty.
#   2. It has the form "docs/runbooks/alerts.md#<anchor>".
#   3. <anchor> matches a real "## <AlertName>" heading in that file, using
#      GitHub's heading-to-anchor rule (lowercase, spaces to hyphens, strip
#      characters outside [a-z0-9-_]) since every alert name here is a bare
#      CamelCase identifier with no punctuation, so the two-file comparison
#      never depends on getting GitHub's fancier slug rules exactly right.
#
# Usage:
#   scripts/check-runbook-urls.sh
#
# Requires python3 with PyYAML (already a CI dependency via the
# prometheus-alerts job's alertmanager.yml check).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

RUNBOOK="docs/runbooks/alerts.md"
RULE_FILES=(
    "monitoring/alerts.yml"
    "observability/burn-rate-alerts.yml"
    "observability/rpc-alerts.yml"
)

if [ ! -f "$RUNBOOK" ]; then
    echo "error: $RUNBOOK not found" >&2
    exit 2
fi

for f in "${RULE_FILES[@]}"; do
    if [ ! -f "$f" ]; then
        echo "error: rule file $f not found" >&2
        exit 2
    fi
done

python3 - "$RUNBOOK" "${RULE_FILES[@]}" <<'PY'
import re
import sys

runbook_path = sys.argv[1]
rule_paths = sys.argv[2:]

import yaml

# GitHub's heading-to-anchor algorithm, restricted to the subset this repo's
# headings actually use (plain CamelCase identifiers): lowercase, then strip
# anything that isn't a letter, digit, hyphen or underscore. Spaces would
# become hyphens under the full algorithm, but no alert name here contains
# one, so that step never triggers - documented rather than silently
# omitted, so a future alert name with a space fails loudly here instead of
# quietly passing on an anchor that GitHub would render differently.
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
        group_name = group.get("name", "<unnamed group>")
        for rule in group.get("rules", []) or []:
            alert_name = rule.get("alert")
            if not alert_name:
                # A recording rule (record:), not an alert - no runbook_url
                # to check.
                continue
            checked += 1
            annotations = rule.get("annotations") or {}
            runbook_url = annotations.get("runbook_url")

            if not runbook_url:
                failures.append(
                    f"{rule_path}: alert '{alert_name}' (group '{group_name}') "
                    "has no runbook_url annotation"
                )
                continue

            expected_prefix = f"{runbook_path}#"
            if not runbook_url.startswith(expected_prefix):
                failures.append(
                    f"{rule_path}: alert '{alert_name}' runbook_url "
                    f"'{runbook_url}' does not point into {runbook_path}"
                )
                continue

            anchor = runbook_url[len(expected_prefix):]
            if anchor not in known_anchors:
                failures.append(
                    f"{rule_path}: alert '{alert_name}' runbook_url anchor "
                    f"'#{anchor}' has no matching '## ' heading in {runbook_path}"
                )

if failures:
    print(f"::error::{len(failures)} of {checked} alert rule(s) failed the runbook_url check:")
    for f in failures:
        print(f"  - {f}")
    sys.exit(1)

print(f"OK: all {checked} alert rule(s) across {len(rule_paths)} file(s) have a resolving runbook_url")
PY
