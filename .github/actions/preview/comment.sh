#!/usr/bin/env bash
# One comment per pull request, edited in place. Thirty commits must not leave
# thirty comments.
set -euo pipefail

marker="<!-- kiln-preview -->"
body="$marker
Preview: $URL"
# The other published ports, in the order the workflow named them.
urls="${URLS:-}"
[ -n "$urls" ] || urls='{}'
extra="$(jq -r --arg port "$PORT" 'to_entries[] | select(.key != $port) | "Port \(.key): \(.value)"' <<< "$urls")"
if [ -n "$extra" ]; then
  body="$body
$extra"
fi

id="$(gh api "repos/$REPO/issues/$PR/comments" --jq ".[] | select(.body | contains(\"$marker\")) | .id" | head -n1)"
if [ -n "$id" ]; then
  gh api -X PATCH "repos/$REPO/issues/comments/$id" -f body="$body" > /dev/null
  echo "edited comment $id"
else
  gh api -X POST "repos/$REPO/issues/$PR/comments" -f body="$body" > /dev/null
  echo "commented"
fi
