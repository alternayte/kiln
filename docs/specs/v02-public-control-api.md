# v0.2.0 public control API

## What it does
A gateway serves the Kiln API on the public internet. Each caller holds an API key or an OAuth token that names one tenant, and the host filters every row by that tenant. An agent drives the same API through MCP. TypeScript and Python SDKs come from one OpenAPI document. A self-hosted install runs one default tenant and never sees tenant management.

## Decisions
- The gateway is a second binary in this repo, `cmd/kiln-gateway` — the public surface changes often, and the host holds live microVMs that a deploy must not disturb.
- The gateway owns tenants, keys, OAuth, rate limits and the audit log in its own store — the host keeps one job, and a gateway deploy never migrates the host database.
- The gateway store is SQLite or Postgres behind one interface — `auth-all` already ships both, and a hosted service needs Postgres when it runs a second gateway.
- `alternayte/auth-all` provides organizations as tenants, org-scoped API keys, roles, rate limits and the audit trail — the library holds this behaviour already, and a second copy drifts.
- An `auth-all` plugin adds the OAuth 2.1 authorization server: discovery, JWKS, userinfo, authorization code with PKCE S256, refresh with rotation and replay detection, client credentials, revocation, introspection, resource indicators, and RFC 7591 and 7592 dynamic client registration — an MCP client registers itself and cannot use a static key.
- The gateway serves `/.well-known/oauth-protected-resource` and the `WWW-Authenticate` challenge — the resource server half belongs to the gateway, not the library.
- API keys and client credentials tokens both work for machines — a quickstart needs one line in a `.env`, and a service needs a short-lived token.
- Every row a caller reaches carries `tenant_id`, and every host query filters by it — a gateway bug must not reach another tenant's sandboxes.
- The gateway sends `X-Kiln-Tenant` with every call, and the host rejects a call with no tenant — the gateway names the caller, and the host decides what that tenant reaches.
- The gateway authenticates to the host with mTLS against a private CA that `kiln init` creates, plus a rotatable host token — a certificate proves which service calls, and a network address does not.
- The host control listener never binds a public address — an auth bypass here runs a stranger's code as root.
- The host enforces per-tenant caps on running sandboxes, templates and total snapshot bytes, and returns `exhausted` — only the host knows what runs now, and one tenant must not fill the box.
- The gateway proxies every `/v1` route with the same paths, bodies and error codes, and streams exec output, files and events without buffering — one API shape keeps the SDKs, the MCP tools and the docs in step.
- An MCP server at `/mcp` over Streamable HTTP exposes one tool per API operation — an agent drives sandboxes without an SDK.
- The gateway publishes an OpenAPI 3.1 document, and the build fails when it does not match the routes — the SDKs, the MCP tools and `llms.txt` come from it.
- The TypeScript and Python SDKs are generated, with a handwritten layer for streaming exec, file transfer, retries and a sandbox object — generators handle types well and streams badly.
- The gateway serves `llms.txt`, `llms-full.txt` and `/.well-known/ai-catalog.json` — an agent finds the MCP endpoint and the docs without a human.
- Every error carries a stable code, one plain sentence and the next call to make — an agent recovers without a human reading the message.
- A viewer belongs to one tenant, and the ingress matches the session tenant to the published hostname's tenant — the staff of one tenant must not read the preview of another tenant.
- The host keeps its operator token for local `kiln ctl` use, and that token acts on every tenant — the operator repairs the box without the gateway.
- The current state is tagged `v0.1.0`, and this work ships as `v0.2.0-rc.N` before `v0.2.0` — no tag claims stability before the gateway serves real traffic.
- `kiln ca issue --name <gateway>` prints the CA certificate, the client certificate and the client key, and `kiln ca revoke --serial <n>` cuts one certificate off — the operator pastes three values, and a stolen certificate dies without a new CA.
- `kiln gateway-env` prints one ready-to-paste block with the host address, the host token and those three PEM values — a setup that takes one command and one paste is a setup people finish.
- The gateway reads its credentials from the environment or from files — a credential store such as Coolify holds either form.
- The API path stays `/v1` — the routes do not break.

## Out
- No billing, no usage metering and no per-second accounting. The `events` table records create and destroy, so metering derives later.
- No sign-up flow and no hosted UI. The hosted service calls the operator API of the gateway.
- No browser-side SDK and no framework packages. A key that creates sandboxes never reaches front-end code.
- No multi-host scheduling. One gateway serves one host.
- No DPoP, no device flow, no back-channel logout, no `private_key_jwt`, no pairwise subjects.
- No gRPC. The API stays HTTP and JSON.

## How I know it works
- Two tenants exist. Tenant A's key lists, reads, execs, publishes and deletes only tenant A's rows. Every call with tenant B's id returns 404, and the host logs the refusal.
- A call with no tenant, an expired key, a revoked key or a token whose audience is another resource returns 401 and never reaches the host.
- An MCP client registers itself, completes the authorization code flow with PKCE, lists the tools, creates a sandbox and reads its exec output as it runs.
- A refresh token used twice is refused, and the tenant's audit log shows the replay.
- `curl` against the gateway with an API key runs `POST /v1/sandboxes/{id}/exec` with `Accept: text/event-stream` and prints output before the command ends.
- A tenant at its running-sandbox cap gets `exhausted`, and other tenants still create sandboxes.
- The gateway refuses to start when its client certificate is missing, and the host refuses a connection that carries no certificate.
- A viewer of tenant A gets a login challenge on tenant B's team preview, and the guest never sees the request.
- The generated TypeScript and Python quickstarts create a sandbox, exec a command, publish a port and destroy the sandbox.
- `llms.txt`, `/.well-known/ai-catalog.json` and the OpenAPI document are fetchable, and the OpenAPI check passes in `just check`.
- `kiln gateway-env` output pasted into a fresh gateway deployment is enough to start the gateway and create a sandbox. No other file or flag is needed.
- A gateway whose certificate is revoked cannot connect, and the host logs the serial.
