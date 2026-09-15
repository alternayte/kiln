#!/usr/bin/env bash
# just demo is the v1 done-line. It runs the sequence in the SDD against a
# live kiln serve, and it asserts every step. It asserts no timing bound: it
# prints the create wall time against the last recorded value.
#
# It needs config.json with a zone and an ACME account, wildcard DNS for
# *.<zone> that points at this host, and ports 443 and 80 reachable.
set -euo pipefail

ROOT="${KILN_ROOT:-/var/lib/kiln}"
CONFIG="$ROOT/config.json"
[ -f "$CONFIG" ] || { echo "demo: $CONFIG is missing; run kiln init" >&2; exit 1; }
TOKEN="$(jq -er .bearer_token "$CONFIG")"
CONTROL="http://$(jq -r '.control_addr // "127.0.0.1:8080"' "$CONFIG")"
ZONE="$(jq -er .zone "$CONFIG")"
TIMING="$ROOT/demo-create-ms"
TEMPLATE="demo"
IMAGE="docker.io/library/python:3.12-slim"
EGRESS='["pypi.org","files.pythonhosted.org"]'

need() { command -v "$1" >/dev/null || { echo "demo: $1 is not on PATH" >&2; exit 1; }; }
need curl
need jq
need ip
need nft

sandboxes=()
snapshots=()
published=()
pass=0
step() { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
fail() { echo "demo: FAILED: $1" >&2; exit 1; }

cleanup() {
  local rc=$?
  trap - EXIT
  for id in "${sandboxes[@]:-}"; do
    [ -n "$id" ] || continue
    curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" "$CONTROL/v1/sandboxes/$id" || true
  done
  for id in "${snapshots[@]:-}"; do
    [ -n "$id" ] || continue
    curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" "$CONTROL/v1/snapshots/$id" || true
  done
  curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" "$CONTROL/v1/templates/$TEMPLATE" || true
  exit "$rc"
}
trap cleanup EXIT

api() { # method path [body]
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS --fail-with-body -X "$method" -H "Authorization: Bearer $TOKEN")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  local out
  if ! out="$(curl "${args[@]}" "$CONTROL$path")"; then
    echo "demo: $method $path failed: $out" >&2
    return 1
  fi
  printf '%s' "$out"
}

status() { # method url -> HTTP status, 000 when the call fails
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X "$1" "$2" 2>/dev/null)" || code=000
  echo "${code:-000}"
}

guest() { # id cmd...  -> stdout; the call fails on a non-zero exit
  local id="$1"; shift
  local payload out code
  payload="$(jq -n '{cmd: $ARGS.positional, timeout_seconds: 300}' --args -- "$@")"
  out="$(api POST "/v1/sandboxes/$id/exec" "$payload")"
  code="$(jq -r .exit_code <<<"$out")"
  if [ "$code" != "0" ]; then
    echo "demo: guest exec failed ($code): $out" >&2
    exit 1
  fi
  # -j prints stdout exactly. -r appends a newline, which a loop that
  # collects outputs counts as one more empty value.
  jq -j '.stdout' <<<"$out"
}

guest_ok() { # id cmd...  -> "yes" when the command exits zero
  local id="$1"; shift
  local payload
  payload="$(jq -n '{cmd: $ARGS.positional, timeout_seconds: 60}' --args -- "$@")"
  local code
  code="$(api POST "/v1/sandboxes/$id/exec" "$payload" | jq -r .exit_code)"
  [ "$code" = "0" ] && echo yes || echo no
}

wait_state() { # id state
  local id="$1" want="$2" state=""
  for _ in $(seq 1 300); do
    state="$(api GET "/v1/sandboxes/$id" | jq -r .state)"
    [ "$state" = "$want" ] && return 0
    sleep 0.5
  done
  fail "sandbox $id stayed in $state, want $want"
}

echo "demo: template $TEMPLATE"

# 1. Import python:3.12-slim and build the template.
created="$(api POST /v1/templates "$(jq -n --arg name "$TEMPLATE" --arg image "$IMAGE" --argjson egress "$EGRESS" \
  '{name: $name, image: $image, vcpus: 2, memory_mb: 512, disk_mb: 2048, setup: [], egress_allow: $egress}')")"
[ -n "$created" ] || fail "create template returned nothing"
for _ in $(seq 1 240); do
  state="$(api GET "/v1/templates/$TEMPLATE" | jq -r .state)"
  [ "$state" = "ready" ] && break
  [ "$state" = "failed" ] && fail "template build failed: $(api GET "/v1/templates/$TEMPLATE" | jq -r .error)"
  sleep 2
done
[ "$state" = "ready" ] || fail "template stuck in $state"
step "template ready"

# 2. Create a sandbox and report the wall time against the last value.
start="$(date +%s%3N)"
SOURCE="$(api POST /v1/sandboxes "$(jq -n --arg template "$TEMPLATE" '{template: $template, lifecycle: "persistent", idle_seconds: 30}')" | jq -er .id)"
sandboxes+=("$SOURCE")
end="$(date +%s%3N)"
create_ms=$((end - start))
last="$(cat "$TIMING" 2>/dev/null || echo none)"
if [ "$last" = "none" ]; then
  printf '  ok   create: %s ms (no earlier value)\n' "$create_ms"
else
  printf '  ok   create: %s ms (last %s ms, %+d ms)\n' "$create_ms" "$last" "$((create_ms - last))"
fi
echo "$create_ms" > "$TIMING"
wait_state "$SOURCE" running
step "sandbox $SOURCE"

# 3. Exec prints 4.
[ "$(guest "$SOURCE" python -c 'print(2+2)')" = "4" ] || fail "python printed something other than 4"
step "exec"

# 4. Write a file in the sandbox.
guest "$SOURCE" sh -c 'mkdir -p /work/www && printf kiln-demo > /work/marker' >/dev/null
[ "$(guest "$SOURCE" cat /work/marker)" = "kiln-demo" ] || fail "the marker did not come back"
step "write /work/marker"

# 5. Snapshot the sandbox.
SNAP="$(api POST "/v1/sandboxes/$SOURCE/snapshot" '{}' | jq -er .snapshot_id)"
snapshots+=("$SNAP")
step "snapshot $SNAP"

# 6. Fork the snapshot into eight sandboxes.
mapfile -t FORKS < <(api POST "/v1/snapshots/$SNAP/restore" '{"count":8}' | jq -r '.sandboxes[].id')
[ "${#FORKS[@]}" = "8" ] || fail "restore made ${#FORKS[@]} sandboxes, want 8"
for id in "${FORKS[@]}"; do sandboxes+=("$id"); done
step "fork 8"

# 7. Every fork sees the pre-fork file.
for i in "${!FORKS[@]}"; do
  [ "$(guest "${FORKS[$i]}" cat /work/marker)" = "kiln-demo" ] || fail "fork $i lost the marker"
done
step "8 forks read the marker"

# 8. Every fork writes its own value, and no fork sees another's.
for i in "${!FORKS[@]}"; do
  guest "${FORKS[$i]}" sh -c "printf 'fork-$i' > /work/marker" >/dev/null
done
for i in "${!FORKS[@]}"; do
  got="$(guest "${FORKS[$i]}" cat /work/marker)"
  [ "$got" = "fork-$i" ] || fail "fork $i read '$got'"
done
step "8 forks diverge"

# 9. Every fork draws its own random bytes.
mapfile -t RANDOMS < <(for id in "${FORKS[@]}"; do
  guest "$id" python -c 'import os; print(os.urandom(16).hex())'
done)
mapfile -t DISTINCT < <(printf '%s\n' "${RANDOMS[@]}" | sort -u)
[ "${#DISTINCT[@]}" = "8" ] || fail "the forks produced ${#DISTINCT[@]} distinct random values"
step "8 forks, 8 random values"

# 10. Every fork's clock is within two seconds of the host.
# Bracket each guest read with host reads, so exec time is not clock drift.
for i in "${!FORKS[@]}"; do
  before="$(date +%s)"
  guest_now="$(guest "${FORKS[$i]}" python -c 'import time; print(int(time.time()))')"
  after="$(date +%s)"
  [ "$guest_now" -ge $((before - 2)) ] && [ "$guest_now" -le $((after + 2)) ] \
    || fail "fork $i clock $guest_now is outside host window [$before, $after] by more than 2s"
done
step "clocks within 2s"

# 11. An allowlisted host is reachable.
[ "$(guest_ok "${FORKS[0]}" python -c "import socket; socket.create_connection(('pypi.org',443),timeout=30).close()")" = "yes" ] \
  || fail "the allowlisted host was not reachable"
step "allowlisted egress"

# 12. A host outside the allowlist is refused.
[ "$(guest_ok "${FORKS[0]}" python -c "import socket; socket.create_connection(('example.com',443),timeout=5)")" = "no" ] \
  || fail "a non-allowlisted host was reachable"
step "non-allowlisted egress refused"

# 13. The metadata address is refused.
[ "$(guest_ok "${FORKS[0]}" python -c "import socket; socket.create_connection(('169.254.169.254',80),timeout=5)")" = "no" ] \
  || fail "the metadata address was reachable"
step "metadata refused"

# 14. Serve a port inside one fork and publish it as public.
PREVIEW="${FORKS[1]}"
guest "$PREVIEW" sh -c "printf 'preview-%s' '$PREVIEW' > /work/www/index.html" >/dev/null
guest "$PREVIEW" sh -c 'cd /work/www && nohup python -m http.server 8000 --bind 0.0.0.0 >/tmp/http.log 2>&1 &' >/dev/null
for _ in $(seq 1 60); do
  [ "$(guest_ok "$PREVIEW" python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000',timeout=2)")" = "yes" ] && break
  sleep 0.5
done
[ "$(guest_ok "$PREVIEW" python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000',timeout=2)")" = "yes" ] \
  || fail "the in-guest server did not start"
PUBLIC_URL="$(api POST "/v1/sandboxes/$PREVIEW/publish" '{"port":8000,"visibility":"public"}' | jq -er .url)"
published+=("$PUBLIC_URL")
for _ in $(seq 1 90); do
  code="$(status GET "$PUBLIC_URL")"
  [ "$code" = "200" ] && break
  sleep 2
done
[ "$code" = "200" ] || fail "the public preview answered $code"
body="$(curl -sS "$PUBLIC_URL")"
[ "$body" = "preview-$PREVIEW" ] || fail "the public preview answered '$body' from the wrong fork"
headers="$(curl -sSI "$PUBLIC_URL")"
grep -qi '^x-robots-tag: noindex' <<<"$headers" || fail "the public preview lost X-Robots-Tag"
grep -qi '^content-security-policy: sandbox' <<<"$headers" || fail "the public preview lost the CSP"
step "public preview $PUBLIC_URL"

# 15. Let it sleep, then fetch again: the request wakes it.
SLEEP_SNAP="$(api POST "/v1/sandboxes/$PREVIEW/snapshot" '{"stop":true}' | jq -er .snapshot_id)"
snapshots+=("$SLEEP_SNAP")
wait_state "$PREVIEW" sleeping
body="$(curl -sS "$PUBLIC_URL")"
[ "$body" = "preview-$PREVIEW" ] || fail "the waking preview answered '$body'"
step "sleep and wake on request"

# 16. Publish a second port as team. An unauthenticated call challenges, and
# the guest never sees it.
guest "$PREVIEW" sh -c 'cd /work/www && nohup python -m http.server 8001 --bind 0.0.0.0 >>/tmp/http.log 2>&1 &' >/dev/null
TEAM_URL="$(api POST "/v1/sandboxes/$PREVIEW/publish" '{"port":8001,"visibility":"team"}' | jq -er .url)"
published+=("$TEAM_URL")
before="$(guest "$PREVIEW" wc -l /tmp/http.log | awk '{print $1}')"
code="$(status GET "$TEAM_URL")"
[ "$code" = "401" ] || [ "$code" = "403" ] || fail "the team preview answered $code without a session"
after="$(guest "$PREVIEW" wc -l /tmp/http.log | awk '{print $1}')"
[ "$before" = "$after" ] || fail "the unauthenticated request reached the guest"
step "team preview challenges, guest untouched"

# 17. A control path on the preview host is never routed.
[ "$(status GET "${PUBLIC_URL%/}/v1/sandboxes")" = "404" ] || fail "a control path was routed on the preview host"
step "control path 404 on the preview host"

# 18. Destroy everything. Every host resource and every hostname goes.
for id in "${FORKS[@]}"; do
  api DELETE "/v1/sandboxes/$id" >/dev/null
done
api DELETE "/v1/sandboxes/$SOURCE" >/dev/null
for id in "${snapshots[@]}"; do
  api DELETE "/v1/snapshots/$id" >/dev/null
done
api DELETE "/v1/templates/$TEMPLATE" >/dev/null
sandboxes=()
snapshots=()
live="$(api GET /v1/sandboxes | jq '[.[] | select(.destroyed_at == null)] | length')"
[ "$live" = "0" ] || fail "$live sandbox rows survived"
if pgrep firecracker >/dev/null 2>&1; then fail "firecracker processes survived"; fi
if ip -o link show 2>/dev/null | grep -q 'kiln-'; then fail "TAP devices survived"; fi
if nft list chains 2>/dev/null | grep -q 'kiln_'; then fail "nftables chains survived"; fi
if grep -q " $ROOT" /proc/mounts 2>/dev/null; then fail "mounts survived"; fi
if [ -n "$(ls -1 "$ROOT/sandboxes" 2>/dev/null)" ]; then fail "sandbox directories survived"; fi
for url in "${published[@]}"; do
  [ "$(status GET "$url")" = "404" ] || fail "a retired hostname still answers: $url"
done
step "zero leaks, retired hostnames 404"

printf 'demo: PASS (%d steps)\n' "$pass"
