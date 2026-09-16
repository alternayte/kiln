#!/usr/bin/env bash
# Cross-compile the guest agent into the directory the kiln binary embeds.
# The kiln build reads that file, so this runs before it, the way the web
# bundle is built before the gateway.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
out="internal/guestbin/bin/kilninit"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o "$out" ./guest/kilninit
echo "kilninit -> $out"
