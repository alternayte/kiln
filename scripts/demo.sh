#!/usr/bin/env bash
# just demo is the v1 done-line. It needs every phase through P6.
set -euo pipefail

echo "demo: the v1 done-line needs every phase through P6. No sandbox path exists yet." >&2
root="$(git rev-parse --show-toplevel 2>/dev/null || echo .)"
if command -v just >/dev/null; then
  (cd "$root" && just status) >&2 || true
fi
exit 1
