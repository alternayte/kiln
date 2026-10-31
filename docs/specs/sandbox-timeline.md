# Sandbox timeline

## What it does
A caller marks a timeline point on a running sandbox. A timeline point holds memory, processes and disk as a diff against the previous point. The caller rewinds the sandbox to a point in place, or forks new sandboxes from a point. A repair attempt then restarts from before a failed attempt, and does not start from the beginning.

## Decisions
### Points
- `POST /v1/templates` takes optional `timeline: true` — dirty page tracking adds cost, so only a template that uses it pays.
- `POST /v1/sandboxes/{id}/timeline` takes an optional `label` and returns a point id — the caller chooses when, because only the caller knows what a step is.
- Kiln takes no point by itself — an automatic point per exec costs disk and pauses a guest with no request.
- `GET /v1/sandboxes/{id}/timeline` lists each point with id, label, time and bytes. `DELETE .../timeline/{point}` removes one, and Kiln merges its pages into the next point — a chain must stay valid.
- The first point is a full snapshot, and each later point is a Firecracker diff snapshot — a diff holds only changed pages.
- Kilnd merges a chain with the Firecracker `snapshot-editor` tool — this tool is the supported way to rebase a diff.
- Each point holds a sparse copy of the overlay — memory holds files open on that overlay, so disk and memory must match.
- A sandbox holds at most 32 points, and their bytes stay under twice the guest memory plus the overlay size. At either limit, Kiln merges the two oldest points, and the list shows the merge — a chain must not fill the disk or slow every restore.
- Point bytes count toward the tenant `max_snapshot_bytes` — a point holds disk as a snapshot does.
- A point of a sandbox in `observe` returns `conflict` — this matches the snapshot rule in `docs/specs/egress-guard.md`.
- A point records the egress policy in force and whether the sandbox is secret-bearing — this matches a snapshot.

### Rewind and fork
- `POST .../timeline/{point}/rewind` stops the guest and restores that point under the same sandbox id and hostnames — a published link keeps working.
- A rewind removes every later point — the timeline stays one line, and a fork is the way to branch.
- A rewind keeps the current egress policy and credential bindings — a rewind must not restore a wider policy or a revoked key.
- A rewind and a fork send new entropy, as a restore does — a copy must not repeat random values.
- A rewind cuts open connections — the far side has moved on.
- `POST /v1/sandboxes/{id}/fork` takes an optional `point` — a fork from a point branches a run.
- A fork from a point follows the normal fork rules, including `allow_secret_fork`, no inherited bindings and an empty egress record — a point is a snapshot.
- A restore or a fork first merges the chain into one memory file, and keeps the result — `snapfault` reads one file, and a second use costs nothing.
- A sleep of a sandbox with a timeline keeps its points — sleep does not change the record of work.
- A destroy removes every point, unless a live fork still reads it, as with a snapshot — a sandbox must leave nothing behind.

### Measurement
- `LOG.md` records the guest pause, point size and restore time for a 4096 MB sandbox on the Dedibox, and the cost of dirty page tracking under load — this spec states no claim that nobody measured.
- The same entry records how long Factoree model calls last, from its `AgentSession` rows — model-wait parking gets a spec only when a diff snapshot and a restore together take a small fraction of a typical call.

### Contract
- `internal/apispec`, the SDKs, the MCP tools and `kiln` carry all of the above — the API contract states what the host accepts.

## Out
- An automatic point per exec or per turn.
- A branch tree inside one sandbox. A fork is a new sandbox.
- A diff of files between two points.
- A point moved to another host.
- A point on a template with no `timeline: true`.
- Model-wait parking.

## How I know it works
- A sandbox from a template with `timeline: true` writes a file and holds a value in a running process. `POST .../timeline` returns a point.
- The sandbox changes both, then `rewind` returns the file and the running process to that point, under the same id and hostname.
- A later point is gone from the list after the rewind.
- A second point has fewer bytes than the guest memory, as `GET .../timeline` shows.
- `POST /fork` with `point` and `count: 3` gives three sandboxes with that state, and `/dev/urandom` differs in each.
- `DELETE` on a middle point leaves a later point that still restores.
- A 33rd point merges the two oldest, and the newest still restores.
- A template with no `timeline: true` returns `conflict` on `POST .../timeline`.
- A rewind after `PUT /egress` narrowed a list keeps the narrowed list.
- `DELETE` on the sandbox leaves no point files on the host, unless a fork still reads one.
- `LOG.md` holds the measured numbers.
