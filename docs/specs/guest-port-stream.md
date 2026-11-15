# Guest port stream

## What it does
A client that is not on the host opens a raw TCP stream to any port of a sandbox through the gateway. The stream is a WebSocket on the control API, and binary messages carry the bytes. The client runs its own TLS inside the stream, so the guest's certificate reaches the client and the host never sees plaintext. A daemon such as Fettle's agentd is reachable with the same API key and tenant checks as every other route (#32).

## Decisions
- `GET /v1/sandboxes/{id}/ports/{port}/stream` upgrades to a WebSocket. A binary message carries bytes in each direction — Traefik in front of the gateway forwards a WebSocket and does not document `CONNECT`.
- The route needs `sandbox:exec`, the scope of the terminal — a reader must not reach a process in the sandbox.
- The gateway checks the role on every `/v1` route and MCP tool, not on the stream alone. Each apispec operation names its permission, and a refusal is 403 before the host sees the call — the gateway checked no role before, so a reader could exec.
- Any port from 1 to 65535 is allowed, and the port does not need to be published — `exec` already reaches every port, so the stream grants nothing new.
- The host dials the guest port before the upgrade. A missing sandbox, a refused tenant or a port with no listener answers with a normal HTTP error and no upgrade — the client reads why it failed.
- A sleeping sandbox wakes when a stream opens, as it does for the terminal — the caller does not track sleep.
- The stream does not hold the sandbox lock. Bytes in either direction touch the idle timer, at most once every 10 seconds — an open but silent stream lets the sandbox sleep, and DELETE and the TTL always work.
- Sleep, destroy and a timeline rewind close every open stream of the sandbox first. The host closes each WebSocket with status 1001 — a TCP connection does not survive a memory restore, and the client reconnects, which wakes the sandbox.
- When either end closes, the whole stream closes. There is no half-close — a WebSocket has no half-close.
- A text message closes the stream with status 1003 — one frame type means one meaning.
- The gateway records one audit entry per stream, with the duration of the stream — the existing forward path records after the stream ends.
- The operation is in `internal/apispec` with the WebSocket flag, so the API docs list it and MCP leaves it out, like the terminal — MCP carries no stream.
- `kiln stream <id> <port>` connects stdin and stdout to the stream — `socat` and a ProxyCommand carry any client over it.
- The release is v0.17.0 — this is a new route, not a fix.

## Out
- No TLS passthrough mode on the ingress, and no change to the `:443` listener.
- No HTTP `CONNECT`.
- No stream helper in the TypeScript or Python SDK. Neither SDK wraps the terminal, and any WebSocket client works.
- No local listener in the CLI, like the one kubectl opens.
- No wake limit on stream opens. The terminal has none.

## How I know it works
- A sandbox whose start command serves HTTPS on port 7421 with a self-signed certificate: `socat TCP-LISTEN:7421 EXEC:'kiln stream <id> 7421'` on a laptop, then `curl --cacert guest.pem --resolve guest:7421:127.0.0.1 https://guest:7421/v1/health` returns the health response, and `openssl s_client` through the same bridge shows the guest certificate.
- With a reader's API key, the same route returns 403 and no upgrade.
- A port with no listener returns an HTTP error and no upgrade.
- A stream that stays silent for longer than the idle time ends with 1001, the sandbox is `sleeping`, and a new stream wakes it.
- `DELETE /v1/sandboxes/{id}` with an open stream returns at once and the stream ends with 1001.
- The gateway audit log shows one entry for the stream, with its duration.
