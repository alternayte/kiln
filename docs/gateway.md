# The gateway

`kilnd serve` alone is one operator, one token, on one box, reachable from
that box. The gateway is what the rest of the world talks to. It holds
tenants, API keys, members, viewers, OAuth, rate limits and an audit log,
serves the web UI with a terminal into each sandbox, serves the same
operations to agents over MCP, and forwards every `/v1` call to the host with
the tenant's name on it. The host filters every row by that name.

You need it when more than one person or program reaches the host, when any
of them is not on the box, or when an agent should drive sandboxes over MCP.
A single operator on one box with `kiln` and `curl` does not.

## What runs where

| Piece | Runs | Holds |
|---|---|---|
| `kilnd serve` | On the KVM host, as root, under systemd | The microVMs, `kiln.db`, the operator token |
| `kiln-gateway` | Anywhere with a public address, as an image | Its own database: tenants, keys, members, sessions, OAuth, audit |

The two are separate on purpose. The gateway changes often and deploys like
any web service; the host holds live microVMs that a deploy must not disturb.

## Deploy

The image is `ghcr.io/alternayte/kiln-gateway:<tag>`, with the web UI built
in. It is distroless: no shell, no package manager. It listens on
`KILN_GATEWAY_ADDR`, `0.0.0.0:8080` in the image, and applies its own
migrations at start.

| Variable | Required | Meaning |
|---|---|---|
| `KILN_GATEWAY_URL` | yes | The public URL people and agents use, for example `https://api.example.com`. It is the OAuth issuer and the origin the session cookie trusts. |
| `KILN_OAUTH_KEY` | yes | 32 bytes, base64, from your credential store: `openssl rand -base64 32`. It wraps the OAuth signing keys at rest. A new key on every restart would invalidate every token issued. |
| `KILN_HOST_URL` | yes | The host. `https://<name>:8443` through the mTLS listener, or `http://127.0.0.1:8080` on the same box. |
| `KILN_HOST_TOKEN` | yes | The host's operator token, from its `config.json`. |
| `KILN_CA_CERT`, `KILN_CLIENT_CERT`, `KILN_CLIENT_KEY` | for an https host | The three PEM values `kilnd gateway-env` prints. Each accepts a PEM block, the PEM as base64, or `\n` escapes, and `<NAME>_FILE` names a file instead. |
| `KILN_OPERATOR_EMAIL`, `KILN_OPERATOR_PASSWORD` | first start | The first operator. Read at every start: a missing operator is created, the role is repaired, and the password is set when one is given. Remove both afterwards. |
| `KILN_DB` | no | The SQLite file, `file:/data/kiln-gateway.db` in the image. Mount `/data`. |
| `KILN_DATABASE_URL` | no | A Postgres DSN. Set, it replaces SQLite, which a second gateway needs. |
| `KILN_GATEWAY_ADDR` | no | The listen address. |
| `KILN_OPERATOR_ROLE` | no | The role that administers tenants, `operator`. |
| `KILN_LOGIN_PATH`, `KILN_CONSENT_PATH` | no | The UI routes the OAuth flow sends a browser to, `/sign-in` and `/consent`. |
| `KILN_SKIP_MIGRATE` | no | Set to anything to skip the migration at start. `kiln-gateway migrate` runs it alone. |

The gateway refuses to start without the host URL, the host token, or, for
an https host, any of the three PEM values. That is on purpose: it never
runs in a state where it cannot reach the host.

### The host on the same box

A gateway beside the host dials the loopback control listener and needs no
certificate:

```sh
docker run -d --name kiln-gateway --network host \
  -v kiln-gateway:/data \
  -e KILN_GATEWAY_URL=https://api.example.com \
  -e KILN_OAUTH_KEY="$(openssl rand -base64 32)" \
  -e KILN_HOST_URL=http://127.0.0.1:8080 \
  -e KILN_HOST_TOKEN="$(sudo jq -r .bearer_token /var/lib/kiln/config.json)" \
  -e KILN_OPERATOR_EMAIL=you@example.com \
  -e KILN_OPERATOR_PASSWORD='at least eight characters' \
  ghcr.io/alternayte/kiln-gateway:v0.10.0
```

`--network host` is what lets it reach `127.0.0.1:8080`. Put a TLS
terminator in front of port 8080, and read
[what it must allow](../infra/README.md#serving-the-gateway-behind-something-else):
the terminal is a WebSocket, and it needs HTTP/1.1 end to end.

### The host on another machine

On the host, add the mTLS listener to `config.json`, restart, and open the
port. Then print the gateway's credentials:

```sh
sudo kilnd gateway-env --name production --host kiln.example.com:8443
```

The steps are in [infra/README.md](../infra/README.md#the-gateway-listener).
Paste the block into the credential store of wherever the gateway runs. The
image on Coolify, Fly, a plain `docker run` or Kubernetes all read the same
variables.

## First sign-in

1. Open `KILN_GATEWAY_URL` and sign in as the operator.
2. Create a tenant on the tenants screen. The same call creates it on the
   host, so the two never drift. `POST /operator/tenants` with the session
   does the same from a script.
3. Switch to the tenant and create an API key on the keys screen. The
   plaintext appears once. It starts with `kiln_` and names that tenant
   alone.
4. Call the API with it:

```sh
curl -sS https://api.example.com/v1/health -H "Authorization: Bearer kiln_..."
```

A tenant's operator invites members by email. `editor` manages sandboxes,
templates, keys, members and viewers; `reader` sees the lists and runs
nothing. There is no public sign-up.

## What the gateway serves

| Path | What |
|---|---|
| `/` | The web UI |
| `/v1/*` | The host API, same paths, bodies and errors, forwarded with the tenant |
| `/mcp` | The same operations as MCP tools, Streamable HTTP, `POST` only |
| `/api/auth/*` | Sign-in, sessions, keys, members, OAuth: the auth-all routes |
| `/api/me` | The caller's tenant and role |
| `/operator/tenants` | Tenant administration, operator only |
| `/openapi.json`, `/llms.txt`, `/llms-full.txt`, `/.well-known/ai-catalog.json` | The contract and the agent documentation, no credential |
| `/.well-known/oauth-authorization-server`, `/.well-known/oauth-protected-resource`, `/.well-known/jwks.json` | OAuth discovery |
| `/healthz` | `{"ok":true}` |

A `/v1` call with no credential answers 401 with a `WWW-Authenticate`
header that names the authorization server, which is how an MCP client
finds it.

## Agents

An agent with an API key sends it as a bearer credential to `/mcp` and to
`/v1`. An MCP client without one registers itself (RFC 7591), completes the
authorization code flow with PKCE in a browser, and holds a token for the
`sandbox` scope; `offline_access` adds a refresh token. Every tool is one
API operation with the same fields. `writeFile` takes `content`; `events`
answers with the events that arrived inside a short window, and takes
`after` and `sandbox_id`.

## Developing the UI

The UI lives in `web/` and is embedded in the gateway binary at build time.
`bun run dev` serves it on `localhost:5173` and forwards `/v1`, `/api` and
`/operator` to a gateway on `localhost:8080`, so the session cookie stays
same-origin. Run that gateway from source:

```sh
export KILN_GATEWAY_URL=http://localhost:5173
export KILN_OAUTH_KEY="$(openssl rand -base64 32)"
export KILN_HOST_URL=... KILN_HOST_TOKEN=...          # and the PEM values for an https host
export KILN_OPERATOR_EMAIL=dev@example.com KILN_OPERATOR_PASSWORD='development password'
go run ./cmd/kiln-gateway
```

`KILN_GATEWAY_URL` names the dev server, not the gateway, because that is
the origin the browser sees. The UI's API client is generated from
`sdk/openapi.json`; after a route change, run `just sdk` and then
`bun run generate` in `web/`, and commit both.
