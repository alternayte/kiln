#!/usr/bin/env bash
# check: sdk
# born: 2026-09-16
# failure: the generated clients and the OpenAPI document drifted from internal/apispec, so an SDK called a route the API no longer serves
# rule: sdk/ holds exactly what cmd/kiln-sdk produces from internal/apispec
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
go run ./cmd/kiln-sdk --check
echo "sdk: ok"
