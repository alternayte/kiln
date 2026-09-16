#!/usr/bin/env bash
# The pull request closed. Every preview of it goes, and so does its template.
set -euo pipefail
KEEP="" exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/prune.sh"
