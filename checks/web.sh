#!/usr/bin/env bash
# check: web
# born: 2026-09-16
# failure: the embedded web UI did not compile, so the gateway shipped a broken or stale bundle
# rule: web/ type-checks and builds, and its generated API client matches sdk/openapi.json
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root/web"

if ! command -v bun > /dev/null; then
  echo "web: bun is not installed. Install it, or build the image, which carries its own." >&2
  exit 1
fi

bun install --frozen-lockfile > /dev/null
# The client comes from internal/apispec by way of sdk/openapi.json, so a
# route change that the UI has not seen fails here rather than in a browser.
bun run generate > /dev/null
if ! git diff --quiet -- src/api; then
  echo "web: src/api is stale. Run 'bun run generate' in web/ and commit it." >&2
  git --no-pager diff --stat -- src/api >&2
  exit 1
fi
bun run build > /dev/null
echo "web: ok"
