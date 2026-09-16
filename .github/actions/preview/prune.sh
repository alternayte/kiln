#!/usr/bin/env bash
# Destroy the previews of this pull request except the one named by KEEP, and
# delete the templates they came from. A pull request holds one preview.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
keep="${KEEP:-}"

api() {
  curl -sS -X "$1" -H "Authorization: Bearer $KILN_API_KEY" -w $'\n%{http_code}' "$KILN_URL$2"
}

rows="$(api GET /v1/sandboxes | sed '$d' | python3 "$here/json.py" previews)"
while read -r id template; do
  [ -n "$id" ] || continue
  [ "$template" = "$keep" ] && continue
  echo "destroying the previous preview $id"
  api DELETE "/v1/sandboxes/$id" > /dev/null || true
  [ -n "$template" ] || continue
  # The template holds the snapshot, so it goes once its sandbox has.
  api DELETE "/v1/templates/$template" > /dev/null || true
done <<< "$rows"
