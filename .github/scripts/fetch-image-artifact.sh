#!/usr/bin/env bash
# Reuse an image that integration.yml already built for this commit, instead of
# building it a second time.
#
# integration.yml builds each image once and uploads it as the artifact
# `image-<name>`. security.yml scans those same images. The two workflows run
# in parallel, so this waits for the artifact to appear.
#
# Usage: fetch-image-artifact.sh <name> <commit-sha> [max-wait-seconds]
# Env:   GH_TOKEN (needs `actions: read`), GITHUB_REPOSITORY
#
# Exit codes:
#   0  artifact downloaded and `docker load`ed
#   1  no usable artifact (integration skipped it, failed first, or timed out);
#      the caller falls back to building the image itself.
set -uo pipefail

name="$1"
sha="$2"
max_wait="${3:-1500}"
deadline=$((SECONDS + max_wait))
# integration.yml may not have started yet when this job does; give up on
# finding a run at all after this long (it was skipped by its path filters).
no_run_deadline=$((SECONDS + 240))

while [ "$SECONDS" -lt "$deadline" ]; do
  run=$(gh run list --workflow integration.yml --commit "$sha" --limit 1 \
    --json databaseId,status -q 'if length == 0 then empty else .[0] | "\(.databaseId) \(.status)" end' 2>/dev/null || true)

  if [ -z "$run" ]; then
    if [ "$SECONDS" -ge "$no_run_deadline" ]; then
      echo "No integration.yml run found for $sha; building locally."
      exit 1
    fi
  else
    run_id="${run% *}"
    status="${run#* }"
    artifact_id=$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}/artifacts" \
      -q ".artifacts[] | select(.name == \"image-${name}\" and (.expired | not)) | .id" 2>/dev/null | head -n1)
    if [ -n "$artifact_id" ]; then
      dest=$(mktemp -d)
      if gh run download "$run_id" -n "image-${name}" -D "$dest" && docker load -i "$dest/image.tar"; then
        echo "Reused image-${name} from integration run ${run_id}."
        exit 0
      fi
      echo "Downloading image-${name} failed; building locally."
      exit 1
    fi
    if [ "$status" = "completed" ]; then
      echo "integration run ${run_id} finished without image-${name}; building locally."
      exit 1
    fi
  fi
  sleep 20
done

echo "Timed out waiting for image-${name}; building locally."
exit 1
