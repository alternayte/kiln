#!/usr/bin/env bash
# check: sdk
# born: 2026-09-16
# failure: the generated clients and the OpenAPI document drifted from internal/apispec, so an SDK called a route the API no longer serves
# rule: sdk/ holds exactly what cmd/kiln-sdk produces from internal/apispec, and the handwritten layers on top of it compile
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
go run ./cmd/kiln-sdk --check

# The handwritten layers are the SDKs people call, and nothing else builds
# them. The TypeScript compiler comes with the web UI's dependencies, which
# checks/web.sh installs.
python3 -c "import ast; ast.parse(open('sdk/python/kiln/__init__.py').read())"
tsc="web/node_modules/.bin/tsc"
if [ ! -x "$tsc" ]; then
  (cd web && bun install --frozen-lockfile > /dev/null)
fi
"$tsc" -p sdk/typescript/tsconfig.json --noEmit
echo "sdk: ok"
