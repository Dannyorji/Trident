#!/usr/bin/env bash
# Point branch protection on dev and main at the three aggregate checks.
#
# Each of the three CI workflows ends in one aggregate job that fails when any
# job it depends on failed or was cancelled and treats path-filtered (skipped)
# jobs as success, so these are the only checks that ever need to be required:
#
#   ci-ok            .github/workflows/ci.yml
#   integration-ok   .github/workflows/integration.yml
#   security-ok      .github/workflows/security.yml
#
# Adding, renaming or removing an individual job never changes this list.
#
# Usage: scripts/set-branch-protection.sh [owner/repo]
# Needs: gh authenticated as a repo admin, and protection already enabled on
# both branches (this only updates the required-status-checks part).
set -euo pipefail

repo="${1:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}"

for branch in dev main; do
  echo "Setting required status checks on ${repo}@${branch}"
  gh api -X PATCH "repos/${repo}/branches/${branch}/protection/required_status_checks" \
    -F strict=true \
    -f 'contexts[]=ci-ok' \
    -f 'contexts[]=integration-ok' \
    -f 'contexts[]=security-ok' >/dev/null
  gh api "repos/${repo}/branches/${branch}/protection/required_status_checks" \
    -q '"  required: " + (.contexts | join(", "))'
done
