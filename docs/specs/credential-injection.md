# Credential injection

Closes alternayte/kiln#21. Needs `docs/specs/egress-guard.md`.

## What it does
A caller binds a credential to one host of one sandbox. A request from that sandbox to that host leaves with the header that Kiln sets, and the value never enters the guest. A sandbox with only bindings forks and restores with no `allow_secret_fork`. Each copy gets only the bindings its caller names.

## Decisions
### Bindings
- `PUT /v1/sandboxes/{id}/credentials` takes a list of `name`, `host`, `header`, `value`, and optional `path` and `methods`, and replaces the whole list — this matches `PUT /egress`, and a caller sets the full policy in one call.
- The value travels inline, and Kiln has no named store — Factoree and GitHub already hold the value, and a second vault adds a concept. A rotation is a new `PUT`.
- `GET /v1/sandboxes/{id}` returns each name, host, header, path and methods, and never the value — a caller must see the policy and nothing else.
- `host` is one exact name — a wildcard sends a key to any subdomain.
- `egress_allow` must cover `host`, or the call returns `invalid`. A `PUT /egress` that removes a bound host returns `conflict` — a binding must never point at a closed host.
- A value with a control character returns `invalid` — it can split a header.
- A sandbox holds at most 16 bindings — each one adds a leaf certificate and a guard rule.
- The call needs the same write scope as exec — a reader changes nothing.
- A sleeping sandbox takes the change without a wake — a policy change needs no running guest.

### Injection
- The header that Kiln sets replaces a header of the same name from the sandbox — a guest must not choose the credential.
- `path` is a prefix and `methods` is a list, and both narrow when the key is sent — a GitHub key bound to `/repos/acme/x/` cannot act on another repository.
- A request that does not match leaves with no key and with that header removed. `egress_allow` stays the only access control — a matcher that also blocks is a second policy.
- Kiln normalizes the path before the match, and sends no key on `..` or an encoded `/` — otherwise `/repos/acme/x/../../other` passes a prefix check.
- The guard sets `Host` to the bound host — a request must not reach another site with the key.
- The guard does not follow a redirect — a redirect to another host must not carry the key.
- The docs state that a provider-scoped key is stronger than a path prefix — provider routing can differ from the prefix.
- Kiln does not scrub responses, and the docs state it — a scrub misses an encoded value and gives false comfort. A caller binds no host that echoes headers.

### Trust
- The guard terminates TLS only on a bound host — all other traffic stays undecrypted.
- Each tenant has one CA. Its private key is sealed in SQLite with `seal_key` and never leaves the daemon — a tenant's trust must not extend to another tenant.
- The daemon issues one leaf certificate per bound host and sends the guard the leaf key — a compromised guard cannot issue a certificate for any other name.
- A template build appends the public CA certificate to the system trust store of the application root — snapshots and forks then already trust it.
- Kilninit sets `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE` — these common clients read those variables and not always the system store.
- A template built before this spec returns `conflict` on `PUT /credentials`, and the message says to rebuild — its guests do not trust the CA.
- A client with a pinned certificate or a fixed root list fails on a bound host, and the docs state it — that caller keeps `env`.

### Storage and lifecycle
- The row is sealed with `seal_key` and belongs to one sandbox. The daemon sends the opened value to the guard of that sandbox over a Unix socket — a guard holds only its own sandbox's values, in memory only.
- A binding survives a sleep, a wake and a daemon restart — it belongs to the sandbox row.
- A fork or a restore starts with no bindings, and each call takes an optional `credentials` list — a check sandbox gets no key unless its caller gives one, and removing one fork's key leaves the others.
- A snapshot holds no binding — the value never reaches guest memory.
- A sandbox with only bindings is not secret-bearing — this is the purpose of the feature.

### Record
- Each injected connection adds to an `injected` row in the egress record, with the binding name — the summary shows that a key was used.
- A bound host also gets a per-request log of method, host, path with no query, status, binding name and time, at most 10,000 rows per sandbox with a dropped count — the guard already reads these requests, and credential use is what a caller audits.
- `GET /v1/sandboxes/{id}/egress/requests` returns that log, with the same scope and 7 day retention as the egress record — a caller reads it after the sandbox is gone.
- No header value and no body reaches a log, an event or the audit log — this includes the gateway request log.

### Contract
- `internal/apispec`, the SDKs and the MCP tools carry all of the above — the API contract states what the host accepts.
- The docs show a placeholder key for a CLI that refuses to start without one — #21 reports that opencode needs this.

## Out
- A named store of values.
- A placeholder swap in a body or a query string.
- A callback for short-lived tokens. A caller sends a new `PUT`.
- A spend budget per fork. The provider or the caller owns usage.
- CB4A leases and DPoP. The sandbox holds no token to protect.
- A Factoree adapter change, which belongs to the Factoree repository.

## How I know it works
- `env` and `cat /proc/*/environ` in the sandbox do not show the value, and `grep` over the SQLite file does not find it.
- `curl https://api.deepseek.com/models` with no auth header returns 200.
- A sandbox `Authorization` header to that host is replaced.
- A request to another allowed host leaves with no injected header.
- A binding with `path: /repos/acme/x/` sends the key on that path. It sends no key on `/repos/acme/x/../../other` or on `/repos/other`.
- A redirect from the bound host to another allowed host carries no key.
- A snapshot restores with no `allow_secret_fork`. Its copy gets 401 from the provider until a `PUT` binds the key.
- Two forks get different keys, and removing one fork's binding leaves the other working.
- After a sleep, a wake and `systemctl restart kilnd`, the binding still works.
- `egress/requests` shows method, path and status, and shows no query string and no header value.
- `PUT /egress` that removes the bound host returns `conflict`.
- `PUT /credentials` on a template built before this spec returns `conflict` and says to rebuild.
