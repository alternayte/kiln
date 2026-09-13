#!/usr/bin/env bash
# Clear a host that cannot reconcile. Removes every Kiln host resource and the
# Kiln root. This is the last resort; the reconciler is the normal path.
set -uo pipefail

ROOT="${KILN_ROOT:-/var/lib/kiln}"

case "$ROOT" in
  ""|"/"|"/var"|"/var/lib"|"/usr"|"/usr/local"|"/home"|"/etc"|"/tmp")
    echo "nuke: refusing to remove $ROOT" >&2
    exit 1
    ;;
esac

echo "nuke: killing Kiln processes"
pkill -f "$ROOT" 2>/dev/null || true
pkill -x firecracker 2>/dev/null || true
pkill -x kiln 2>/dev/null || true
pkill -f 'kiln snapfault' 2>/dev/null || true
sleep 1

if command -v ip >/dev/null; then
  echo "nuke: deleting kiln- TAP devices"
  for tap in $(ip -o link show 2>/dev/null | sed -n 's/^[0-9]*: \(kiln-[^:@]*\).*/\1/p'); do
    ip link del "$tap" 2>/dev/null || true
  done
fi

if command -v nft >/dev/null; then
  echo "nuke: flushing kiln_ chains"
  nft list chains 2>/dev/null | awk '
    /^table / { fam=$2; tbl=$3 }
    /chain kiln_/ { name=$2; print fam, tbl, name }
  ' | while read -r fam tbl chain; do
    nft flush chain "$fam" "$tbl" "$chain" 2>/dev/null || true
  done
  echo "nuke: deleting kiln tables"
  nft list tables 2>/dev/null | while read -r _ fam tbl; do
    case "$tbl" in
      *kiln*) nft delete table "$fam" "$tbl" 2>/dev/null || true ;;
    esac
  done
fi

if [ -r /proc/mounts ]; then
  mounts="$(grep " $ROOT" /proc/mounts 2>/dev/null | awk '{print $2}' | sort -r || true)"
  if [ -n "$mounts" ]; then
    echo "nuke: unmounting $ROOT mounts"
    while read -r m; do
      umount -l "$m" 2>/dev/null || umount "$m" 2>/dev/null || true
    done <<< "$mounts"
  fi
fi

echo "nuke: removing $ROOT"
rm -rf "$ROOT"

left=0
if pgrep -f "$ROOT" >/dev/null 2>&1; then
  echo "nuke: processes still match $ROOT" >&2
  left=1
fi
if [ -e "$ROOT" ]; then
  echo "nuke: $ROOT still exists" >&2
  left=1
fi
if [ "$left" -ne 0 ]; then
  echo "nuke: incomplete" >&2
  exit 1
fi
echo "nuke: clean"
