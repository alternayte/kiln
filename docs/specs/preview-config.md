# Preview config

## What it does
`POST /v1/sandboxes` takes an `env` map of literal key and value pairs.
The host puts the values into guest memory with the resume hooks, beside the injected host secrets.
The start command, `exec` and the terminal see the values.
`GET /v1/sandboxes/{id}` returns the key names and never the values.
The preview Action takes an `env` input and passes it to sandbox create for a preview from the base repository.
The preview Action takes a `publish` input of extra ports and publishes each one beside `port`.

## Decisions
- `env` is on sandbox create, not on the template — a template forks into many sandboxes, and a value on the template row goes into the snapshot lineage and every fork.
- Kiln handles every `env` value like the host secrets — Kiln cannot tell a credential from a flag, and a second field for plain values is a label the tenant gets wrong.
- The host never writes a value to disk — the same rule as host secrets (`internal/sandbox/sandbox.go:62`).
- The store keeps the key names on the sandbox row — `GET` and the audit log show which keys a sandbox has.
- A non-empty `env` marks the sandbox `SecretBearing` — fork and snapshot restore then need `allowSecret`, as for host secrets.
- A sleep keeps the values in the memory image — the same as host secrets today; a wake does not inject again.
- A key that is in both `env` and `secrets` rejects the request with `invalid` and names the key — any precedence overrides one side with no error.
- A key matches `^[A-Za-z_][A-Za-z0-9_]*$` — the guest builds a process environment from it.
- `env` holds at most 128 keys and 64 KiB of keys and values together — the resume hooks frame is limited to 1 MiB (`internal/guestproto/guestproto.go:24`).
- The guest merge order is base, host secrets, `env`, then per-exec `env` — per-exec values stay the most specific layer.
- The Action input `env` is multiline, one `KEY=value` per line, split on the first `=` — it matches `setup`, and callers fill values from `${{ secrets.* }}`.
- The Action ignores blank lines and fails the step on a line with no `=`, before it calls Kiln — a malformed line is a caller error.
- `up.sh` masks each value with `::add-mask::`, and `payload.py` builds the JSON — no value reaches a command line or the log.
- The Action sends no `env` when the head repository is not the base repository, and logs that it did not — a label approves one commit, and a credential in that commit's sandbox can leave through its published port.
- `port` stays required, the Action publishes it first, and it stays the `url` output — the first published port gets `<random>.<zone>`, and existing workflows keep working.
- The Action input `publish` lists extra ports separated by spaces and defaults to empty — it matches `egress-allow`.
- The Action fails the step on a `publish` entry that is not a port from 1 to 65535 or that equals `port` — a typo must not give a preview with a missing hostname.
- Each extra port gets the same visibility as `port` — one preview has one audience.
- Sandbox create waits for `port` only — an extra port that listens later answers 502 for a moment, which does not fail the preview.
- The PR comment lists every port with its URL, and the output `urls` is a JSON map from port to URL — a reviewer and a later step both find each hostname.
- A frontend finds another port's hostname from its own `Host` header as `<random>-<port>.<zone>` — every hostname of one sandbox shares its stem (`internal/sandbox/publish.go:70`).

## Out
- Env on templates or template setup steps.
- OCI image `Config.Env`.
- Changing `env` on a running or sleeping sandbox.
- Multiline values in the Action input.
- Per-tenant host secrets.
- A flag that sends `env` to fork previews.
- Writing files into a sandbox before the start command runs.
- A different visibility per port.
- Readiness checks on extra ports.
- Passing published hostnames to the start command.

## How I know it works
- `POST /v1/sandboxes` with `env: {"GREETING": "hi"}` and a start command that serves `$GREETING` returns `hi` on the published hostname.
- `exec` of `sh -c 'echo $GREETING'` in that sandbox prints `hi`.
- `GET /v1/sandboxes/{id}` shows `GREETING` in the key names and `hi` nowhere in the body.
- `grep -r hi` over the SQLite file and the gateway audit log finds no value from `env`.
- A create with `secrets: ["X"]` and `env: {"X": "y"}` returns `invalid` naming `X`.
- A fork of that sandbox without `allowSecret` is refused.
- The sandbox sleeps and wakes, and `exec` still prints `hi`.
- A preview from the base repository with `env: DATABASE_URL=...` gets the value, and the Action log shows `***`.
- A preview from a labelled fork gets no `env`, and the Action log says so.
- A line `NOEQUALS` in the Action input fails the step, and the host log shows no create call.
- A preview with `port: 3000` and `publish: 8080 9000` comments three URLs: `<random>.<zone>` for 3000, and `<random>-8080.<zone>` and `<random>-9000.<zone>`.
- The `urls` output of that run parses as JSON with keys `3000`, `8080` and `9000`.
- A request to `<random>-8080.<zone>` reaches the process on guest port 8080.
- `publish: 70000` fails the step before any call to Kiln.
- A workflow with no `publish` input gives the same comment and `url` output as before.
