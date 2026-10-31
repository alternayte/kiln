#!/usr/bin/env bash
# The pull request closed. Every preview of it goes, and so does its template.
set -euo pipefail
exec kiln preview down --group "repository=$REPO" --group "pull_request=$PR"
