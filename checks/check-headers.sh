#!/usr/bin/env bash
# check: check-headers
# born: 2026-09-12
# failure: a check without its origin is a veto nobody can safely retire
# rule: every script in checks/ and checks/staged/ has non-empty check, born, failure and rule header fields, and check matches the file name
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
fail=0
shopt -s nullglob
for f in "$root"/checks/*.sh "$root"/checks/staged/*.sh; do
  head="$(head -n 12 "$f")"
  for field in check born failure rule; do
    if ! grep -Eq "^# ${field}: [^[:space:]].*" <<< "$head"; then
      echo "rule: check header field '$field' missing or empty: ${f#$root/}" >&2
      fail=1
    fi
  done
  name="$(sed -n 's/^# check: //p' <<< "$head" | head -1)"
  if [[ -n "$name" && "$name" != "$(basename "$f" .sh)" ]]; then
    echo "rule: '# check: $name' does not match file name: ${f#$root/}" >&2
    fail=1
  fi
  born="$(sed -n 's/^# born: //p' <<< "$head" | head -1)"
  if [[ -n "$born" ]] && ! [[ "$born" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
    echo "rule: '# born:' must be YYYY-MM-DD: ${f#$root/}" >&2
    fail=1
  fi
done
exit "$fail"
