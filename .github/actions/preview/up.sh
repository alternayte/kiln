#!/usr/bin/env bash
# Build this commit into a template, run one sandbox from it, publish the
# application port, and print the hostname. kiln preview up does the work, so
# the Action and an agent run one flow.
set -euo pipefail

# Every value is masked first, so no later line of the log shows one.
while IFS= read -r line; do
  value="${line#*=}"
  if [ "$value" != "$line" ] && [ -n "$value" ]; then
    echo "::add-mask::$value"
  fi
done <<< "$ENV_LINES"
if [ -n "${REGISTRY_TOKEN:-}" ]; then
  echo "::add-mask::$REGISTRY_TOKEN"
fi

# A fork's commit never sees env: a label approves the code, not what the code
# can reach, and a value can leave through the published port.
if [ "$FORK" = "true" ]; then
  if [ -n "$(tr -d '[:space:]' <<< "$ENV_LINES")" ]; then
    echo "this pull request comes from a fork, so env is not sent to its preview"
  fi
  ENV_LINES=""
fi

args=(
  preview up --json
  --name "pr-${PR}-${SHA:0:7}"
  --image "$IMAGE"
  --port "$PORT"
  --start "$START"
  --publish "$PUBLISH"
  --vcpus "$VCPUS"
  --memory-mb "$MEMORY_MB"
  --disk-mb "$DISK_MB"
  --ttl-seconds "$TTL_SECONDS"
  --idle-seconds "$IDLE_SECONDS"
  --env-file -
  --registry-username "$REGISTRY_USERNAME"
  # The group finds the previous preview of one pull request, so the Action
  # keeps no state of its own.
  --group "repository=$REPO"
  --group "pull_request=$PR"
  --metadata "commit=$SHA"
)
for host in $EGRESS; do
  args+=(--egress-allow "$host")
done
while IFS= read -r line; do
  if [ -n "${line//[[:space:]]/}" ]; then
    args+=(--setup "$line")
  fi
done <<< "$SETUP"

# The client reads the token from the environment, so it stays off the command
# line. With --json, a failure comes back on stdout.
if ! out="$(printf '%s\n' "$ENV_LINES" | KILN_REGISTRY_TOKEN="${REGISTRY_TOKEN:-}" kiln "${args[@]}")"; then
  echo "the preview did not come up: $out" >&2
  exit 1
fi

url="$(jq -r .url <<< "$out")"
echo "url=$url" >> "$GITHUB_OUTPUT"
echo "urls=$(jq -c .urls <<< "$out")" >> "$GITHUB_OUTPUT"
echo "preview at $url"
