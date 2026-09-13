# P4 fork and egress

## What it does
Copies a running sandbox into independent copies, and gives every sandbox default-deny egress with a per-template allowlist. Every copy starts from one snapshot and diverges from there.

## Decisions
- `POST /v1/sandboxes/{id}/fork` snapshots the sandbox when it is running, then restores `count` copies. Each copy gets a new sparse overlay and its own copy-on-write memory through its own `snapfault`. A fork copies no rootfs.
- Fork is all-or-nothing. A failure at any copy destroys every copy made in that call and returns 500.
- The host sends distinct entropy to every copy. Eight forks produce eight different `/dev/urandom` values.
- `POST /v1/sandboxes/{id}/snapshot` returns 201 with `{snapshot_id, size_bytes}` and creates a listed row in `snapshots`. With `"stop": true` the sandbox enters `sleeping` after the snapshot; its id, metadata and published hostnames survive, and any request wakes it. Without `stop` the sandbox resumes.
- `POST /v1/snapshots/{id}/restore` copies a listed snapshot into new sandboxes and has the same semantics as fork. `DELETE /v1/snapshots/{id}` returns 409 while restored children are alive.
- Fork and restore refuse a secret-bearing memory image unless `allow_secret_fork: true` is sent, and return `conflict`. A snapshot of a secret-bearing sandbox is allowed, and its row is marked secret-bearing.
- Sleep images from `stop: true` or idle are the sandbox's own state, not rows in `snapshots`. They never appear in `GET /v1/snapshots` and cannot be deleted.
- One TAP per sandbox, named `kiln-<short-id>` and stored in `tap_name`. Every guest sees `172.31.0.2/30` with gateway `172.31.0.1`; only the host side differs.
- One nftables chain per sandbox, named `kiln_<short-id>`, default drop.
- DNS: the guest resolves through the host resolver on the gateway, and only port 53 to the gateway is allowed. The resolver answers only names in the template's `egress_allow`, forwards those upstream, and inserts every answer into the sandbox's set with the record's TTL as the timeout.
- Egress permits TCP 443 to the sandbox's set. `169.254.169.254` is dropped before any allow rule and cannot be configured back on.
- The host forwards nothing between sandboxes. Sandbox A cannot reach sandbox B.
- The control API address is dropped by the same chain, even when the allowlist is permissive.
- Only the vsock CID is pooled, from 3 upward. Exhaustion returns `exhausted`, never a collision.
- The `snapshots` table lands here.

## Out
- No public ingress, no published ports, no wake on request, no timers.
- No per-template cgroup limits yet.

## How I know it works
- `just gate P4`: fork one sandbox into 8. All 8 see a pre-fork file. Each writes a different value, and no copy sees another copy's value.
- Each of the 8 reads `/dev/urandom`, and all 8 values differ.
- Eight forks of a 512 MB template use under 2 GB.
- A failure injected at copy 5 destroys copies 1 to 4.
- An allowlisted host is reachable, a non-allowlisted host is not, and `169.254.169.254` is refused with the allowlist set to everything. Sandbox A cannot reach sandbox B.
- `scripts/leak.sh` counts are equal.
