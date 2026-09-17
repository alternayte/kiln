#!/usr/bin/env bash
# Build this commit into a template, run one sandbox from it, publish the
# application port, and print the hostname.
#
# curl talks to the gateway, because the API is the product. python3 builds
# and reads the JSON; it is on every runner and needs no build step, and a
# shell that assembles JSON by hand breaks on the first quote in a setup
# command.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -X "$method" -H "Authorization: Bearer $KILN_API_KEY" -w $'\n%{http_code}')
  if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  curl "${args[@]}" "$KILN_URL$path"
}
status() { tail -n1 <<< "$1"; }
payload() { sed '$d' <<< "$1"; }

# Caller errors fail before the template builds. This also masks every env
# value, so no later line of the log shows one.
python3 "$here/payload.py" check

if [ "$FORK" = "true" ] && [ -n "$(tr -d '[:space:]' <<< "$ENV_LINES")" ]; then
  echo "this pull request comes from a fork, so env is not sent to its preview"
fi

short="${SHA:0:7}"
name="pr-${PR}-${short}"

echo "building template $name from $IMAGE"
body="$(NAME="$name" python3 "$here/payload.py" template)"
out="$(api POST /v1/templates "$body")"
if [ "$(status "$out")" != "202" ]; then
  echo "the template was refused: $(payload "$out")" >&2
  exit 1
fi

# Wait for the build. A failure prints the events: a silent preview failure
# wastes the reviewer's time, not the author's.
state=building
for _ in $(seq 1 180); do
  sleep 10
  out="$(api GET "/v1/templates/$name")"
  [ "$(status "$out")" = "200" ] || continue
  state="$(payload "$out" | python3 "$here/payload.py" get state)"
  [ "$state" = "building" ] || break
done
if [ "$state" != "ready" ]; then
  echo "the template did not build: state=$state" >&2
  payload "$out" | python3 "$here/payload.py" get error >&2 || true
  echo "--- events" >&2
  payload "$(api GET /v1/events)" >&2 || true
  exit 1
fi

body="$(NAME="$name" python3 "$here/payload.py" sandbox)"
out="$(api POST /v1/sandboxes "$body")"
if [ "$(status "$out")" != "201" ]; then
  echo "the sandbox was refused: $(payload "$out")" >&2
  exit 1
fi
sandbox="$(payload "$out" | python3 "$here/payload.py" get id)"

# port goes first: the first published port of a sandbox gets the short
# hostname, and every other port shares its stem.
published=""
for p in "$PORT" $(python3 "$here/payload.py" ports); do
  body="$(python3 "$here/payload.py" publish "$p")"
  out="$(api POST "/v1/sandboxes/$sandbox/publish" "$body")"
  if [ "$(status "$out")" != "201" ]; then
    echo "port $p was refused: $(payload "$out")" >&2
    exit 1
  fi
  u="$(payload "$out" | python3 "$here/payload.py" get url)"
  published+="$p $u"$'\n'
  echo "port $p at $u"
done
url="$(head -n1 <<< "$published" | cut -d' ' -f2-)"
urls="$(python3 "$here/payload.py" urls <<< "$published")"

echo "url=$url" >> "$GITHUB_OUTPUT"
echo "urls=$urls" >> "$GITHUB_OUTPUT"
echo "preview at $url"

# The previous preview of this pull request goes last, so the comment never
# names a hostname that has already been retired.
KEEP="$name" "$here/prune.sh" || true
