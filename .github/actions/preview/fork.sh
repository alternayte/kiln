#!/usr/bin/env bash
# A pull request from a fork runs a preview only while a maintainer's label
# stands, and a new commit takes the label away.
set -euo pipefail

if [ "$FORK" != "true" ]; then exit 0; fi

if [ "$ACTION" = "synchronize" ]; then
  gh api -X DELETE "repos/$REPO/issues/$PR/labels/preview" > /dev/null 2>&1 || true
  echo "A new commit landed on a fork pull request, so the preview label is removed." >&2
  echo "A maintainer labels it again to run a preview for this commit." >&2
  exit 1
fi

if grep -q '"preview"' <<< "$LABELS"; then exit 0; fi
echo "This pull request comes from a fork and carries no preview label." >&2
echo "A maintainer adds the label to run a preview for it." >&2
exit 1
