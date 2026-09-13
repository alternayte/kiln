# Kiln — Software Design Document

Self-hosted microVM sandboxes. One Go binary, one SQLite file, one VPS.

**This file is the whole specification.** There are no other design documents. Read it
in full at the start of every session. It is about 1000 lines and that is deliberate —
reading three of four documents and inventing the fourth is the failure this format
removes.

**This file is frozen.** A pre-commit hook rejects commits that touch it (§7.3). If the
design is wrong, say so and stop. Never edit this file so that it agrees with your
code. That failure is silent, it is inherited by every session after it, and it is the
single worst outcome available in this project.

> Name is a placeholder. `grep -rl kiln . | xargs sed -i 's/kiln/newname/g'` changes it.

## Contents

1. Goal — what is being built, and what done means
2. Architecture — components, interfaces, and the five hazards
3. API — the frozen HTTP surface
4. Data model — SQLite schema and on-disk layout
5. Security — threat model, egress policy, controls
6. Phases — P0 to P5, each with an exit gate
7. Verification — the two scripts, and how gates run
8. Session protocol — how an agent works here, and the build log

---

## 1. Goal

### One sentence

Kiln runs isolated Linux microVMs on one machine, creates them from OCI images in
under a second, and forks a running one into many copies.

### Why it exists

Three workloads, one mechanism:

| Workload | What it needs |
|---|---|
| Untrusted LLM-generated code | Create fast, run, discard |
| A coding agent session | Keep the filesystem and memory between calls |
| Parallel evals across branches | Many copies of one prepared environment |

All three are **snapshot and restore**. Create is a restore from a template snapshot.
Sleep is a snapshot. Fork is a restore performed N times. There is one primitive, and
lifecycle is a field on the sandbox, not a separate subsystem.

If the implementation ever grows a second code path for "ephemeral" versus
"persistent" sandboxes, the design has been misread.

### The done-line

v1 is done when this command succeeds, unmodified, on both the Lima VM on a Mac
laptop and on a Linux VPS:

```sh
just demo
```

`just demo` performs exactly this, and asserts each step:

1. Import `python:3.12-slim` from a registry and build a template.
2. Create a sandbox from that template. Assert wall time under the recorded baseline.
3. Exec `python -c "print(2+2)"`. Assert stdout is `4`.
4. Write `/work/marker` inside the sandbox.
5. Snapshot the sandbox.
6. Fork the snapshot into 8 sandboxes.
7. In each fork, read `/work/marker`. Assert all 8 see it.
8. In each fork, write a different value to `/work/marker`. Assert no fork sees
   another fork's value.
9. In each fork, read 16 bytes from `/dev/urandom`. Assert all 8 values differ.
10. In each fork, read the wall clock. Assert each is within 2s of host time.
11. From one fork, curl an allowlisted host. Assert success.
12. From one fork, curl a non-allowlisted host. Assert failure.
13. From one fork, curl `169.254.169.254`. Assert failure.
14. Start an HTTP server inside one fork and publish its port as `public`. Fetch the
    URL over HTTPS from outside with no credential. Assert the response came from that
    fork.
15. Let that fork sleep. Fetch the URL again. Assert it wakes and answers.
16. Publish a second port as `team`. Fetch with no session. Assert a login challenge,
    and assert from inside the guest that the request never arrived.
17. Fetch `https://<preview-host>/v1/sandboxes`. Assert 404.
18. Destroy everything. Assert zero leaked processes, TAP devices, mounts, files,
    database rows, and that every published hostname now 404s.

Steps 9, 10 and 13 are not decoration. They are the three failures that a snapshot
based system produces silently, and they are described in `docs/04-security.md`.

### What must be true

- A sandbox is a Firecracker microVM with its own kernel.
- A template is built from any OCI image reference and stored as a snapshot, not an
  image.
- Create restores a template snapshot. Sleep is a snapshot. Fork is a restore done N
  times, and the copies are independent. **(R-P-006)**
- A sandbox exposes exec, file read, and file write.
- A sandbox port can be published on the public internet over HTTPS, at a stable
  hostname, either behind an unguessable URL or behind a team login.
- A published sandbox that has slept restores when a request arrives for it. The
  caller waits; the caller does not manage it.
- Egress is default-deny with a per-template allowlist.
- All state survives a daemon restart, and killing the daemon leaks nothing.
- One binary, one SQLite file, no runtime dependency beyond Firecracker, a kernel
  image, and standard Linux tooling.
- The same binary and the same tests run on a laptop dev VM and on a VPS.

Hazard and control IDs (`H1`-`H5`, `C1`-`C7`) live in `docs/01-architecture.md` and
`docs/04-security.md`. Phase gates reference them. There is no coverage matrix — the
gates are the coverage.

### Non-goals for v1

Named so an agent does not build them:

- **No GPU support.**
- **No multi-tenancy of sandboxes.** Sandboxes have no owner. One operator controls
  them all through one token. Viewer accounts exist for `team` previews (§5 C6) and
  they grant nothing except the right to view a preview.
- **No public control plane.** `/v1/*` is not reachable from the internet in v1. That
  is v1.1 and it has its own phase. See C8.
- **No web UI.** A read-only status page at `/` is the maximum.
- **No billing or metering.**
- **No Kubernetes.**
- **No clustering.** One host. The interfaces in `docs/01-architecture.md` leave room
  for a second host later. Do not build for it now.
- **No second hypervisor backend.** Firecracker only. macOS parity comes from a Lima
  Linux VM, not from a port.
- **No image builder.** Kiln imports images that something else built. It does not
  contain BuildKit or parse Dockerfiles.

---

## 2. Architecture

### Shape

**Kiln runs directly on the host. It is not containerized, and there is no Dockerfile.**

It launches Firecracker under `jailer`, creates TAP devices, writes nftables rules,
mounts filesystems and opens `/dev/kvm`. Running that inside a container requires
`--privileged` with host networking and `/dev/kvm` passed through, at which point the
container provides no isolation and only makes the jailer chroot and the
cgroup-per-sandbox logic harder to reason about. The isolation boundary is one level
down: each sandbox is a real VM with its own kernel.

Deployment is one binary at `/usr/local/bin/kiln`, a systemd unit, and state under
`/var/lib/kiln`.

One binary, `kiln`, with subcommands:

| Subcommand | Role |
|---|---|
| `kiln serve` | API server, store, reconciler, VM supervisor, proxy |
| `kiln snapfault` | Userfaultfd page-fault server. One process per restoring VM. |
| `kiln ctl` | CLI client for the HTTP API |
| `kiln init` | First-run setup: directories, kernel fetch, nftables base, DB |

`kiln snapfault` is a hidden subcommand of the same binary, launched by `kiln serve`.
It is a separate **process** so that Go garbage collection pauses in the control plane
do not land in the page-fault path during restore. This is the reason. Do not merge it
back into the server process to "simplify".

A second guest binary, `kilninit`, is PID 1 inside every microVM. It is built from
this repo and baked into every rootfs.

### Packages

Package by feature. One directory per box below.

```
cmd/kiln/            main, subcommand wiring
internal/api/        HTTP handlers, request and response types
internal/store/      SQLite, migrations, queries
internal/template/   OCI pull, ext4 build, template snapshot creation
internal/sandbox/    lifecycle: create, exec, snapshot, fork, sleep, destroy
internal/runtime/    Firecracker process control, jailer, VM config
internal/snapshot/   snapshot files, UFFD server, page cache
internal/network/    TAP devices, address pool, nftables rules
internal/ingress/    public HTTPS proxy, hostname routing, certs, wake-on-request
internal/reconcile/  desired state versus host reality, sweeps
guest/kilninit/      guest PID 1, vsock server, resume hooks
```

### The three permitted interfaces

`AGENTS.md` forbids interfaces without two implementations. These three are exempt
because a second implementation is planned or because tests need a seam:

1. **`runtime.Runtime`** — start, pause, resume, snapshot, stop a VM. Firecracker is
   the only implementation in v1. A libkrun implementation may follow. This is the
   only place that knows Firecracker exists.
2. **`snapshot.PageFaultSource`** — serves guest memory pages during restore. Backed
   by `kiln snapfault` in v1. A future implementation may read from object storage.
3. **`store.Store`** — SQLite in v1. The seam exists so tests run against a temp DB,
   not so a second database can be added. Do not add one.

No other interfaces. If you feel the need for a fourth, say so and wait.

### Dependencies

This list is complete. Ask before adding.

- `modernc.org/sqlite` (pure Go, no cgo)
- `github.com/regclient/regclient` (OCI pull)
- `golang.org/x/sys/unix` (userfaultfd, mount, netlink helpers)
- `github.com/mdlayher/vsock` (host side of the guest channel)
- `github.com/caddyserver/certmagic` (ACME with DNS-01, for wildcard certificates)
- `github.com/alternayte/auth-all` (viewer login for `team` previews — operator's own
  library, and browser-shaped, which is exactly what preview viewers are)
- Standard library for HTTP, JSON and the proxy.

**Exception: libraries authored by the operator** (`alternayte/*`) are allowed without
asking. They are not third-party risk. Everything else on this list still applies.

External binaries at runtime: `firecracker`, `jailer`, `mke2fs`, `nft`, `ip`.
Pinned versions are recorded in `docs/03-data-model.md`.

### How a sandbox is created

```
POST /v1/sandboxes {template: "py312"}
  |
  v
sandbox.Create
  |-- store: insert row, state=creating
  |-- network: allocate TAP + /30, apply nftables chain for this sandbox
  |-- snapshot: open template memory file, start `kiln snapfault` with a UFFD
  |-- runtime: launch firecracker under jailer, load snapshot, resume
  |-- guest: kilninit resume hook runs (clock, entropy, hostname, vsock)
  |-- store: state=running
  v
201 {id, state: running}
```

Cold boot from a rootfs happens **only** during template build. Normal create never
boots a kernel.

### Disk model

Firecracker has no qcow2 and no copy-on-write disks. Forking is therefore done with
two drives and an overlay inside the guest:

- **Drive A** — the template rootfs, ext4, attached read-only, shared by every
  sandbox from that template. One file on the host, opened many times.
- **Drive B** — a per-sandbox writable ext4 file, sparse, created empty at
  create time.

`kilninit` mounts an overlayfs with A as lower and B as upper, then pivots into it.
A fork copies nothing. It allocates a new sparse Drive B and restores shared memory
pages copy-on-write through the UFFD server.

This is the reason fork is cheap. If someone proposes copying the rootfs per sandbox,
that is the design being lost.

### Memory model

A template snapshot is a Firecracker memory file plus a VM state file.

On restore, Kiln does **not** let Firecracker read the memory file directly. It
registers guest memory with userfaultfd and points Firecracker at the UFFD socket.
`kiln snapfault` serves pages from the template memory file on demand, and keeps
written pages private to that VM.

Consequences to hold on to:

- Eight forks of one 512 MB template do not use 4 GB. They share clean pages.
- The template memory file must stay open and unchanged while any child lives.
  Deleting a template while sandboxes are running must be refused.
- Restore latency is dominated by how many pages are faulted before the guest is
  useful, not by the file size.

### Networking

Every guest sees the **same** network configuration: address `172.31.0.2/30`, gateway
`172.31.0.1`. The difference between sandboxes lives entirely on the host side, in
which TAP device the VM is attached to and what the host does with that traffic.

This is deliberate. A restored snapshot carries the guest's network stack with it.
If guest addresses varied per sandbox, every restore would need to reconfigure
networking inside the guest, and every fork would race.

Host side, per sandbox: one TAP device, one nftables chain, source NAT to the host's
outbound address.

Egress policy is default-deny. See `docs/04-security.md`.

### Known hazards

These are the failure modes that snapshot-based sandboxes produce, and they are all
silent. Each has a gate.

#### H1 — Duplicate entropy across forks

A restored VM resumes with the kernel random pool in the state it had at snapshot
time. Eight forks generate the **same** random bytes. That means the same UUIDs, the
same session tokens, the same TLS nonces. This is a security bug, not a curiosity.

Fix: `kilninit` installs a resume hook. On resume it writes fresh entropy from the
host into `/dev/urandom` via `RNDADDENTROPY`, before any user process is unpaused.
The host supplies distinct bytes per fork over vsock.

Gate: `just gate P5`, step 9 of `just demo`.

#### H2 — Frozen clock

A restored VM's clock is the snapshot's clock. A sandbox restored from a template
built yesterday believes it is yesterday. TLS certificate validation then fails in
ways that look like network bugs.

Fix: resume hook calls `clock_settime` with host time, and re-arms the guest's
timekeeping, before user processes resume.

Gate: `just gate P4`, step 10 of `just demo`.

#### H3 — Dead vsock connections after restore

Connections do not survive a snapshot. The guest agent's socket is invalid on resume.

Fix: `kilninit` treats resume as a reconnect event. The host waits for a fresh hello
on the new connection before reporting the sandbox as running. Never assume the
pre-snapshot connection works.

#### H4 — Orphaned host resources

A crashed or killed daemon leaves firecracker processes, TAP devices, mounts, jailer
chroots and sparse files behind. These accumulate until the host runs out of
something.

Fix: every host resource is named after its sandbox ID, and every sandbox is placed
in its own cgroup. The reconciler on start lists reality, compares it to the store,
and destroys anything the store does not know about.

Gate: `just gate P7` kills the daemon mid-operation and asserts convergence.

#### H5 — Template deletion under live children

Covered under the memory model above. Refuse, do not reference-count lazily.

### What runs where

| Environment | Purpose | KVM |
|---|---|---|
| macOS laptop, inside a Lima Linux VM | Development, full test suite | Yes, nested |
| GitHub-hosted `ubuntu-24.04` | Correctness gates on every push | Yes |
| Linux VPS, self-hosted runner | Performance gates, nightly | Yes, bare |

Hosted GitHub runners can run Firecracker, but this is not contractual. The preflight
in `just` checks `/dev/kvm` and **fails** when it is missing. It never skips.

---

## 3. API — frozen

Everything reaches Kiln through this API. `kiln ctl` is a client of it, and the Go SDK
wraps it. Nothing but the daemon talks to Firecracker.

```
laptop  --HTTPS-->  kiln serve (VPS)  -->  Firecracker microVMs
 SDK / ctl / curl     one bearer token
```

This is not an E2B-compatible surface. Existing E2B SDK code will not point at it. That
is an accepted consequence of choosing a clean API for a single operator, and it is the
decision to revisit if Kiln is ever given to anyone else.

**This surface is frozen.** Do not change it without being asked. If the
implementation cannot satisfy it, stop and report the conflict.

Base path `/v1`. JSON in, JSON out. Authentication is one bearer token from config.
No accounts, no scopes.

Errors are `{"error": {"code": "...", "message": "..."}}` with a stable `code`.
Codes: `not_found`, `invalid`, `conflict`, `exhausted`, `internal`.

### Templates

#### `POST /v1/templates`

```json
{
  "name": "py312",
  "image": "docker.io/library/python:3.12-slim",
  "vcpus": 2,
  "memory_mb": 512,
  "disk_mb": 4096,
  "setup": ["pip install requests"],
  "egress_allow": ["pypi.org", "files.pythonhosted.org"]
}
```

Pulls the image, builds an ext4 rootfs, boots it once, runs `setup`, snapshots.
Returns `202` with a build ID. Building is asynchronous and observable.

`egress_allow` is the allowlist inherited by every sandbox from this template. An
empty list means no egress at all. The field is required. There is no implicit
default.

#### `GET /v1/templates` and `GET /v1/templates/{name}`

Returns name, image, resources, snapshot size, child sandbox count, build state.

#### `DELETE /v1/templates/{name}`

`409 conflict` while any sandbox from this template exists.

### Sandboxes

#### `POST /v1/sandboxes`

```json
{
  "template": "py312",
  "lifecycle": "ephemeral",
  "ttl_seconds": 3600,
  "idle_seconds": 300,
  "metadata": {"any": "string"}
}
```

`lifecycle` is `ephemeral` or `persistent`.

- `ephemeral` — destroyed at `ttl_seconds`, or at `idle_seconds` of no activity.
- `persistent` — snapshotted and paused at `idle_seconds`, restored on next request.
  `ttl_seconds` may be null.

This field is the **only** difference between the two modes. There is one code path.

Returns `201` with `{id, state, template, created_at}`.

#### `GET /v1/sandboxes` and `GET /v1/sandboxes/{id}`

States: `creating`, `running`, `sleeping`, `waking`, `stopping`, `destroyed`, `failed`.

A request to a `sleeping` sandbox wakes it. The caller does not manage this.

#### `POST /v1/sandboxes/{id}/exec`

```json
{"cmd": ["python", "-c", "print(1)"], "cwd": "/work", "env": {}, "timeout_seconds": 60}
```

Default response is buffered: `{"exit_code": 0, "stdout": "...", "stderr": "..."}`.
With `Accept: text/event-stream`, streams `stdout`, `stderr` and a final `exit` event.

Output is capped at 1 MiB per stream in buffered mode. Beyond that the response
carries `"truncated": true`. Do not silently discard.

#### `GET /v1/sandboxes/{id}/files/{path}`

Returns file bytes. `404` when absent.

#### `PUT /v1/sandboxes/{id}/files/{path}`

Body is file bytes. Creates parent directories. Returns `204`.

#### `POST /v1/sandboxes/{id}/snapshot`

Returns `201` with `{snapshot_id, size_bytes}`. The sandbox is paused during the
snapshot and resumes afterwards, unless `"stop": true` is sent.

#### `POST /v1/sandboxes/{id}/fork`

```json
{"count": 8}
```

Snapshots the sandbox if it is running, then restores `count` copies. Returns `201`
with `{"sandboxes": [{id, state}, ...]}`. Partial failure destroys every copy made in
this call and returns `500`. Forks are all-or-nothing.

#### `POST /v1/sandboxes/{id}/publish`

```json
{"port": 5173, "visibility": "public"}
```

`visibility` is `public` or `team`, and it is required. There is no default.

- `public` — anyone holding the URL may open it. The URL is the credential, so the
  subdomain contains 128 bits of randomness and is never derived from the sandbox ID,
  a branch name, or a PR number.
- `team` — a viewer session is required. Login is handled by `auth-all` at
  `/_kiln/auth/` on the same hostname.

Returns `{"url": "https://<subdomain>.<zone>/", "visibility": "public"}`.

Hostnames are `<random>.<zone>` for the first published port and
`<random>-<port>.<zone>` for any others. The zone is configured once at `kiln init`.
The certificate is a wildcard for `*.<zone>`.

A sandbox may publish several ports. Publishing dies with the sandbox, and the
hostname is never reused.

**A request to a published hostname wakes a sleeping sandbox.** The ingress proxy
holds the request while the restore happens and then forwards it. This is what makes a
preview environment usable days after its last visit, and it is the reason lifecycle
and ingress are one mechanism rather than two.

#### `DELETE /v1/sandboxes/{id}`

Destroys the sandbox and every host resource named after it. Idempotent.

### Snapshots

#### `GET /v1/snapshots`, `GET /v1/snapshots/{id}`, `DELETE /v1/snapshots/{id}`

`DELETE` returns `409 conflict` while restored children are alive.

#### `POST /v1/snapshots/{id}/restore`

```json
{"count": 1}
```

Same semantics as fork. Fork is this operation with an implicit snapshot first.

### Operations

#### `GET /v1/health`

`{"ok": true, "kvm": true, "firecracker": "1.x.y", "sandboxes": 3}`

#### `GET /v1/events`

Server-sent events. Every state transition, with sandbox ID, old state, new state and
reason. This is the observability surface for v1.

#### `GET /`

A read-only HTML status page. Sandboxes, templates, resource use. No controls, no
forms, no JavaScript build step. This is the maximum UI in v1.

---

## 4. Data model

### Principle

SQLite records **intent**. The host records **reality**. When they disagree, the host
wins and the reconciler corrects the database. Never treat a database row as proof
that a process exists.

### On-disk layout

Everything under one root, default `/var/lib/kiln`.

```
/var/lib/kiln/
  kiln.db                       SQLite, WAL mode
  kernel/vmlinux-<ver>          pinned kernel, fetched by `kiln init`
  templates/<name>/
    rootfs.ext4                 read-only base drive, shared
    mem                         snapshot memory file
    state                       Firecracker VM state file
    manifest.json               image digest, resources, allowlist, build time
  sandboxes/<id>/
    overlay.ext4                sparse writable drive
    firecracker.sock            Firecracker API socket
    uffd.sock                   page-fault socket
    jail/                       jailer chroot
  snapshots/<id>/
    mem
    state
    parent                      file containing the parent snapshot or template ID
```

Naming rule: every host resource carries the sandbox ID. TAP device `kiln-<short-id>`,
cgroup `kiln/<id>`, nftables chain `kiln_<short-id>`. The reconciler depends on this.
There are no exceptions and no unnamed temporary resources.

### Pinned versions

Recorded here, asserted by `just gate P0`, fetched by `kiln init`.

| Component | Version |
|---|---|
| Firecracker | pin at the newest 1.x release when P0 starts, then do not move |
| Guest kernel | a 6.1 LTS `vmlinux` built for the Firecracker minimal config |
| Go | the toolchain version in `go.mod` |

Bumping any of these is its own change with its own gate run. Never bump inside
another phase.

### Schema

Migrations are numbered SQL files, applied in order, recorded in `schema_migrations`.
Never edit an applied migration.

```sql
CREATE TABLE templates (
  name           TEXT PRIMARY KEY,
  image_ref      TEXT NOT NULL,
  image_digest   TEXT NOT NULL,
  vcpus          INTEGER NOT NULL,
  memory_mb      INTEGER NOT NULL,
  disk_mb        INTEGER NOT NULL,
  egress_allow   TEXT NOT NULL,          -- JSON array, may be empty, never null
  state          TEXT NOT NULL,          -- building | ready | failed
  error          TEXT,
  created_at     INTEGER NOT NULL
);

CREATE TABLE snapshots (
  id             TEXT PRIMARY KEY,
  template_name  TEXT NOT NULL REFERENCES templates(name),
  parent_id      TEXT REFERENCES snapshots(id),
  size_bytes     INTEGER NOT NULL,
  created_at     INTEGER NOT NULL
);

CREATE TABLE sandboxes (
  id             TEXT PRIMARY KEY,
  template_name  TEXT NOT NULL REFERENCES templates(name),
  snapshot_id    TEXT REFERENCES snapshots(id),   -- what it was restored from
  lifecycle      TEXT NOT NULL,          -- ephemeral | persistent
  state          TEXT NOT NULL,
  ttl_seconds    INTEGER,
  idle_seconds   INTEGER,
  last_active_at INTEGER NOT NULL,
  tap_name       TEXT,
  vsock_cid      INTEGER,
  pid            INTEGER,
  metadata       TEXT NOT NULL DEFAULT '{}',
  created_at     INTEGER NOT NULL,
  destroyed_at   INTEGER
);

CREATE TABLE published (
  sandbox_id     TEXT NOT NULL REFERENCES sandboxes(id),
  guest_port     INTEGER NOT NULL,
  subdomain      TEXT NOT NULL UNIQUE,     -- 128 bits of randomness, never reused
  visibility     TEXT NOT NULL,            -- public | team
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (sandbox_id, guest_port)
);

-- Viewer accounts for `team` previews. Owned by auth-all, listed here so the
-- schema is complete. These accounts grant nothing except the right to view.
-- auth-all migrations create them; do not hand-write them.

CREATE TABLE events (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  sandbox_id     TEXT,
  from_state     TEXT,
  to_state       TEXT,
  reason         TEXT,
  at             INTEGER NOT NULL
);
```

### Resource pools

Two pools, both allocated from SQLite inside a transaction, both reclaimed by the
reconciler.

- **vsock CID** — from 3 upward. Unique per running VM.
- **TAP index** — determines the device name. Guest addressing never varies, so the
  index only names the host device.

Exhaustion returns `exhausted`, never a collision. A collision here means two VMs
share a channel, which is a data leak between sandboxes.

### The multi-host seam

One column, `sandboxes.host_id`, is deliberately **absent** in v1. When a second host
is added, snapshots must be addressable off-box, which means the snapshot directory
becomes an object-storage prefix and `PageFaultSource` gains a remote implementation.

Both are already interfaces. Nothing else in the schema anticipates clustering. Do not
add host columns, leases, or leader election to v1.

---

## 5. Security

### Threat model for v1

Stated position: **hostile eventually, pragmatic now.**

**Two planes, two threat models.** This is the load-bearing distinction in v1.

| Plane | Reachable from | Threat model |
|---|---|---|
| **Ingress** — traffic to published sandbox ports | The open internet | Hostile now |
| **Control** — `/v1/*` | Localhost or a tunnel only | Pragmatic, because it is not exposed |

The split exists because the consequences are not comparable. Ingress going wrong means
someone sees a page they should not have seen. The control API going wrong means
someone else's code runs as root on the host. Those do not belong behind the same
defences, and they do not belong in the same phase.

In scope for v1:

- Code inside a sandbox is assumed to be hostile to the host and to other sandboxes.
- The boundary is the hypervisor. A guest kernel exploit must stay in that guest.
- The network boundary is enforced on the host, never in the guest.

Out of scope for v1, and stated so no one pretends otherwise:

- Side-channel attacks between co-resident VMs.
- Denial of service by a sandbox against the host through resource exhaustion beyond
  its cgroup limits.
- A malicious operator. There is one operator and it is you.

### Non-negotiable controls

Each is a gate. None is optional, and none may be deferred to "after v1".

#### C1 — Jailer always

Every Firecracker process runs under `jailer`: its own chroot, its own cgroup, seccomp
filters on, dropped privileges, no host filesystem visible beyond the jail.

Running `firecracker` directly is acceptable in exactly one place: a unit test that
does not touch the network. Nowhere else.

#### C2 — Default-deny egress

Per sandbox, an nftables chain that:

1. Drops all outbound traffic by default.
2. Allows the resolved addresses of the template's `egress_allow` list, by IP and
   port.
3. Drops `169.254.169.254` unconditionally, before any allow rule, with no way to
   configure it back on.

Rule 3 exists because on most VPS providers that address returns cloud metadata,
which may include provider API credentials. LLM-generated code will reach for it,
either by accident or because it was told to. A sandbox that can read host cloud
credentials is a full compromise of everything the operator owns.

DNS is resolved by a host-side resolver that only answers for allowlisted names. The
guest has no direct DNS egress.

#### C3 — No credentials in snapshots

A snapshot is a copy of guest memory. Anything in memory at snapshot time exists in
every fork, forever, on disk.

So: secrets are injected after restore over vsock, at the moment the sandbox becomes
running. They are never baked into a template, never present during a template build,
and never written to the rootfs.

If a sandbox holds injected secrets, forking it is refused unless the caller passes
`"allow_secret_fork": true`. Default is refuse.

#### C4 — Entropy reseed on every resume

See hazard H1 in `docs/01-architecture.md`. Without this, forks generate identical
random values. The host sends distinct bytes to each restored guest over vsock, and
`kilninit` feeds them to the kernel pool with `RNDADDENTROPY` before unblocking user
processes.

The gate asserts eight forks produce eight different values. Treat a failure here as
release-blocking, never as flaky.

#### C5 — Clock resync on every resume

See hazard H2. A stale clock breaks TLS validation in confusing ways, and it makes
any log timestamp from a sandbox untrustworthy.

#### C6 — Published ports are public, and treated as hostile in both directions

A published port serves code that an agent wrote, to visitors on the open internet.
Both sides of that sentence are untrusted.

- **The subdomain is the credential for `public` visibility.** 128 bits from a CSPRNG.
  Never derived from a sandbox ID, branch name, PR number or commit SHA — a guessable
  preview URL is an open door, and PR numbers are public.
- **`team` visibility requires a viewer session**, enforced at the proxy before any
  guest traffic is forwarded. A request that fails auth must never reach the guest.
- **Published hostnames are never reused.** A retired subdomain 404s forever. Reuse
  means a stale link lands in someone else's preview.
- **The proxy strips hop-by-hop headers in both directions** and never forwards its own
  credentials, cookies or the control-plane token into the guest.
- **The guest cannot learn the host.** Published traffic arrives on the TAP address,
  and the guest sees no host hostname, no control-plane address and no cloud metadata.

Publishing dies with the sandbox.

#### C6a — Wake-on-request is rate limited

A published hostname that wakes a sleeping sandbox turns one HTTP request into a
restore. That is an amplification primitive: cheap for a stranger to send, expensive
for the host to serve.

Wakes are rate limited per subdomain and capped concurrently across the host. Over the
cap, the proxy returns 503 rather than queuing restores.

#### C7 — Resource limits per sandbox

Every sandbox has a cgroup with memory and CPU limits set from its template. A
sandbox cannot consume the host by looping.

#### C8 — The control API is not on the internet in v1

Ingress is public. The control API is not. `kiln serve` binds its control listener to
`127.0.0.1`, and remote access in v1 is a tunnel or a private network. The ingress
listener binds `0.0.0.0:443` and serves only published hostnames — a request to the
ingress port for a control path is a 404, never a route.

This is a v1 boundary, not a permanent one. Making the control API publicly reachable
is **v1.1**, it has its own phase, and it is not something to enable with a flag,
because every concession below has to be withdrawn in the same change: scoped tokens
in place of one static string, rotation, rate limiting, and an audit trail.

The reason for the order is that the two failures are not comparable. A wrong preview
URL shows someone a page. A wrong control API runs a stranger's code as root on the
host.

The reason is C-level, not stylistic: authentication in v1 is a single static bearer
token compared in constant time. There are no accounts, no scopes, no rotation and no
rate limiting. That is adequate for one operator on a private network and inadequate
for a public port. An API that can create privileged VMs and expose their ports must
not be reachable from the open internet behind one static string.

When multi-tenancy arrives, `alternayte/auth-all` is the intended direction for the
control plane. It brings users, sessions, TOTP, OAuth, rate limiting and a SQLite store
that matches §4. Do not pull it in for v1 — v1 has no users, and sessions and cookies
are the wrong shape for callers that are an SDK, CI and an agent.

**Authentication alone does not make this API safe to expose.** Three risks sit outside
anything an auth library covers, and each needs its own answer before the port is
opened:

1. **Exposed sandbox ports are a second surface.** `POST /expose` puts untrusted code
   on a listening port. That path does not pass through control-plane auth, and it
   needs its own answer — its own token, its own origin, and ideally its own hostname.
2. **The blast radius is root on the host.** Most auth bugs leak data. An auth bypass
   here creates privileged VMs. That argues for mTLS or a private network *in addition
   to* authentication, never instead of it.
3. **A sandbox must not reach the control plane.** Guest traffic to the host's API
   address is dropped by the same default-deny chain as C2. This is an nftables rule,
   not middleware, and it must hold even when a template allowlist is permissive.

#### C9 — Published previews are an abuse surface on your domain

Agent-generated web applications served under your zone can host a phishing page as
easily as a dev server, and the reputational damage lands on your domain.

- Put a CDN or WAF in front of `*.<zone>` in production.
- Serve previews from a zone that is not your primary domain.
- `Content-Security-Policy: sandbox` and `X-Robots-Tag: noindex` on every preview
  response. Previews must never be indexed.

## What is intentionally not done

Say these out loud rather than half-building them:

- No LUKS encryption of sandbox disks. One operator, one host, no shared tenancy.
- No image signature verification. Images are pulled by digest, which is recorded, but
  not verified against a signing authority.
- No audit log beyond the `events` table.
- No rate limiting on the API. One operator holds the token.

Each becomes real work the moment a second tenant exists. That is a v2 conversation,
not a v1 shortcut.

### Review trigger

Any change that touches `internal/network/`, `internal/runtime/` jailer setup, or the
resume hooks in `guest/kilninit/` requires the security gate to be re-run in full,
even when the change looks unrelated. The gate is `just gate sec`.

---

## 6. Phases

Seven phases. Each has a **goal**, a **scope fence**, and an **exit gate**.

A phase is done when `just gate P<n>` passes. That tags the commit. There is no
evidence tree and no close ceremony — a gate is idempotent, so re-run it and look.

Read only the phase `just status` names. One phase per session.

**P0, P3 and P6 are attended.** `just loop` refuses them.

---

### P0 — Harness *(attended — an afternoon, not a day)*

The harness cannot verify itself, and every later phase trusts it. If `preflight.sh`
returns zero on a host without KVM, every green build after this is a lie and no gate
will ever tell you.

**Build.** Go module. `cmd/kiln` with `serve`, `ctl`, `init` stubs. `dev-env.sh` for
Lima. `nuke.sh`. The four scripts in `scripts/` already exist — read them, do not
rewrite them. `.github/workflows/kvm.yml` on `ubuntu-24.04` running `just preflight`.

**Fence.** No Firecracker. No SQLite. No API.

**Gate.** `go test ./tests/harness/...` proves: `preflight.sh` exits non-zero with no
`/dev/kvm`; `leak.sh` fails when its wrapped command leaves a stray `kiln-` TAP
device; `just status` reports P1 next.

**Read yourself:** `preflight.sh` fails closed, and `leak.sh` actually counts.

---

### P1 — Boot and exec

One VM boots from a rootfs in `testdata/`, runs a command, shuts down clean.

**Build.** `internal/runtime` — Firecracker under jailer. `guest/kilninit` — PID 1
with a vsock exec server. Host vsock client.

**Fence.** No API. No database. No snapshots. No image import.

**Gate.** Boot, exec `echo hello`, get `hello`, shut down. Repeat 20 times in a loop
under `leak.sh`. Counts before equal counts after.

---

### P2 — Image to template snapshot

Any OCI reference becomes a bootable rootfs, and then a template snapshot.

**Build.** `internal/template` — pull by digest with regclient, apply layers and
whiteouts, `mke2fs -d`, inject `kilninit`. Then boot once, run `setup`, pause,
snapshot memory and state, write `manifest.json`. SQLite and migrations land here.

**Fence.** Restore is P3. This phase only produces snapshots.

**Gate.** Import `python:3.12-slim`, boot it, `python -c "print(2+2)"` gives `4`. An
image with a file deleted in an upper layer produces a rootfs without that file —
whiteouts are what quietly breaks here. Rebuilding from the same digest gives the same
file listing. Deleting a template with children is refused.

---

### P3 — Restore and the resume hooks *(attended)*

The phase that makes or breaks the project. The gate catches a broken restore. It does
not catch a restore that works and is architecturally wrong — pages served eagerly
instead of on demand passes every assertion here and falls apart at fifty sandboxes.

**Build.** `kiln snapfault` as a separate process (see `docs/01-architecture.md` for
why). Restore path in `internal/runtime`. Resume hooks in `kilninit`: clock, entropy,
hostname, vsock reconnect. The API server and `POST /v1/sandboxes`.

**Fence.** One restore at a time. Fork is P4.

**Gate.** Create from a template, exec works. Guest clock within 2s of host **(H2)**.
Hostname is the sandbox ID. Sandbox reports running only after a fresh vsock hello
**(H3)**. 20 sequential create-destroy cycles under `leak.sh`, clean.

**Read yourself:** the UFFD code. Confirm pages are faulted on demand, not preloaded.

---

### P4 — Fork and egress

**Build.** `POST /fork` — shared read-only memory file, private written pages, a new
sparse overlay per fork, fresh entropy per fork. `internal/network` — TAP pool,
per-sandbox nftables chain, host-side DNS limited to the allowlist.

**Gate.** Fork one sandbox into 8. All 8 see a pre-fork file. Each writes a different
value and no fork sees another's **(R-P-006)**. Each reads `/dev/urandom` and all 8
differ **(H1, C4)**. Eight forks of a 512 MB template use under 2 GB. Fork is
all-or-nothing — inject a failure at copy 5, assert 1 to 4 are destroyed. An
allowlisted host is reachable, a non-allowlisted one is not, `169.254.169.254` is
refused even with the allowlist set to everything **(C2)**. Sandbox A cannot reach
sandbox B.

**Worth a `just review P4`.** Entropy and the metadata block are the two findings that
cost you the most if they slip.

---

### P5 — Lifecycle and reconcile

**Build.** TTL and idle timers, sleep as snapshot-and-pause, wake on request, the
reconciler and startup sweep, cgroup limits, `kiln init`, the Go SDK.

**Fence.** No public ingress. Wake is triggered by an API call in this phase; the proxy
triggering it is P6.

**Gate.** A `persistent` sandbox sleeps after `idle_seconds` and wakes with files and
memory intact. An `ephemeral` one dies at TTL. `SIGKILL` the daemon with three
sandboxes running and one mid-create, restart, the reconciler converges, zero orphans
**(H4)**. A stray `kiln-deadbeef` TAP is swept on restart.

---

### P6 — Public ingress *(attended)*

The only part of Kiln that strangers can reach. Everything else in this project is
defended by not being reachable; this is defended by being correct.

**Build.** `internal/ingress` — HTTPS listener on `0.0.0.0:443`, hostname routing to
sandbox and port, wildcard certificate via CertMagic and ACME DNS-01 (HTTP-01 cannot
issue wildcards), `auth-all` mounted at `/_kiln/auth/` for `team` visibility,
wake-on-request with the request held during restore, wake rate limiting, and the
read-only status page.

**Gate.**
- A `public` preview is reachable over HTTPS at its hostname with no credential.
- A subdomain is 128 bits of randomness. Assert it is not derived from the sandbox ID,
  and that two published ports never collide **(C6)**.
- A `team` preview returns a login challenge, and an unauthenticated request **never
  reaches the guest**. Assert at the guest, not at the proxy.
- A request to a retired subdomain 404s. Hostnames are never reused **(C6)**.
- A request to a sleeping sandbox's hostname wakes it and returns the response. The
  client sees one slow request, not an error.
- Over the concurrent wake cap, the proxy returns 503 and does not queue restores
  **(C6a)**.
- A request to the ingress port for `/v1/sandboxes` 404s. It is never routed **(C8)**.
- The control listener is not bound to a public address **(C8)**.
- The guest cannot reach the control API, even with a permissive template allowlist.
- Every preview response carries `X-Robots-Tag: noindex` **(C9)**.

**Read yourself:** the auth check for `team` visibility, and the wake path. A proxy
that forwards first and authenticates second passes a naive test and is wrong.

Then `just demo` — the full sequence in §1 — passing unmodified on the Lima VM and on
the VPS.

---

## 7. Verification

Two things must execute. Everything else in this project is prose, and prose cannot
count a leaked network device.

### 7.1 How a gate runs

A gate is a Go test with the `kvm` build tag, run inside `leak.sh`:

```sh
./leak.sh go test -tags=kvm -count=1 -timeout 20m -run TestGateP3 ./tests/...
```

On pass, tag the commit: `git tag -f gate/P3`.

That tag is the entire progress-tracking system. `git tag -l 'gate/*'` tells you what
is closed, and the first phase without a tag is the next one. There is no evidence
file, no coverage matrix, and no close ceremony. **Gates are idempotent — re-run one
and look.**

Two rules about gates that matter more than they look:

- **Never write a timing assertion into a gate.** Latency is not deterministic on
  shared hardware. Put it in a gate and it flakes, then someone adds a retry, and the
  gate becomes decorative. Measure performance separately and compare against the last
  run by eye.
- **Never skip on a missing precondition.** If `/dev/kvm` is absent the suite fails. A
  skip turns a red build green, and a green build must mean the work was verified.

### 7.2 leak.sh

The highest-value check in the project. The most common bug in a system like this is a
host resource that outlives its sandbox, and it is completely silent until the host
runs out of something. No amount of instruction makes an agent notice an orphaned TAP
device. This does, deterministically, in forty lines.

Every VM test from P1 onward runs inside it.

Write this to `leak.sh` exactly as it appears. Do not improvise it.

```bash
#!/usr/bin/env bash
# Wrap a command. Count host resources before and after. Any difference fails.
#
# This is the highest-value check in the project. The most common bug here is a
# resource that outlives its sandbox, and it is silent until the host runs out.
set -uo pipefail

ROOT="${KILN_ROOT:-/var/lib/kiln}"
DB="$ROOT/kiln.db"

snapshot() {
  echo "procs $(pgrep -c firecracker 2>/dev/null || echo 0)"
  echo "taps $(ip -o link show 2>/dev/null | grep -c 'kiln-' || echo 0)"
  echo "chains $(nft list chains 2>/dev/null | grep -c 'kiln_' || echo 0)"
  echo "mounts $(grep -c " $ROOT" /proc/mounts 2>/dev/null || echo 0)"
  echo "dirs $(ls -1 "$ROOT/sandboxes" 2>/dev/null | wc -l)"
  echo "rows $(sqlite3 "$DB" 'select count(*) from sandboxes where destroyed_at is null' 2>/dev/null || echo 0)"
}

names() {
  echo "--- firecracker pids:"; pgrep -a firecracker 2>/dev/null || true
  echo "--- taps:";   ip -o link show 2>/dev/null | grep 'kiln-' || true
  echo "--- chains:"; nft list chains 2>/dev/null | grep 'kiln_' || true
  echo "--- dirs:";   ls -1 "$ROOT/sandboxes" 2>/dev/null || true
}

before="$(snapshot)"
"$@"; rc=$?
sleep 1                      # let normal teardown finish before accusing it
after="$(snapshot)"

if [ "$before" != "$after" ]; then
  echo "LEAK DETECTED" >&2
  diff <(echo "$before") <(echo "$after") >&2 || true
  echo "" >&2; names >&2
  exit 1
fi

echo "leak check: clean"
exit $rc
```

`sqlite3` is not a Go dependency. Either install it in the dev VM, or replace that line
with `kiln ctl count-sandboxes` once P2 exists.

### 7.3 The hook

Eight lines of grep that block the one unrecoverable failure. An agent that edits this
SDD to match its code leaves behind a specification that describes the bug, and every
later session inherits it as truth.

A sentence asking nicely is not equivalent, because the agent that breaks this rule is
by definition the one that has stopped following sentences.

Write this to `scripts/hooks/pre-commit`, `chmod +x` it, and run
`git config core.hooksPath scripts/hooks`.

```bash
#!/usr/bin/env bash
# Two rules. Both block failures that no later gate can detect.
set -euo pipefail
staged() { git diff --cached --name-only --diff-filter=ACMR; }

# 1. The spec is frozen. A stuck agent edits the spec until it agrees with its
#    code, and then the build passes against a description of the bug.
if staged | grep -qE '^SDD\.md$' && [ "${KILN_SPEC_CHANGE:-}" != "1" ]; then
  staged | grep -E '^SDD\.md$' | sed 's/^/    /' >&2
  echo "REJECTED: commit touches SDD.md. The spec is frozen." >&2
  echo "  If the design is wrong, say so and stop. Override: KILN_SPEC_CHANGE=1" >&2
  exit 1
fi

# 2. No skipped tests. A skip on a missing precondition turns a red build green.
skips="$(staged | grep -E '_test\.go$' | xargs -r grep -nE 't\.Skip' || true)"
if [ -n "$skips" ]; then
  echo "$skips" | sed 's/^/    /' >&2
  echo "REJECTED: tests contain skips. Assert the precondition and fail instead." >&2
  exit 1
fi
```

**The P0 gate must prove the hook works** — that a commit touching `SDD.md` is actually
rejected. An instruction to install a hook is an instruction. An assertion that the
hook rejects a commit is a check. Only the second one survives an agent that has
decided the rule is inconvenient.

When you genuinely change the design, override it: `KILN_SPEC_CHANGE=1 git commit ...`.
If you need that twice in one phase, the design is wrong and the answer is a
conversation, not a commit.

### 7.4 Preflight

At the top of every VM test run, assert and fail on any miss: `/dev/kvm` exists and is
readable and writable; `firecracker`, `jailer`, `nft` and `mke2fs` are on PATH;
`firecracker --version` matches the pin in §4. Never warn and continue.

---

## 8. Session protocol

### 8.1 Every session, in order

1. Read this file, in full.
2. Read `LOG.md`. If it does not exist, create it with the format in §8.4.
3. Run `git tag -l 'gate/*'`. The first phase in §6 without a tag is your phase.
   **The tags decide what is done, not `LOG.md` and not your reading of the code.**
4. Run the fast checks: `gofmt`, `go vet`, `go build ./...`, `go test ./internal/...`.
   If they are red, fix that first.

Then state, in four lines, before writing any code:

- the phase and its exit gate
- which assertions fail now
- the smallest change that moves one to passing
- anything in the phase you believe is wrong or underspecified

Then implement it. Do not wait for approval — the gate is the check.

### 8.2 While working

- Write the failing test first.
- Run the gate after each change. Read the real failure. Do not guess.
- Respect the scope fence. Work belonging to a later phase is a finding, not a bonus.
- No mocks in integration tests. A mocked microVM proves nothing.
- Package by feature. No `services/`, `models/`, `utils/`.
- No interface until there are two implementations, except the three named in §2.
- No dependency outside the list in §2. Ask first.
- Simple technical English in comments and commits. Short sentences, active voice.

### 8.3 Stop conditions

Stop, write what you tried into `LOG.md`, and report. Do not continue.

- **Two failed attempts at the same assertion.** A third attempt means broadening the
  change until a test passes, which is the worst outcome available here. Recovery is
  `git reset --hard gate/<previous phase>`.
- The design appears wrong. Say it once, with the reason. Do not edit this file.
- The hook rejected your commit. That is information about your change, not an
  obstacle to route around.

### 8.4 LOG.md

For orientation, never for truth. Its job is the thing context loss destroys: what
happened last session, what is half-finished, and what was tried and failed so the next
session does not try it again.

**The git tags decide whether a phase is done.** If `LOG.md` says a phase is complete
and there is no `gate/` tag for it, the log is wrong. Never write "phase complete" —
write what you did and let the tag speak.

Append at the end of every session. Never rewrite earlier entries.

```markdown
## 2026-09-14 — P3

Done: UFFD handler serves pages on demand, restore path wired, clock hook works.
Half-finished: entropy reseed writes to /dev/urandom but RNDADDENTROPY returns EPERM.
Tried and failed: seeding from the guest side before pivot — too late, user processes
  had already started.
Next: check whether kilninit keeps CAP_SYS_ADMIN after the pivot.
Gate: TestGateP3 — 4 of 6 assertions pass.
```

### 8.5 Attended phases

**P0**, **P3** and **P6** are marked attended in §6. They are not run unattended, because in
both the gate cannot catch a wrong implementation. If you are an agent and the human
has not explicitly said they are watching, do those two phases and then stop for
review rather than continuing to the next.
