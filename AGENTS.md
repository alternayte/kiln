# Kiln

## What this is
Kiln runs isolated Linux microVMs on one host, built from OCI images and forked into copies. The build authority is `docs/specs/`. Phases P0 to P6 are built; `LOG.md` records what is done.

## Run
On a Linux host with KVM, as root: `go run ./cmd/kilnd init`, then `go run ./cmd/kilnd serve`.
The server and the systemd unit: `infra/README.md`.

## Test
`just check` runs every check in `checks/` and is the gate.
`just check-slow` runs the read-only commands this file quotes.
`just gate P1` runs one phase gate; it needs root and KVM. `just demo` is the v1 done-line.

## Stack rules
- Isolation is a microVM with its own kernel, launched under jailer. No host containers.
- Storage is one SQLite file. No Postgres.
- One host, one binary, systemd. No Kubernetes, no clustering.
- Three interfaces are permitted: runtime.Runtime, snapshot.PageFaultSource, store.Store. No others.
- `docs/internal/sdd.md` is frozen. Never edit it to match code.

## Domain words
- sandbox: a Firecracker microVM created from a template snapshot. Avoid: container, instance.
- template: an OCI image built once into a rootfs plus memory and state, from which sandboxes restore. Avoid: base image.
- snapshot: a memory and state copy that restores into one or more sandboxes. Avoid: backup, checkpoint.
- fork: one snapshot restored into several new sandboxes. Avoid: clone.
- sleep: snapshot a sandbox and stop it while keeping its id and published hostnames. Avoid: suspend, hibernate.
- publish: serve a guest port on a public hostname. Avoid: expose, port-forward.
- tenant: one customer of a Kiln install; it owns templates, sandboxes, snapshots, published hostnames and viewers. Avoid: org, account, workspace, customer. The auth-all API names the type Organization, so that word alone stays legal.
- gateway: the public binary that holds tenants, keys, OAuth, rate limits, the audit log and MCP, and proxies /v1 to the host. Avoid: proxy, control plane, edge.
- host: the machine and daemon that run microVMs and own the sandbox data. Avoid: node, worker, backend.
- API key: a long-lived machine credential that names one tenant and its scopes. Avoid: secret, bearer token.
- viewer: a login that opens team previews for one tenant and nothing else. Avoid: preview user.
- operator: the person who runs the host, holds the operator token and repairs the box. Avoid: owner. The auth-all API names a plugin admin, so that word alone stays legal.
- reader: a member of a tenant who sees its templates, sandboxes and snapshots and changes nothing. Avoid: read-only member.
- terminal: an interactive PTY session into a running sandbox, carried over a WebSocket. Avoid: shell session, console, SSH.
- preview environment: one template and one sandbox built from one commit, published on a hostname for a pull request. Avoid: preview deployment, ephemeral environment, review app.
- start command: the command a template names to run its application, which the host runs once when a sandbox restores from that template snapshot. Avoid: entrypoint, run command.
- service: an image that runs beside the application in a template's microVM, started before the template snapshot. Avoid: sidecar, dependency.
- seed: the commands that run after every service is ready and before the template snapshot. Avoid: fixture, bootstrap.
- egress guard: the per-sandbox host process that reads the TLS hostname of outbound port 443 traffic and enforces the allow list. Avoid: egress proxy, sidecar proxy.
- egress record: the host's summary of each destination a sandbox reached or was refused. Avoid: network log, flow log.
- credential binding: one credential tied to one exact host of one sandbox, which the egress guard injects as a header. Avoid: secret mapping, injection rule.
- timeline point: a marked memory, process and disk state of one sandbox that a caller rewinds to or forks from. Avoid: savepoint.
- viewer fork: the fork of a published preview environment that belongs to one viewer login. Avoid: visitor copy, session sandbox.
