#!/usr/bin/env bash
# One comment per pull request, edited in place. Thirty commits must not leave
# thirty comments.
set -euo pipefail

marker="<!-- kiln-preview -->"
body="$marker
Preview: $URL"

id="$(gh api "repos/$REPO/issues/$PR/comments" --jq ".[] | select(.body | contains(\"$marker\")) | .id" | head -n1)"
if [ -n "$id" ]; then
  gh api -X PATCH "repos/$REPO/issues/comments/$id" -f body="$body" > /dev/null
  echo "edited comment $id"
else
  gh api -X POST "repos/$REPO/issues/$PR/comments" -f body="$body" > /dev/null
  echo "commented"
fi
