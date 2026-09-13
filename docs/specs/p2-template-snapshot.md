# P2 template snapshot

## What it does
Turns any OCI reference into a bootable ext4 rootfs, boots it once, runs the template's setup commands, and stores the result as a template snapshot. The manifest records the image digest, the resources and the egress allowlist.

## Decisions
- `internal/template` pulls the image by digest with regclient, applies layers and whiteouts in order, and builds the filesystem with `mke2fs -d`. It injects `kilninit`.
- Registry credentials never enter the API. Kiln uses the host's registry credential file when one exists. Other images must be public.
- The build boots the rootfs in a VM that has an internal id but no sandbox row. Its TAP and nftables chain carry that id, so `scripts/leak.sh` counts them and the sweep destroys them.
- The build VM gets default-deny egress built from the template's `egress_allow`, with the metadata address dropped first. The host-side resolver and the per-VM chain land in this slice, because `setup` needs the allowlist. P4 reuses the same code for sandboxes.
- `egress_allow` is required and may be empty. An empty list means no egress.
- `setup` runs in order inside the build VM. A failing command fails the build and its output goes in `error`.
- The snapshot is the template: `mem`, `state` and `manifest.json` under the template directory. The rootfs remains the read-only shared drive.
- Template state is `building`, `ready` or `failed`. `POST /v1/templates` returns 202 with `{name, state}` and the caller polls `GET /v1/templates/{name}`. A name in `building` returns 409. A name in `failed` accepts a new build. A name in `ready` returns 409 until it is deleted.
- `DELETE /v1/templates/{name}` returns 409 while any live sandbox or any snapshot row exists. Otherwise it deletes the template, its files and its destroyed sandbox rows in one transaction. The events table keeps the history.
- The HTTP API server lands here with the three template endpoints: one bearer token from `config.json`, errors as `{"error":{"code","message"}}` with stable codes.
- SQLite and its numbered migrations land here. The first migration creates `templates` and `events`. An applied migration is never edited.
- Rebuilding the same digest produces the same file listing.
- A failed build destroys its VM and leaves no TAP, chain or directory behind.

## Out
- No restore. The gate runs `python -c "print(2+2)"` in the build VM, before the snapshot.
- No fork and no sandbox lifecycle.

## How I know it works
- `just gate P2` imports `python:3.12-slim`, runs the setup, and reads `4` from `python -c "print(2+2)"` in the build VM.
- An image with a file deleted in an upper layer produces a rootfs without that file.
- Two builds from the same digest produce the same file listing.
- `DELETE` on a template with a live sandbox, or with a snapshot, returns 409.
- The gate runs inside `scripts/leak.sh` with equal counts.
