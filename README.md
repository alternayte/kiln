# Kiln

Self-hosted microVM sandboxes for coding agents and for CI. One Go binary on
one Linux host with KVM runs isolated Firecracker microVMs, built from OCI
images into snapshots, restored in under a second, forked into copies, put to
sleep when idle, and served on public hostnames.

Two things use it:

- **An agent** gets a sandbox to run untrusted code in, keeps its files and
  memory between calls, and forks it to try several branches at once.
- **A pull request** gets a running copy of its application on a hostname, so
  a reviewer clicks a link and an agent proves its change runs. The GitHub
  Action in this repository does that end to end.

## How it works

- A **template** is an OCI image built once into a root filesystem plus a
  memory and state snapshot. Creating a sandbox restores that snapshot. There
  is no cold boot on the request path.
- A **sandbox** is a Firecracker microVM with its own kernel, launched under
  `jailer`, in its own cgroup, behind its own nftables chain. Egress is deny
  by default, and a template names the hostnames its sandboxes may reach.
- **Sleep** is a snapshot. **Fork** is a restore done N times. Forks share
  clean memory pages through userfaultfd, so eight forks of a 512 MB
  template do not cost 4 GB.
- A **published** port gets `https://<random>.<zone>/`. A request to a
  sleeping sandbox's hostname wakes it and waits.
- The host keeps one SQLite file, and a restart adopts the microVMs that are
  still running. A crashed daemon leaks nothing: the reconciler destroys
  every host resource no row owns.

The design and its reasons are in [`docs/specs/`](docs/specs/), one file per
slice. [`docs/internal/sdd.md`](docs/internal/sdd.md) is the frozen v1 design
record and is older than the code in places; the specs win where they differ.

## Requirements

- An x86_64 Linux host with `/dev/kvm`. Bare metal, or a VM with nested
  virtualization. A CPU without XSAVE cannot run Firecracker.
- Root. `jailer` needs it; it drops each microVM to its own uid.
- Ubuntu 24.04 or Debian 12 with `iproute2`, `nftables`, `e2fsprogs` and
  `curl`. `scripts/dev-env.sh` installs them.
- For public hostnames: a zone whose DNS is on Cloudflare, with a wildcard
  record that points at the host, and port 443 open. See
  [`infra/README.md`](infra/README.md#preview-setup-p6).

## Install a host

A release carries one static binary with the guest agent inside it. No Go
toolchain, no source.

```sh
curl -fsSLO https://github.com/alternayte/kiln/releases/latest/download/kiln
curl -fsSLO https://github.com/alternayte/kiln/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS
sudo install -m755 kiln /usr/local/bin/kiln
sudo kiln init
sudo kiln serve
```

`kiln init` creates `/var/lib/kiln`, fetches the pinned Firecracker and
kernel, verifies their checksums, writes the guest agent, applies the nftables
base, creates the database, and writes `config.json` with the operator token. It
is idempotent, and a second run keeps the token.

`kiln serve` binds the control API to `127.0.0.1:8080`. It is never bound to
a public address; remote access goes through the gateway below or a tunnel.

To run it as a service:

```sh
sudo install -m644 infra/kiln.service /etc/systemd/system/kiln.service
sudo systemctl daemon-reload
sudo systemctl enable --now kiln
```

`systemctl restart kiln` keeps every sandbox running. To upgrade, install the
new binary over the old one and restart.

## The first sandbox

Every call carries the operator token from `config.json`. These run on the host.

```sh
TOKEN="$(sudo jq -r .bearer_token /var/lib/kiln/config.json)"
api() { curl -sS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }

# 1. Build a template. The build runs after the 202; the first one pulls the
#    image and boots it once, so it takes a few minutes.
api -X POST localhost:8080/v1/templates -d '{
  "name": "py312",
  "image": "docker.io/library/python:3.12-slim",
  "vcpus": 1, "memory_mb": 512, "disk_mb": 2048,
  "egress_allow": ["pypi.org", "files.pythonhosted.org"]
}'

# 2. Wait for it.
until [ "$(api localhost:8080/v1/templates/py312 | jq -r .state)" = ready ]; do sleep 5; done

# 3. Create a sandbox. lifecycle and idle_seconds are required: an ephemeral
#    sandbox is destroyed after idle_seconds of no activity, a persistent one
#    sleeps then and wakes on the next request.
ID="$(api -X POST localhost:8080/v1/sandboxes \
  -d '{"template":"py312","lifecycle":"ephemeral","idle_seconds":600}' | jq -r .id)"

# 4. Run something.
api -X POST "localhost:8080/v1/sandboxes/$ID/exec" -d '{"cmd":["python3","-c","print(2+2)"]}'

# 5. Destroy it.
api -X DELETE "localhost:8080/v1/sandboxes/$ID"
```

`kiln ctl` is the same API from the command line: `kiln ctl health`,
`kiln ctl templates`, `kiln ctl sandboxes`, `kiln ctl exec <id> -- <cmd>`,
`kiln ctl events`, `kiln ctl destroy <id>`.

`egress_allow` is required on every template. An empty list allows no egress,
and DNS fails inside the sandbox. The guest reaches only the names on the
list; `169.254.169.254` is refused whatever the list says.

A template with a `start` command and a `port` comes up serving: creating a
sandbox from it answers only once the guest accepts a connection on that
port. Kiln boots its own init and ignores what the image says to run, so an
application template needs both.

## Publish a port

Publishing needs a zone. Run `kiln init` again with it, with the Cloudflare
token in the environment, then restart `kiln serve`:

```sh
export CLOUDFLARE_DNS_API_TOKEN=...   # Zone:Read and Zone:DNS:Edit on the zone
sudo -E kiln init --zone example.com --acme-email you@example.com
```

```sh
api -X POST "localhost:8080/v1/sandboxes/$ID/publish" -d '{"port":8000,"visibility":"public"}'
```

The answer carries `https://<random>.example.com/`. `public` needs no
credential: the random subdomain is the credential. `team` needs a viewer
login, which `kiln init` creates from `KILN_VIEWER_EMAIL` and
`KILN_VIEWER_PASSWORD`, or `POST /v1/viewers` later. The whole setup is in
[`infra/README.md`](infra/README.md#preview-setup-p6).

## The gateway

The host alone is one operator with one token on one box. The gateway is the
public face: it holds tenants, API keys, members, OAuth, rate limits and an
audit log, serves the web UI with a terminal into each sandbox, serves the
same API to agents over MCP, and forwards `/v1` to the host over mTLS. A
self-hosted install that needs remote access, more than one person, or an
agent runs it. [`docs/gateway.md`](docs/gateway.md) is the setup.

## The API, the SDKs and agents

The API is `/v1`, JSON in and out, one bearer credential per call. Errors are
`{"error":{"code":"...","message":"..."}}` with a stable code: `invalid`,
`not_found`, `conflict`, `exhausted`, `internal`.

- [`sdk/openapi.json`](sdk/openapi.json): every route, body and error.
- [`sdk/llms.txt`](sdk/llms.txt) and [`sdk/llms-full.txt`](sdk/llms-full.txt):
  the same for an agent. The gateway serves both, and
  `/.well-known/ai-catalog.json`.
- [`sdk/python`](sdk/python) and [`sdk/typescript`](sdk/typescript): a
  generated client plus a handwritten layer for streaming exec, files, the
  event stream and a `Sandbox` object. Neither is published to a registry
  yet; install from the repository path.
- [`client`](client): the Go client, the package the gates use.

```python
from kiln import Kiln

kiln = Kiln("https://api.example.com", api_key="kiln_...")
kiln.build_template(name="py312", image="docker.io/library/python:3.12-slim",
                    vcpus=1, memory_mb=512, disk_mb=2048, egress_allow=[])
sandbox = kiln.create_sandbox_object(template="py312", lifecycle="ephemeral", idle_seconds=600)
print(sandbox.exec(["python3", "-c", "print(2+2)"]).stdout)
sandbox.destroy()
```

An MCP client points at `https://<gateway>/mcp` with an API key, or
registers itself and completes the OAuth code flow. Every API operation is a
tool.

`GET /v1/events` is a server-sent event stream that never ends. It starts at
the latest event; `?after=0` replays the tenant's history, `?sandbox_id=`
keeps one sandbox.

## Preview environments in CI

A pull request gets a running copy of its application on a hostname, and a
comment with the link. The Action builds nothing: you push the image, it
builds a template from it, runs one sandbox, publishes the port, and deletes
everything when the pull request closes.

```yaml
- uses: alternayte/kiln/.github/actions/preview@v0.7.0
  with:
    url: ${{ vars.KILN_URL }}
    api-key: ${{ secrets.KILN_API_KEY }}
    image: ghcr.io/${{ github.repository }}:${{ github.event.pull_request.head.sha }}
    port: "8000"
    start: /app/server --port 8000
```

[`.github/actions/preview/README.md`](.github/actions/preview/README.md) has
the workflow, the fork rule and the `env` and `publish` inputs.
[`docs/preview-environments.md`](docs/preview-environments.md) is the same
recipe as four API calls, for an agent.

## Configuration

`kiln init` writes `$KILN_ROOT/config.json`, mode 0600. Edit it by hand for
the keys `init` has no flag for, then restart `kiln serve`.

| Key | Default | Meaning |
|---|---|---|
| `bearer_token` | generated | The operator credential. `kiln ctl` and the gateway use it. |
| `control_addr` | `127.0.0.1:8080` | The control API. Keep it on loopback. |
| `ingress_addr` | `0.0.0.0:443` | The public HTTPS listener for published hostnames. |
| `zone` | none | The preview zone. `--zone` sets it. No zone refuses every publish. |
| `acme.email`, `acme.dns_provider`, `acme.credentials` | none | The ACME registration and the DNS credential. `--acme-email` and `CLOUDFLARE_DNS_API_TOKEN` set them. |
| `gateway_addr` | none | The mTLS listener a gateway dials, for example `0.0.0.0:8443`. Empty keeps the host private. |
| `gateway_names` | host part of `gateway_addr` | Hostnames and addresses in that listener's certificate. Required when `gateway_addr` binds every address. |
| `seal_key` | generated | Seals registry tokens before they reach the database. A lost key loses every stored token. |
| `secrets` | none | Host secrets by name. A sandbox created with `secrets: ["NAME"]` gets the value in its environment. |

Environment: `KILN_ROOT` (default `/var/lib/kiln`) for every command;
`CLOUDFLARE_DNS_API_TOKEN`, `KILN_VIEWER_EMAIL` and `KILN_VIEWER_PASSWORD`
for `kiln init`.

## Development

`just check` is the gate. It needs Go (the version in `go.mod`), `just` and
`bun`. It builds the guest agent, runs every script in `checks/`, `gofmt`,
`go vet`, the unit tests, the SDK generator check and the web build. It runs
on a laptop with no KVM.

The phase gates boot real microVMs. They need a Linux host with KVM and root,
and they run on every push in CI. On such a host:

```sh
sudo bash scripts/dev-env.sh
bash scripts/build-kilninit.sh
sudo go run ./cmd/kiln init
sudo env "PATH=$PATH" just gate P1      # P0..P6, or sec
```

The order is P0 harness, P1 boot, P2 template, P3 restore, P4 fork and
egress, P5 lifecycle, P6 public ingress. P6 and `just demo` need a zone and
run on the operator's server. `just status` lists the gate tags.

`just sdk` regenerates `sdk/` from `internal/apispec`, and `bun run generate`
in `web/` regenerates the UI's client from it. A route change without both is
a failed check. `bun run dev` in `web/` serves the UI against a gateway on
`localhost:8080`.

Where things are:

| Path | What |
|---|---|
| `cmd/kiln` | The host: `init`, `serve`, `ctl`, `ca`, `gateway-env`, `version` |
| `cmd/kiln-gateway` | The gateway |
| `internal/` | One package per feature: `api`, `template`, `sandbox`, `runtime`, `snapshot`, `network`, `ingress`, `reconcile`, `store`, `gateway`, `apispec` |
| `guest/kilninit` | PID 1 inside every microVM |
| `web/` | The React UI the gateway embeds |
| `tests/` | The phase gates, `kvm` build tag |
| `docs/specs/` | The build authority, one spec per slice |
| `LOG.md` | What each session did and found |
| `AGENTS.md` | Rules and domain words for agents working here |

## What is not here

- One host. No clustering, no second hypervisor, no GPU.
- x86_64 only. The pinned Firecracker and kernel are x86_64.
- No image builder. Kiln imports images something else built.
- No billing or metering. The `events` table records every transition.
- The host binary is the only release artifact; the gateway ships as
  `ghcr.io/alternayte/kiln-gateway:<tag>`.
