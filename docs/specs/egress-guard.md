# Egress guard

Closes alternayte/kiln#3.

## What it does
All outbound port 443 traffic of a sandbox passes through a host process, the egress guard. The guard reads the TLS hostname and enforces `egress_allow` on that name. The host records each destination of a sandbox, with counts and bytes, and a caller reads this record. A caller learns an allow list from refusals on any sandbox, or from `observe` mode on trusted code.

## Decisions
### Enforcement
- The guard reads the TLS SNI and refuses a name outside `egress_allow` — the current rule accepts a resolved IP and port 443 (`internal/network/network.go:293`), so a sandbox reaches every site on a shared CDN address.
- A connection with no SNI is refused — the guard cannot name its destination.
- The guard resolves the allowed name itself and dials that address, not the address the guest chose — a guest otherwise sends an allowed SNI to its own server.
- The guard refuses a name that resolves to a private, loopback, link-local or metadata address — a public name must not reach the host network.
- The guard forwards bytes for a name with no credential binding and never decrypts them — only `docs/specs/credential-injection.md` terminates TLS.
- Other outbound ports and UDP 443 stay blocked — the guard cannot name their destination.
- Domain fronting on a name with no binding stays possible, and the docs state it — the guard does not read the encrypted `Host` header.

### Process
- `kilnd egressguard` runs once per sandbox as a child process, like `snapfault` — `systemctl restart kilnd` must not cut an open model call.
- `Attach` starts the guard, `Adopt` reconnects to it, and the reconciler kills a guard that no sandbox row owns — a crashed daemon leaks nothing.
- nftables redirects guest port 443 to the guard of that sandbox — the guest cannot choose another path.
- A dead guard refuses port 443, an event records the exit, and the daemon starts a new guard — egress fails closed.
- A sandbox that a daemon adopts with no guard keeps its current rule until its next wake — an upgrade must not cut a running sandbox.
- `PUT /v1/sandboxes/{id}/egress` also makes the guard cut every open connection — a narrowed list must apply at once.

### Egress record
- SQLite holds one row per sandbox and destination: `host` or IP, `port`, `decision`, `count`, `bytes_out`, `bytes_in`, `first_seen`, `last_seen` — a row per connection or packet is too many writes.
- `decision` is `allowed`, `denied`, `injected` or `observed`. `allowed` means a connection reached that name — a resolved name proves nothing.
- The resolver records refused names, the guard records connections, and a rate-limited nftables log records refused direct-IP connections — a hostile process skips DNS.
- The guard sends records to the daemon and buffers a bounded amount during a restart. A full buffer counts the dropped records, and the read route returns the count — a gap must be visible.
- `GET /v1/sandboxes/{id}/egress/log` returns the rows, with the same scope as a sandbox read — Factoree reads what it cannot capture inside an untrusted guest.
- Rows stay for 7 days after the sandbox is destroyed — a caller reads a check sandbox after it is gone.
- A fork or a restore starts with no rows — each copy has its own record.

### Learning
- The first refusal per destination in a sandbox emits an `egress.denied` event — a caller approves live with `PUT /egress`, and a repeated refusal cannot flood the stream.
- Kiln holds no approval queue — the caller owns that decision.
- `PUT /egress` takes `{"observe": true}` in place of `egress_allow`. Any TLS name on port 443 passes, any name resolves, and rows take the decision `observed` — this finds a large list in one run.
- `observe` keeps the private address refusal and the metadata refusal — trusted code still must not reach the host network.
- `observe` returns `conflict` on a secret-bearing sandbox or a sandbox with credential bindings — open egress and a credential must not share a sandbox.
- Fork and snapshot return `conflict` while a sandbox observes — a widened sandbox must not become an origin.
- A `PUT` with `egress_allow` ends `observe` and cuts outbound connections — this is the same call as any policy change.
- The tenant caps gain `allow_observe`, true by default — an operator refuses this mode per tenant.
- The event log and the gateway audit log record each change of mode — a widened sandbox must leave a trace.
- `kiln egress <id> --propose` prints the current list plus refused and observed names as exact names, and applies nothing — a guessed wildcard widens a list with nobody noticing.

### Contract
- `internal/apispec`, the SDKs, the MCP tools and `kiln` carry all of the above — the API contract states what the host accepts.
- `kiln egress <id>` prints the egress record — `docs/specs/agent-cli.md` needs this route, and the route starts here.

## Out
- URLs and bodies in the egress record of a name with no binding.
- The name of the guest process that made a request. The host cannot see it, and a guest report can be forged.
- OpenTelemetry export.
- A byte budget. The record reports bytes, and the caller decides.
- Signed records.

## How I know it works
- `just gate sec` runs each attack from a sandbox that allows one name behind a shared CDN address, and each is refused: another SNI on that address, no SNI, a direct IP, UDP 443, an allowed SNI sent to a guest-chosen address, and a name that resolves to `169.254.169.254`.
- `curl https://<allowed name>` works, and the guard log shows no decrypted bytes.
- After `systemctl restart kilnd`, a download in progress through the guard finishes.
- `kill -9` on a guard makes port 443 fail, an event appears, and a later `curl` works.
- `GET .../egress/log` shows `allowed` and `denied` rows with counts and bytes. A forked copy returns no rows.
- A destroyed sandbox returns rows for 7 days and returns `not_found` after.
- `/v1/events` shows one `egress.denied` after 100 refused attempts to one name.
- In `observe`, `curl` to any public site works, and `--propose` prints that name. A fork of that sandbox returns `conflict`.
- `observe` on a sandbox with `env` returns `conflict`.
