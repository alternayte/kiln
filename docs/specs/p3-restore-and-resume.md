# P3 restore and resume

## What it does
Creates a sandbox by restoring a template snapshot, and serves the sandbox API. A create never boots a kernel. The resume hooks correct the clock, the entropy pool, the hostname and the vsock channel before user processes run.

## Decisions
- `kiln snapfault` is a separate process, one per restoring VM. It serves guest memory pages from the template memory file through userfaultfd and keeps written pages private to that VM. Pages are faulted on demand, never preloaded. The separate process keeps Go garbage collection out of the page-fault path.
- Create allocates a sparse overlay of the template's `disk_mb` as the writable drive, opens the template memory file read-only, starts `snapfault`, launches Firecracker under jailer, loads the snapshot, and resumes.
- `kilninit` mounts an overlay with the template rootfs as lower and the overlay as upper, then pivots into it. Create copies no rootfs.
- On resume, before user processes run, `kilninit` writes host-supplied entropy with `RNDADDENTROPY`, sets the clock from the host, sets the hostname to the sandbox ID, and reconnects vsock. The sandbox reports `running` only after a fresh vsock hello.
- The API base path is `/v1`, authenticated by the bearer token in `config.json`, compared in constant time. Errors are `{"error":{"code","message"}}` with codes `not_found`, `invalid`, `conflict`, `exhausted`, `internal`.
- `config.json` also holds secret values by name.
- States: `creating`, `running`, `sleeping`, `waking`, `stopping`, `destroyed`, `failed`.
- `POST /v1/sandboxes` takes `template`, `lifecycle`, `idle_seconds`, `ttl_seconds`, `metadata` and `secrets`, and returns 201 with the sandbox. `idle_seconds` is required and greater than zero. `ttl_seconds` is optional, and null means no deadline. A create that names an unknown secret returns `invalid` and creates nothing.
- `lifecycle` is `ephemeral` or `persistent` and is the only difference between the two modes. Ephemeral destroys at the first of idle or ttl. Persistent sleeps at idle and destroys at ttl when ttl is set.
- Secrets become environment variables for every exec in that sandbox. Kiln writes the values nowhere on disk.
- Exec is buffered by default and capped at 1 MiB per stream, with `truncated` set beyond the cap. `Accept: text/event-stream` streams instead, with `stdout` and `stderr` events and a final `exit` event carrying the exit code and timeout flag.
- The file endpoints treat the path after `/files/` as the guest path without its leading slash. `..`, an empty path, a directory and any non-regular file return `invalid`. `GET` streams. `PUT` creates parent directories and writes through a temp file plus a rename.
- The exec environment is a fixed base (`PATH`, `HOME=/root`) plus injected secrets plus the request `env`, with the request winning. `cwd` defaults to `/`.
- `last_active_at` refreshes on exec, file read, file write, snapshot, fork and publish. Status and event reads do not refresh it.
- Create checks free space first. Less than `disk_mb` free returns `exhausted` and creates nothing.
- The `sandboxes` table lands here. It holds no `host_id` column.

## Out
- No network device, no egress and no published ports. Those arrive in P4 and P6.
- No fork, no timers, no reconciler and no ingress.

## How I know it works
- `just gate P3`: create a sandbox from a template, and exec works.
- The guest clock is within 2 seconds of the host, and the hostname equals the sandbox ID.
- The sandbox reports `running` only after a fresh vsock hello.
- 20 create and destroy cycles run inside `scripts/leak.sh` with equal counts.
