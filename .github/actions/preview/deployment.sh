#!/usr/bin/env bash
# The GitHub deployment of this preview: the object that says "this commit runs
# at this URL". A failed call warns and never fails the run: a workflow with no
# deployments: write must not break on upgrade, and the preview matters more
# than its status.
set -uo pipefail

environment="preview/pr-${PR:-}"

warn() { echo "::warning::the deployment was not reported: $1"; }

# status <deployment> <state> [environment_url]
status() {
  local body
  body="$(jq -n --arg state "$2" --arg log "${LOG_URL:-}" --arg url "${3:-}" \
    '{state: $state} + (if $log == "" then {} else {log_url: $log} end) + (if $url == "" then {} else {environment_url: $url} end)')"
  gh api -X POST "repos/$REPO/deployments/$1/statuses" --input - <<< "$body" > /dev/null
}

# retire [keep] sets inactive on every deployment of this pull request except
# one. GitHub skips a transient environment in its automatic inactive, so these
# would go on naming previews that no longer exist.
retire() {
  local ids id state
  ids="$(gh api -X GET --paginate "repos/$REPO/deployments" -f environment="$environment" -F per_page=100 --jq '.[].id')" || return 1
  for id in $ids; do
    [ "$id" = "${1:-}" ] && continue
    state="$(gh api "repos/$REPO/deployments/$id/statuses?per_page=1" --jq '.[0].state // ""')" || return 1
    [ "$state" = inactive ] && continue
    status "$id" inactive || return 1
  done
}

case "$1" in
  start)
    # The SHA is what Factoree compares with the published commit, so ref is
    # the SHA and never the branch.
    body="$(jq -n --arg ref "$SHA" --arg env "$environment" \
      '{ref: $ref, environment: $env, transient_environment: true, auto_merge: false, required_contexts: []}')"
    if ! id="$(gh api -X POST "repos/$REPO/deployments" --input - --jq .id <<< "$body")"; then
      warn "create failed; does the workflow have deployments: write?"
      exit 0
    fi
    echo "id=$id" >> "$GITHUB_OUTPUT"
    status "$id" in_progress || warn "in_progress failed"
    ;;
  success)
    [ -n "${DEPLOYMENT:-}" ] || exit 0
    status "$DEPLOYMENT" success "$URL" || warn "success failed"
    retire "$DEPLOYMENT" || warn "the earlier deployments stay active"
    ;;
  failure)
    status "$DEPLOYMENT" failure || warn "failure failed"
    ;;
  retire)
    retire || warn "the deployments stay active"
    ;;
  *)
    echo "unknown deployment step $1" >&2
    exit 2
    ;;
esac
exit 0
