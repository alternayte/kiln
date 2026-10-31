# Agent CLI

Amends `docs/specs/release-artifacts.md`.

## What it does
`kiln` is a client binary for people and agents, on Linux and macOS, that talks to a gateway or a host. It has one command per API operation, JSON output, a fixed exit code table, and one command that runs a preview environment. `kiln init` gives a repository the Kiln skill and the MCP config. The host binary becomes `kilnd`.

## Decisions
### Names
- The client is `kiln` and the host is `kilnd` — most callers use the client, and `kiln`, `kiln ctl` and `kilnctl` together confuse people.
- `kilnd` carries `init`, `serve`, `ca`, `gateway-env`, `version`, and the internal `snapfault` and `egressguard` — this is the current host binary with one command removed.
- `kiln ctl` goes, and `kiln` replaces it — one client has one command set.
- The rename is a hard break with no alias — `kiln` must become the client, and Kiln is before 1.0.
- `kiln-gateway`, `kiln-sdk` and `@alternayte/kiln` stay — they already name their packages.
- `kiln` reads `KILN_URL` and `KILN_API_KEY`. With no `KILN_URL`, it reads `$KILN_ROOT/config.json` when that file is readable — an operator on the host needs no setup.

### Commands
- The generator builds one command per operation from `internal/apispec`, and `checks/sdk.sh` fails on a route with no command — a route must not reach the API without the CLI.
- Output is text on a terminal and JSON with `--json` — people and agents read different forms.
- `--wait` works on a template build, with `--timeout` — an agent must not write a polling loop.
- `create`, `fork` and each error print the next command to run. With `--json`, the same text is in a `next` field — an agent recovers with no search of the docs.
- `kiln preview up --image X --port N` builds the template, waits, creates one sandbox, publishes the port and prints the URL. `kiln preview down` removes them — this is the edit, push, run and read loop.
- `preview up` takes the same inputs as the Action, and the Action downloads `kiln` and calls it — two implementations of one flow drift.
- `docs/specs/egress-guard.md` adds `kiln egress <id>`, with its route — the egress record does not exist before that spec.
- `kiln logs <id>` prints the start command, service and build output. It reads a new `GET /v1/sandboxes/{id}/logs` route that returns the output kilninit already keeps — no route returns that output today. `docs/specs/template-services.md` adds the service output.

### Exit codes
- The table is generated into the docs and the skill: 0 success, 1 API or network failure, 2 usage or configuration, 3 `not_found`, 4 `conflict`, 5 `exhausted`, 6 `invalid`, 14 `--timeout` ended the wait — an agent branches with no text parse.
- `kiln exec` returns the exit code of the command, and 125 when Kiln itself failed — a caller must tell the two apart, and Docker uses this convention.

### Agent setup
- `kiln init` writes `.claude/skills/kiln/SKILL.md`, a marked Kiln section in `AGENTS.md`, and a `.mcp.json` entry that names `${KILN_URL}/mcp` and `${KILN_API_KEY}` — the skill gives the work loop, and MCP gives the operations.
- `kiln init` keeps the rest of an existing `AGENTS.md` and `.mcp.json`, and keeps a changed skill unless `--force` is set — a tool must not overwrite a user's file.
- The binary embeds the skill, so the skill matches the binary — a stale skill teaches wrong commands.
- `kiln init` never writes the key to a file — a repository must not hold a credential.
- `sdk/llms.txt` and `sdk/llms-full.txt` gain the CLI and the exit codes — an agent with no skill still finds them.

### Release
- A release carries `kilnd` for Linux amd64, `kiln` for Linux and macOS on amd64 and arm64, and `SHA256SUMS` — the host needs KVM, and the client runs anywhere.
- The release workflow publishes `kiln-sdk` to PyPI and `@alternayte/kiln` to npm — an install from a repository path blocks adoption.
- `infra/kiln.service`, the docs, the scripts and the gates use `kilnd` — no live reference may name the old binary.
- `infra/README.md` gives the upgrade: stop the unit, install `kilnd`, remove the old binary, replace the unit, start it — every existing host needs these steps.

## Out
- A JSON Schema and a `validate` command. Kiln has no config file.
- A Cursor rule or any file for one agent other than Claude Code. `AGENTS.md` serves the others.
- A Homebrew tap.
- An E2B-compatible API.
- An alias from `kiln serve` to `kilnd serve`.

## How I know it works
- On macOS, `KILN_URL=... KILN_API_KEY=... kiln sandboxes list --json` parses as JSON.
- On a host as root, with no `KILN_URL`, `kiln health` answers.
- A new route in `internal/apispec` with no generated command fails `just check`.
- `kiln sandboxes get nope`, a usage error and a `--timeout` end return 3, 2 and 14.
- `kiln exec <id> -- sh -c 'exit 7'` returns 7. An exec on a stopped gateway returns 125.
- `kiln preview up` prints a URL that serves the application. `kiln preview down` leaves no template, sandbox or hostname.
- A failed start command makes `kiln logs <id>` print the last output.
- `kiln init` in a repository writes the skill, the marked section and `.mcp.json`, and leaves other `AGENTS.md` lines unchanged. A `grep` for the key finds nothing.
- A preview from the Action works unchanged after the Action moves to `kiln`.
- On a host at the previous release, the upgrade steps keep every sandbox running, and each `snapfault` keeps serving. A release gate proves this.
- `pip install kiln-sdk` and `npm install @alternayte/kiln` install the release version.
