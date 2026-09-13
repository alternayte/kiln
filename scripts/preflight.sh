#!/usr/bin/env bash
# Assert the host can run VM work. Fails closed. Never skips.
set -uo pipefail

here="${BASH_SOURCE[0]%/*}"
[ "$here" = "${BASH_SOURCE[0]}" ] && here="."
here="$(cd "$here" && pwd -P)"
# shellcheck source=versions.env
source "$here/versions.env"

ROOT="${KILN_ROOT:-/var/lib/kiln}"
KVM="${KILN_KVM_DEV:-/dev/kvm}"
fail=0
miss() { echo "preflight: $*" >&2; fail=1; }

if [ ! -e "$KVM" ] || [ ! -r "$KVM" ] || [ ! -w "$KVM" ]; then
  miss "KVM: $KVM is missing or not readable and writable"
fi

# Resolve from the Kiln root first, then PATH.
find_bin() {
  if [ -x "$ROOT/bin/$1" ]; then
    echo "$ROOT/bin/$1"
    return 0
  fi
  command -v "$1" 2>/dev/null
}

for b in firecracker jailer nft mke2fs; do
  if [ -z "$(find_bin "$b")" ]; then
    miss "binary: $b is not on PATH and not at $ROOT/bin/$b"
  fi
done

fc="$(find_bin firecracker)"
if [ -n "$fc" ]; then
  out="$("$fc" --version 2>&1 || true)"
  v="${out##* }"
  if [ "$v" != "$FIRECRACKER_VERSION" ]; then
    miss "version: firecracker reports '$v', pin is '$FIRECRACKER_VERSION'"
  fi
fi

if [ "$fail" -ne 0 ]; then
  echo "preflight: failed" >&2
  exit 1
fi
echo "preflight: ok: $KVM, firecracker $FIRECRACKER_VERSION, kernel $KERNEL_VERSION"
