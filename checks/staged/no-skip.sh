#!/usr/bin/env bash
# check: no-skip
# born: 2026-09-13
# failure: a test called t.Skip, so a missing precondition turned a red build green and a gate reported success without running its work
# rule: a staged *_test.go file that calls t.Skip is refused
set -uo pipefail

fail=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  hits="$(git show ":$f" 2>/dev/null | grep -nE 't\.Skip' || true)"
  if [ -n "$hits" ]; then
    echo "$hits" | sed "s|^|$f:|" >&2
    fail=1
  fi
done < <(git diff --cached --name-only --diff-filter=ACMR | grep -E '_test\.go$' || true)

if [ "$fail" -ne 0 ]; then
  echo "REJECTED: staged tests contain t.Skip. Assert the precondition and fail instead." >&2
  exit 1
fi
