#!/usr/bin/env bash
# Install the kiln client of this Action's tag, and check it against the
# release checksums. An Action not pinned to a tag installs the latest release.
set -euo pipefail

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "no kiln client for $(uname -m)" >&2; exit 1 ;;
esac
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
asset="kiln-$os-$arch"

if [[ "${ACTION_REF:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
  base="https://github.com/alternayte/kiln/releases/download/$ACTION_REF"
else
  base="https://github.com/alternayte/kiln/releases/latest/download"
fi

dir="$RUNNER_TEMP/kiln-client"
mkdir -p "$dir"
curl -fsSL -o "$dir/$asset" "$base/$asset"
curl -fsSL -o "$dir/SHA256SUMS" "$base/SHA256SUMS"
(cd "$dir" && grep " $asset\$" SHA256SUMS | sha256sum -c -)
install -m755 "$dir/$asset" "$dir/kiln"
echo "$dir" >> "$GITHUB_PATH"
