#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <image>" >&2
  exit 2
fi

image="$1"
history_file="$(mktemp)"
config_file="$(mktemp)"
trap 'rm -f "$history_file" "$config_file"' EXIT

# docker history exposes layer-creating commands, including ARG/ENV values.
# Inspect the final image config as well so a secret cannot hide in Env/Labels.
docker history --no-trunc --format '{{.CreatedBy}}' "$image" > "$history_file"
docker image inspect --format '{{json .Config.Env}} {{json .Config.Labels}}' "$image" > "$config_file"

sensitive='(DATABASE_URL|REDIS_URL|STELLAR_RPC_URLS?|PGBOUNCER_ADMIN_URL|ADMIN_API_KEY|INTERNAL_API_KEY|STAGING_API_KEY|API_KEY_SALT|API_KEY_HASHES|ALERT_WEBHOOK_URL|OTEL_EXPORTER_OTLP_ENDPOINT|NPM_TOKEN|STAGING_KUBECONFIG|INTERNAL_(SERVER|CLIENT)_KEY|BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY)'

if grep -Eiq "$sensitive" "$history_file" "$config_file"; then
  echo "secret-bearing key name or private-key marker found in image history/config: $image" >&2
  # Do not print the matching line: if this check ever catches a real value,
  # echoing the layer command would leak it into the CI log.
  exit 1
fi

echo "image history/config contains no secret-bearing fields: $image"
