#!/usr/bin/env bash
# Prepare a Linux host with KVM for Kiln development. Idempotent.
set -euo pipefail

here="${BASH_SOURCE[0]%/*}"
[ "$here" = "${BASH_SOURCE[0]}" ] && here="."
here="$(cd "$here" && pwd -P)"
# shellcheck source=versions.env
source "$here/versions.env"

ROOT="${KILN_ROOT:-/var/lib/kiln}"
KVM="${KILN_KVM_DEV:-/dev/kvm}"

if [ ! -e "$KVM" ] || [ ! -r "$KVM" ] || [ ! -w "$KVM" ]; then
  echo "dev-env: $KVM is missing or not readable and writable. KVM is required." >&2
  exit 1
fi

sudo=()
if [ "$(id -u)" -ne 0 ]; then
  sudo=(sudo)
fi

packages=(iproute2 nftables e2fsprogs sqlite3 curl ca-certificates tar gzip jq git)
if ! command -v apt-get >/dev/null; then
  echo "dev-env: no apt-get. Install by hand: ${packages[*]}" >&2
  exit 1
fi

missing=()
for p in "${packages[@]}"; do
  dpkg -s "$p" >/dev/null 2>&1 || missing+=("$p")
done
if [ "${#missing[@]}" -gt 0 ]; then
  echo "dev-env: installing ${missing[*]}"
  "${sudo[@]}" apt-get update -qq
  DEBIAN_FRONTEND=noninteractive "${sudo[@]}" apt-get install -y -qq "${missing[@]}"
fi

"${sudo[@]}" install -d -m 0755 "$ROOT"
echo "dev-env: root $ROOT"
if [ -r /etc/os-release ]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  echo "dev-env: os ${PRETTY_NAME:-unknown}"
fi
echo "dev-env: host kernel $(uname -r)"
echo "dev-env: kvm $KVM"
echo "dev-env: firecracker pin $FIRECRACKER_VERSION ($ARCH)"
echo "dev-env: kernel pin $KERNEL_VERSION"
for b in nft mke2fs sqlite3 ip go just git; do
  if command -v "$b" >/dev/null; then
    echo "dev-env: $b $(command -v "$b")"
  else
    echo "dev-env: $b missing"
  fi
done
