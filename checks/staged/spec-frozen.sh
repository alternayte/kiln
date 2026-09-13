#!/usr/bin/env bash
# check: spec-frozen
# born: 2026-09-13
# failure: an agent edited docs/internal/sdd.md until the frozen design agreed with its code, and every later session inherited the bug as a fact
# rule: a staged change to docs/internal/sdd.md is refused unless KILN_SPEC_CHANGE=1
set -euo pipefail

if ! git diff --cached --name-only --diff-filter=ACMR | grep -qx 'docs/internal/sdd.md'; then
  exit 0
fi
if [ "${KILN_SPEC_CHANGE:-}" = "1" ]; then
  exit 0
fi
echo "REJECTED: commit touches docs/internal/sdd.md. The design is frozen." >&2
echo "  If the design is wrong, say so and stop. Override: KILN_SPEC_CHANGE=1" >&2
exit 1
