#!/usr/bin/env bash
# Wrap a command. Count host resources before and after. Any difference fails.
#
# This is the highest-value check in the project. The most common bug here is a
# resource that outlives its sandbox, and it is silent until the host runs out.
set -uo pipefail

ROOT="${KILN_ROOT:-/var/lib/kiln}"
DB="$ROOT/kiln.db"

snapshot() {
  echo "procs $(pgrep -c firecracker 2>/dev/null || echo 0)"
  echo "taps $(ip -o link show 2>/dev/null | grep -c 'kiln-' || echo 0)"
  echo "chains $(nft list chains 2>/dev/null | grep -c 'kiln_' || echo 0)"
  echo "mounts $(grep -c " $ROOT" /proc/mounts 2>/dev/null || echo 0)"
  echo "dirs $(ls -1 "$ROOT/sandboxes" 2>/dev/null | wc -l)"
  echo "rows $(sqlite3 "$DB" 'select count(*) from sandboxes where destroyed_at is null' 2>/dev/null || echo 0)"
}

names() {
  echo "--- firecracker pids:"; pgrep -a firecracker 2>/dev/null || true
  echo "--- taps:";   ip -o link show 2>/dev/null | grep 'kiln-' || true
  echo "--- chains:"; nft list chains 2>/dev/null | grep 'kiln_' || true
  echo "--- dirs:";   ls -1 "$ROOT/sandboxes" 2>/dev/null || true
}

before="$(snapshot)"
"$@"; rc=$?
sleep 1                      # let normal teardown finish before accusing it
after="$(snapshot)"

if [ "$before" != "$after" ]; then
  echo "LEAK DETECTED" >&2
  diff <(echo "$before") <(echo "$after") >&2 || true
  echo "" >&2; names >&2
  exit 1
fi

echo "leak check: clean"
exit $rc
