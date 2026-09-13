#!/usr/bin/env bash
# check: versions
# born: 2026-09-13
# failure: cmd/kiln/versions_gen.go drifted from scripts/versions.env, so the binary fetched versions the pins no longer named
# rule: cmd/kiln/versions_gen.go is byte-identical to what scripts/gen-versions.sh produces from scripts/versions.env
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
scripts/gen-versions.sh "$tmp"
if ! diff -u cmd/kiln/versions_gen.go "$tmp"; then
  echo "rule: regenerate: scripts/gen-versions.sh > cmd/kiln/versions_gen.go" >&2
  exit 1
fi
