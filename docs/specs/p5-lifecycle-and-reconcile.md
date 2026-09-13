# P5 lifecycle and reconcile

## What it does
Gives sandboxes their timers: idle sleep, idle destroy and TTL destroy. It restarts the daemon without leaking host resources, adopts the sandboxes the host still has, and adds `kiln init` and the Go SDK.

## Decisions
- One timer per sandbox. Ephemeral destroys at the first of `idle_seconds` of no activity or `ttl_seconds`. Persistent sleeps at idle and destroys at ttl when set.
- A request to a `sleeping` sandbox wakes it: the API reports `waking` until a fresh vsock hello, then `running`. Files and memory are intact.
- Sleep snapshots the sandbox's memory and stops the VM. The image lives under the sandbox's own directory and is not a listed snapshot.
- Every sandbox has a cgroup with the template's `memory_mb` and `vcpus` as limits.
- The reconciler sweeps on start and every 60 seconds. A `running` row with a live Firecracker process and socket is adopted. A `running` row with no process becomes `failed` with a reason. A row in `creating`, `waking` or `stopping` becomes `failed` and its host resources are destroyed. A host resource with no row is destroyed.
- The host is the truth. A database row never proves that a process exists.
- `kiln init` creates the Kiln root, the pinned binaries, the nftables base and the database.
- `client/` wraps the HTTP API for Go callers. It is the only package outside this module imports.
- Events are not pruned in v1. The table is written on state transitions only.
- After this slice, a change that touches `internal/network`, the jailer setup in `internal/runtime`, or the resume hooks in `guest/kilninit` requires `just gate sec` in full.

## Out
- No public ingress. Wake is triggered by API calls.
- No viewer accounts and no ACME.

## How I know it works
- `just gate P5`: a persistent sandbox sleeps after `idle_seconds` and wakes with files and memory intact.
- An ephemeral sandbox dies at its TTL.
- `SIGKILL` the daemon with three running sandboxes and one mid-create, restart, and the sweep converges with zero orphans.
- A stray `kiln-deadbeef` TAP is swept on restart.
- `scripts/leak.sh` counts are equal.
