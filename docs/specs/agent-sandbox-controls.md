# Agent sandbox controls

Closes the Kiln side of alternayte/kiln#16. The Factoree adapter is `docs/specs/kiln-sandbox-provider.md` in alternayte/factoree.

## What it does
A template names a non-root user, and everything a caller runs or writes in its sandboxes uses that user. A caller replaces the egress allow list of one running sandbox. A caller starts a detached process, reads its output from an offset, and sends it a signal. A sandbox does not sleep while a detached process runs.

## Decisions
### User
- `POST /v1/templates` takes an optional `user`, a name or a numeric id from the image — one template per image digest sets it once, and no caller can omit it per call.
- Exec, detached processes, file reads and writes, the terminal and the start command use `user`. `setup` runs as root — the image and its setup need root, and an agent does not.
- A template with no `user` runs everything as root — existing templates do not change.
- The image `USER` does not set this field — an existing image with `USER` must not change behaviour without a request.
- The build fails when `user` does not exist in the image after `setup` — a sandbox from that template cannot run anything.
- Kilninit sets the uid, the gid, the supplementary groups, `HOME` and `USER` from `/etc/passwd` and `/etc/group` — tools read these, and a uid alone gives a broken environment.
- A file operation runs with the filesystem identity of `user` — kilninit is root, and otherwise an agent symlink redirects a root write to `/opt/factoree/bin`.

### Egress
- `PUT /v1/sandboxes/{id}/egress` takes `egress_allow` and replaces the policy of that sandbox — Factoree changes the list three times per run.
- The sandbox row stores the policy, and `Attach` and `Adopt` read it first — otherwise a wake or a daemon restart restores the template list.
- A sleeping sandbox takes the change without a wake, and the wake applies it — a policy change needs no running guest.
- A snapshot records the policy in force, and a fork or a restore starts with it — a copy must not widen to the template list.
- `*.example.com` is valid in a template and in this call, and it matches each subdomain and not `example.com` — this is the rule in Factoree's `covers`, and an adapter switch must break nothing.
- The call returns after the host swaps the list, empties the resolved address set and cuts every outbound connection of the sandbox — repository code holds such a connection open otherwise. Inbound connections to published ports stay.
- `GET /v1/sandboxes/{id}` returns the current `egress_allow` — a caller must see the policy in force.

### Detached processes
- `POST /v1/sandboxes/{id}/processes` takes `cmd`, `cwd` and `env` as exec does, and returns an id once the process starts — a runner outlives any one request.
- `GET .../processes/{pid}?stdout_offset=&stderr_offset=` returns `running`, `exit_code`, the output after each offset and the next offsets — a poll reads only new output.
- `DELETE .../processes/{pid}?signal=` signals the process group, with `TERM` as the default — a runner has children, and a caller needs a `KILL` after a `TERM`.
- Each process runs in its own session and has no deadline — the caller owns the deadline, as the Factoree port states.
- Kilninit keeps the last 8 MiB of each stream in memory, and the response reports the bytes lost — guest memory bounds it, and a sandbox user cannot read or change it.
- Kilninit keeps 256 process records and removes the oldest exited record first. A start with 256 running processes fails — one sandbox must not fill guest memory.
- The processes and their output live in guest memory, so they survive a snapshot, a sleep and a daemon restart. A fork holds its own copy of each — this follows from what a snapshot is.
- At the idle deadline the host asks kilninit for its running process count. A count above zero moves the deadline, for persistent and ephemeral sandboxes — the guest is the only true source, and an agent run must not stop. The TTL still applies.
- A `GET` on a process touches activity like exec — a poll is a caller at work.

### Contract
- `internal/apispec`, the SDKs and the MCP tools carry all of the above — the API contract states what the host accepts.
- The new routes need the same write scope as exec, and a `GET` on a process needs the same scope as a sandbox read — a reader changes nothing.

## Out
- A root override per exec.
- A process list route. Factoree holds its process ids.
- Restart, log files or a process supervisor.
- A template or a snapshot change for each repository. Factoree decides its warm snapshots.
- The Factoree image and the Factoree adapter.

## How I know it works
- `id` from exec in a template with `user: agent` prints the agent uid, and a write to a root-owned file fails.
- `PUT /files` writes a file the user owns. A symlink in the sandbox to a root-owned file makes the same call fail, and the target stays unchanged.
- After `PUT /egress` removes a host, `curl` to it fails in the guest, an open connection to it stops, and a publish connection continues.
- The narrowed policy still holds after a sleep and a wake, after a host daemon restart, and in a fork.
- `curl` to `a.example.com` works under `*.example.com`, and `curl` to `example.com` does not.
- A detached `sleep 3600` in a sandbox with `idle_seconds: 60` keeps it running past the deadline. After `DELETE`, the sandbox sleeps at the next deadline.
- `GET` returns output from an offset, then `running: false` and the exit code.
- `SANDBOX_ADAPTER=kiln just live` in Factoree opens a pull request.
