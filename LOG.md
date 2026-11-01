# LOG

Orientation only. The gate tags decide what is done. Append at the end of a
session. Never rewrite an earlier entry.

## 2026-09-14 — P5 closed, P6 not started

Done: P5 lifecycle and reconcile. `gate/P5` is tagged at 0c740ca. The gate
passed on CI: a persistent sandbox slept and woke with files and memory
intact, an ephemeral one died at its TTL, a SIGKILL with three running
sandboxes and one mid-create converged on restart, the stray TAP was swept,
and the leak check was clean.

Half-finished: P6 public ingress is not started. Two inputs are missing.

1. The preview zone and the company that runs its DNS. The ingress imports
   exactly one DNS client package for the ACME DNS-01 challenge. The build
   cannot compile until that one package is named. See the note below.
2. A server for `just gate P6` and `just demo`. The operator has not set one
   up yet. Do not look for a host, and do not run either command, until the
   operator says the server is ready.

Tried and failed: nothing.

Next: when the operator names the preview zone and its DNS company, build
`internal/ingress`, the `published` table, the publish API, the status page
and `scripts/demo.sh`, then run `just gate P6` and `just demo` on the server.

Gate: no `gate/P6` tag.

## Note: the P6 DNS provider

The P6 spec says "v1 imports the one lego provider the operator uses".
CertMagic v0.25.4 no longer uses lego. `certmagic.DNS01Solver` takes a
libdns provider. Import exactly one, for the company that runs the preview
zone. Package paths, one per company:

- Cloudflare: `github.com/libdns/cloudflare`
- Gandi v5: `github.com/libdns/gandiv5`
- Google Cloud DNS: `github.com/libdns/googleclouddns`
- OVH: `github.com/libdns/ovh`

The operator's live zones are split across Cloudflare, Gandi, Google Cloud
DNS and GoDaddy. Do not guess. Ask which zone the previews use.

## 2026-09-14 — P6 built, gate and demo held for the server

Done: P6 public ingress, on `main`. `internal/ingress`
(hostname routing, Auth-All viewer login at `/_kiln/auth/`, wake-on-request
with the request held, the two wake caps, header rules), the `published`
table and the publish/retire API, the read-only status page at `GET /`, the
`client` publish methods, `scripts/demo.sh` and `tests/p6/gate_p6_test.go`.
`just check` passes, and CI ran `check` and the P0 to P5 gates green.

The DNS client is Cloudflare: `github.com/libdns/cloudflare` through
`certmagic.DNS01Solver`, the one provider v1 imports. `config.json` carries
`zone` and `acme` (`email`, `dns_provider`, `credentials`).
`CLOUDFLARE_DNS_API_TOKEN` is the credential key. `kiln init` takes
`--zone` and `--acme-email`, copies the token from the environment, and
creates the first viewer from `KILN_VIEWER_EMAIL` and
`KILN_VIEWER_PASSWORD`. There is no self-signup: the ingress serves only
`/session`, `/sign-in/email` and `/sign-out` on the auth path.

Held: `just gate P6` and `just demo` need a server with a zone, wildcard DNS
for `*.<zone>`, an ACME account and reachable port 443. The operator has not
set one up. Do not look for a host, and do not tag `gate/P6`, until the
operator says the server is ready. When it is:

    sudo go run ./cmd/kiln init --zone <zone> --acme-email <address> \
      # with CLOUDFLARE_DNS_API_TOKEN, KILN_VIEWER_EMAIL and KILN_VIEWER_PASSWORD set
    sudo env "PATH=$PATH" just gate P6
    just demo

Gate: no `gate/P6` tag. P6 is attended, so the operator runs the gate.

## 2026-09-14 — P6 review findings, fixed

`just review gate/P5` ran against the P6 diff (axis 1 openai, axis 2 spec).
Axis 1 found five bugs. Four were real and are fixed; the fifth was a wording
reading kept deliberately.

1. Later published ports drew a fresh random value. The spec's `<random>` is
   one stem per sandbox, so port 8001 is `<stem>-8001`. Fixed in
   `internal/sandbox/publish.go`, and the P6 gate already asserted this.
2. The `_in` chain had no `ct state established,related accept`, so the
   guest's replies to host-originated ingress connections hit the final drop.
   Public previews could not answer. Fixed in `internal/network/network.go`.
3. A retired hostname could be handed out again. Migration
   `0005_retired_hostnames.sql` adds a tombstone table; retire and sandbox
   destroy write tombstones, and publish refuses a tombstoned hostname.
4. `scripts/demo.sh` used `pgrep -c ... || echo 0`, which prints `00` when
   there is no process, so the zero-leak assertion failed on a clean host.
   The checks now use `grep -q` and empty-output tests.
5. The status page's unit table was `KiMGTPE`, so a mebibyte rendered as
   `iiB`. Fixed and unit tested.

Axis 2 read "forwards no host credential" as "forwards no Host header". The
proxy keeps the preview hostname in the guest's Host header on purpose: a
dev server needs it to build correct URLs, and the preview hostname is not a
credential. Only the machine identity of the host stays hidden.

Also before the server: `infra/README.md` has the preview setup checklist,
and the ingress now cleans the request path before the control-path check,
so `//v1/...` cannot slip past it.

## 2026-09-14 — the security gate

`just gate sec` had no `TestSec` tests, so it failed closed. The P5 rule says
a change to `internal/network`, the jailer setup or the resume hooks needs
that gate in full, and P6 changed the network. `tests/sec/gate_sec_test.go`
now holds `TestSecControls`: C1 jailer (chroot, cgroup, seccomp, uid 65534),
C2 default-deny egress, C3 secrets never in a template and no secret fork by
default, C4 entropy and C5 clock on restore, C6 hostname randomness, one
stem per sandbox, no reuse of a retired hostname and the team session check
before the guest, C6a the wake cap, C7 the cgroup limits, C8 the guest
cannot reach the control port, and C9 the preview headers, credential
stripping and host-only guest cookies.

The gate does not need a zone or ACME: it runs the ingress handler in
process behind an httptest TLS server, so it runs in CI. `sec` is now a
matrix entry in `.github/workflows/kvm.yml`, and it passes there.

Bringing the gate up found three more bugs, two of them in the P6 gate:
- Both gates replaced every `MARKER` in the in-guest server script, which
  rewrote the variable name and gave Python a syntax error. The placeholder
  is now `__MARKER__`. The P6 gate would have failed on the VPS for this.
- The jail check asserted `readlink /proc/<pid>/root`. A process in its own
  mount namespace reports `/`, so the check now reads the jail root's
  directory listing: it holds `vmlinux` and `rootfs.ext4` and no host `/etc`.
- C6 retired the team hostname before the session checks, so the challenge
  answer was 404. Retirement runs last now.

## 2026-09-15 — v1 done on the server

Host: Scaleway Dedibox Start-2-L, Xeon D-1531, 32 GB, Ubuntu 24.04, kernel
6.8. The EUR 4.99 Atom plan cannot run Firecracker: no XSAVE, so every start
fails with `Missing KVM capabilities: 0x38`.

Every gate passes on it: P0 to P6 and sec. `just demo` passes all 18 steps
against `statapad.com`, with a real wildcard certificate.

P6 attended review: signed off by the operator after reading the team auth
check (`internal/ingress/ingress.go`, the session is checked before the proxy
runs) and the wake path (wake limit before any restore, the request held in
`DialGuest`).

Done-line decision: the operator's laptop is an M1, and Firecracker in Lima
needs nested virtualization, which Apple silicon has from M3. The VPS is the
only done-line host for v1. The SDD is unchanged.

Found on the server and fixed: reconcile read cgroup interface files as
sandbox cgroups (about 50s per serve start); a snapshot delete failed its
foreign key when a later snapshot named it as parent, after the files were
already gone; the sec clock check and demo steps 9 and 10 measured exec time
as drift or counted a jq newline as a value; jq 1.7 took the guest's `-c`.

`infra/kiln.service` runs `kiln serve` under systemd with `KillMode=process`.
A `systemctl restart kiln` adopted a running sandbox and kept its files.

AGENTS.md: the operator asked for the Run and What-this-is lines to match the
code.

## 2026-09-16 — v0.2.0 built, and the gateway is live

The spec `docs/specs/v02-public-control-api.md` is built in four chunks:
tenants on the host, the gateway, the contract with the agent surface, and
OAuth. The gateway runs on Coolify at https://api.statapad.com against the
Dedibox host over mTLS. A tenant created through the gateway lands on the
host, an API key of that tenant reads its own rows, and an access token from
the authorization code flow does the same.

Found by running it, not by reading it:
- A template build outlives its request, so it ran on the daemon context and
  wrote every template to the default tenant. The cap could never fire.
- A viewer created before tenants held none, so an upgraded host refused the
  viewer the operator already had. Serve adopts those viewers now.
- The plugin's SetRole speaks for an actor who already holds rights, which a
  new viewer does not.
- auth-all accepts its own access token only when the audience names the
  issuer, so the resource identifier is the issuer.
- The consent page bounced back to sign-in forever: needsSignIn describes
  the browser when the application asked, not now.

The deployment taught three things about a distroless image: Coolify runs a
pre-deployment command and a health check through a shell inside it, and it
stores one line per environment variable, which a PEM block cannot survive.
The gateway migrates at start, the health check is off, and the certificates
are read as base64.

The host: a firewall that opens 22, 443 and 8443 and nothing else, SSH keys
only after 755 password attempts in two hours, and the Dedibox image's open
DNS resolver stopped.

AGENTS.md carries two exceptions now. The auth-all API names a type
Organization and a plugin admin, so those two words are legal in code that
calls it.

## 2026-09-16 — v0.3.0, the web UI and a terminal

Done: the gateway serves a React SPA from its own binary, and a sandbox has an
interactive shell in the browser. The auth prefix moved to `/api/auth`, the
auth-all default, so the official TypeScript client works with no local
variant; every access token issued under the old issuer stopped being
accepted. The member role `viewer` became `reader`, because `viewer` already
names a preview login.

Found by running it, not by reading it:
- The gateway wraps its ResponseWriter twice, for the audit status and for the
  WWW-Authenticate header, and neither wrapper implemented http.Hijacker.
  httputil.ReverseProxy refused to switch protocols, so every terminal died
  at the upgrade. The audit code broke the feature it was watching.
- Coolify's Traefik advertises HTTP/3. A WebSocket over h2 or h3 needs
  Extended CONNECT, which net/http does not serve, so the browser hung in
  CONNECTING while the same request over HTTP/1.1 answered 101. quic-go
  advertises Extended CONNECT unconditionally; Go disabled the h2 equivalent
  by default in 1.24. Removing `--entrypoints.https.http3` fixed it.
- A build gave its VMs an id hashed from the template name, and the
  reconciler protected an id hashed from tenant/name. The two never matched,
  so every build was an orphan and any sweep inside the two to four minutes
  of a build killed the VM and purged its TAP. Three builds failed this way
  before the cause was clear. The same mismatch would have given two tenants
  building one name the same VM id.
- The terminal client forgave its retry budget the moment a socket opened.
  A socket that opened and died 140ms later therefore retried for ever, at a
  constant half second. The budget is forgiven only after the shell holds.
- A sandbox restored from a template built before the guest agent had a
  terminal refuses the op, and the host closed with no reason. The close now
  carries one, and names the rebuild that fixes it.
- /bin/sh is dash on a Debian image: no tab completion, no history, no arrow
  keys, and a bare # for a prompt. The agent takes bash when the image has it.
- A session can hold memberships and no active tenant. Every /v1 call answered
  403 and the screens sat empty. A chooser stands in the way now.

The demo that closes this: on the Dedibox, a template built through the UI, a
sandbox created from it, and a shell in the browser running `uname -srm` and
reporting `stty size` as the window narrowed.

## 2026-09-17 — v0.4.0, preview environments

Done: a pull request, or an agent that finished a feature, gets a running
copy of the application on a public hostname. A tenant stores a registry
credential with `POST /v1/registries`; the host pulls the image with the
credentials of that tenant and no other, and the sandbox never holds the
token, because the sandbox runs the code under review. `POST /v1/templates`
takes `ttl_seconds`, and the reconciler deletes a template past its deadline
with its sandboxes and its snapshots. `.github/actions/preview` drives all of
it from a workflow, and `docs/preview-environments.md` and `llms.txt` carry
the same recipe for an agent.

The spec said the token is "encrypted at rest with the host key". There was
no such key, and no encryption anywhere in the repo. There is one now, in
config.json, which is 0600. Checking that turned up the thing worth knowing:
SQLite created kiln.db 0644, so every local user could read every tenant's
rows. The store takes it to 0600 on open.

Found by running it, not by reading it:
- A build gave its VMs an id hashed from the template name, and the
  reconciler protected an id hashed from tenant/name. The two never matched,
  so every build was an orphan and any sweep inside the two to four minutes
  of a build killed the VM and purged its TAP. Three builds died before the
  cause was clear. The same mismatch would have given two tenants building
  one name the same VM id. Fixing Runtime.Start left Network.Attach on the
  old id, and the next build died the same way; one id now names the VM, the
  attachment and the protection.
- The startup sweep reported "8 destroyed, 0 templates" while that same sweep
  had deleted a template, its running sandbox and a 512MB snapshot. The
  templates count was the stuck-template count, and the expired ones had no
  count at all.
- A build no longer loads the operator's Docker credentials at all. A tenant
  with no stored credential pulls anonymously, which a public image allows.

Not verified here, and not claimed: anything that needs a real pull request.
The comment edited in place and the fork label flow are syntax-checked only.

## 2026-09-17 — v0.5.0, the start command

Done: a template names the command that runs its application and the port it
listens on. A sandbox created from that template comes up serving, so a
published hostname answers on the first request.

Found by wiring the preview Action into a real repository, which is the only
reason it was found at all: Kiln boots kilninit as PID 1 and ignores what the
image says to run. A sandbox restored from a template held the application's
files and ran nothing, so every preview environment of v0.4.0 published a
dead port. The feature shipped and could not have worked.

- The start command runs only for a template snapshot. A wake and a fork
  restore memory that already holds the process, and a second copy would
  fight for the port. Measured after a sleep and a wake: one server, still
  serving.
- exec kills its process group when the call ends, so the application would
  have died with the request that started it. kilninit gained a start op that
  detaches it into its own session.
- Creating a sandbox answers only once the guest accepts a connection on the
  port. It took 8 seconds against a Debian image, against 5 for a template
  with no start command.
- A start command that exits before it listens fails the sandbox and the
  error carries the application's own output, not just an exit code.
- The watch that reports a crash is a connection to the guest, so it died
  with the VM at every sleep, and only a restore from a template took it up
  again. A sandbox that slept once reported no crash for the rest of its
  life. Every restore takes the watch up now.

Not verified here, and not claimed: the Action against a real pull request.
The repository to try it on is restitch-gateway, which serves an API, has a
Dockerfile, and calls upstreams that exercise egress_allow.

## 2026-09-17 — v0.5.1, the Action proved against a real pull request

Done: the preview Action ran on alternayte/restitch-gateway#30 and a person
clicked the link. The whole chain worked unattended: build and push to ghcr,
store a pull credential that expires with the job, build a template with a
start command, wait for the port, create the sandbox, publish, comment the
URL. The composition endpoint answered, composing two upstreams that
egress_allow named and nothing else.

Found by running it, not by reading it:
- The Action never passed a start command, so every preview it made published
  a port with nothing behind it. That is what sent v0.5.0 back to the guest
  agent.
- The Action's helper was called json.py. A script's own directory comes
  first on sys.path, so import json imported the script itself and every
  dumps call failed. It is payload.py now.

Confirmed against the pull request, not asserted: the comment is edited in
place rather than repeated, a later commit replaces the preview and retires
the old hostname, and closing the pull request leaves no template, no sandbox
and a hostname that answers 404.

The Action is pinned by tag in the documentation now. A workflow on @main
tracks every push to this repository, and a preview that breaks on somebody
else's commit is worse than one that lags a release.

Still not verified: a pull request from a fork, which is the one path where a
stranger's code meets the tenant's API key.

## 2026-09-17 — v0.6.0, a tag is enough to install a host

Done: a tag publishes the kiln binary for linux/amd64 with a checksums file,
and the gateway image to ghcr. The binary carries the guest agent, so
`kiln init` writes it out instead of shelling to `go build`. A host needs no
Go toolchain and no source.

This closes a real trap, not only a packaging gap. `installKilninit` compiled
the agent from the source tree, and when the source was missing it kept
whatever binary already sat in the bin directory. A host could therefore run
an agent nobody chose, and the failure surfaced far from its cause: earlier
this session a sandbox refused a terminal because its template carried an
older agent, and the browser saw an abrupt socket close with no reason.

- The agent is built before the binary that embeds it, and it is not
  committed, the way the web bundle is not. `just check` rebuilds it.
- `kiln init` behaves the same in a repository and on a bare box. There is
  one install path.
- A tag runs `just check` and every gate first. A release binary is the one
  thing people run without reading the code.
- The publish step refuses a binary that does not report the tag, so a
  release cannot carry a version nobody bumped.
- `kiln version` prints that version, which is the constant the gateway
  already serves in its document.

Verified before tagging: the release binary, copied to the Dedibox with no
source and no Go, ran `kiln init` into a scratch root and wrote a working
guest agent.

## 2026-09-17 — v0.7.0, env and extra ports for previews

Done: `POST /v1/sandboxes` takes `env`, and the preview Action takes `env`
and `publish`. A preview that needs a per-PR database URL, or serves a
frontend and an API on two ports, now works from CI. Spec:
`docs/specs/preview-config.md`.

- `env` sits on sandbox create, not on the template. A template forks into
  many sandboxes, and a value on its row would reach every one of them.
- Kiln handles every `env` value like the host secrets. It reaches guest
  memory through the resume hooks and never reaches disk. The store keeps
  the key names, which `GET` returns as `env_keys`.
- A key in both `env` and `secrets` is refused. Either precedence would
  override one side with no error.
- A fork's preview gets no `env`. The label approves the code, not what the
  code can reach.
- `port` is published first and stays `url`. Each `publish` port shares its
  hostname stem, and `urls` maps every port to its URL.

Verified before tagging: every gate passed in CI on the feature commit,
including the new `TestGateP4/EnvFork`. Not yet verified: the Action against
a real pull request with `env` and `publish`.

## 2026-10-11 — release-readiness review, and the onboarding path verified

Done: a root `README.md` and `docs/gateway.md`, and the installation,
quickstart, configuration and API documentation checked against what the
binaries do. `just check` passes on a laptop with no KVM. No gate was run:
every gate needs a KVM host, and CI runs them on push.

Found by reading the code against the documents, not by running a VM:
- `POST /v1/templates` refuses a body without `vcpus`, `memory_mb` and
  `disk_mb`, and `POST /v1/sandboxes` refuses one without `lifecycle` and
  `idle_seconds`. The OpenAPI document, both SDKs, the MCP tool schemas and
  `llms.txt` called every one of them optional, so a client built from the
  contract failed its first call. They are required in `internal/apispec`
  now, and the generated files follow.
- The web UI's template form never sent the three resource fields, so every
  build from it was refused with `vcpus 0 is outside 1..32`. The regenerated
  client's types caught it; the form has the fields now, with defaults.
- `GET /v1/events` is a stream that never ends and starts at the latest
  event. The MCP `events` tool read it to the end, so a tool call hung the
  gateway request. The generated SDK `events()` methods did the same. The
  preview Action printed it after a failed build and hung the job. The tool
  reads for a bounded window and takes `after` and `sandbox_id`; the SDKs
  carry a streaming iterator in the handwritten layer; the Action reads
  with a time limit and `?after=0`.
- The MCP `writeFile` tool had no way to carry content and wrote empty
  files. It takes `content` now. The `terminal` operation was listed as a
  tool, and a WebSocket cannot be a tool call; it is left out.
- The OpenAPI `Event` schema named `from` and `to`; the host sends
  `from_state` and `to_state`. `readFile` and `writeFile` are octet streams
  in the document now, and `events` is `text/event-stream`.
- The release notes install the binary at `/usr/local/bin/kiln`, and
  `infra/kiln.service` ran `/var/lib/kiln/bin/kiln`. The unit names the
  release path now, and the from-source steps install there too. A box
  whose unit still names the old path keeps working until the unit file is
  copied.
- `kiln init` had no documented way to open the gateway listener:
  `gateway_addr` and `gateway_names` are hand-edited keys. `infra/README.md`
  says so now, and `docs/gateway.md` carries every gateway variable, which
  no document outside the spec named before.
- The Vite dev server forwarded `/auth`, a prefix that moved to `/api/auth`
  in v0.3.0, so `bun run dev` could not sign in. It forwards `/api` now.
- `checks/web.sh` compared the generated client against the git index, so
  a regenerated and uncommitted client failed as stale. It compares before
  and after generation now. `checks/sdk.sh` type-checks the handwritten
  TypeScript layer and parses the Python one; nothing compiled them before.
- `kiln init --help` exited 1 with `flag: help requested`, and a missing
  config said `serve:` whatever command asked. Both fixed.

The SDD gained a header, with `KILN_SPEC_CHANGE=1`, that names it the v1
design record frozen on 2026-10-11 and `docs/specs/` the build authority,
and lists what shipped after it; its body is unchanged, and the README says
which document wins.
`gate/P6` was tagged on the Dedibox on 2026-09-15 and never pushed, so
`just status` on a fresh checkout names P6 open. Neither SDK is published to a
registry. There is no `LICENSE` file, and `sdk/typescript/package.json` says
MIT; the operator decides the licence. `docs/specs/template-services.md`
and the two AGENTS.md lines it adds were untracked before this session and
are left as they were.

## 2026-10-11 — v0.8.0, a licence, a CLA and the contract corrected

Done: the host and the gateway are AGPL-3.0, and the Go client, both SDKs
and the preview Action are MIT, so calling Kiln never pulls the AGPL into a
caller's code. A pull request from anyone but the maintainer needs the CLA
in `CLA.md`, which `.github/workflows/cla.yml` asks for once and records on
the `cla-signatures` branch. `.github/workflows/sdk.yml` publishes
`kiln-sdk` to PyPI and `@alternayte/kiln` to npm on a tag through trusted
publishing, after a one-time registration on each registry.

- `exhausted` answers 429, not 507. A cap is a quota, and 429 is the status
  every client library backs off on.
- `GET /v1/events` pages a replay in batches of 500 and reads again at
  once while a page is full, so `?after=0` over a long history is not one
  tick that builds it all in memory. The store takes a limit. Tested.
- The TypeScript package imported `./generated` with no extension, which
  Bun resolves and Node.js does not; the emitted package could not be loaded
  by the runtime npm users have. It imports `./generated.js` now, and the
  emitted module was loaded under Node.js before tagging.
- The maintainer-only `just` recipes, `review`, `model` and the sync in
  `agents`, say what they need instead of failing on a missing binary.
- Three specs for what comes next, none built: `sandbox-labels.md`,
  `preview-tokens.md` and `start-health-check.md`.

Verified before tagging: `just check` on a laptop, and the kvm workflow
green on the commit before this one, P0 to P5 and sec. Not verified here:
the two registry publishes, which need the registrations; and the live
host, which keeps the old unit path until its binary is installed at
`/usr/local/bin/kiln`.

## 2026-10-30 — v0.9.0, build timing, a credential per build, agent controls and deployments

Done: the four specs from issues #15, #16 and #17.

- `docs/specs/template-build-phases.md`: the host records one event per phase
  of a build and a restore, with `phase` and `duration_ms`, and a template
  event now names its template. The Action polls every 2 seconds and prints
  the phases.
- `docs/specs/template-build-credential.md`: `POST /v1/templates` takes
  `credential`, which lives in memory for the build. The Action takes
  `registry-token`. The audit row records its presence.
- `docs/specs/agent-sandbox-controls.md`: a template names a `user`.
  `PUT /v1/sandboxes/{id}/egress` replaces the policy, persists it on the row
  and on later snapshots, and cuts what the guest opened. `*.example.com` is
  valid. Detached processes live in guest memory, and the idle deadline asks
  the guest before it sleeps or destroys a sandbox.
- `docs/specs/preview-deployments.md`: the Action reports a GitHub deployment.

Things the build taught:

- `pull` streams each blob straight into the tree, so `layers` is one phase.
- The host needs `conntrack`, a new package, to cut a connection. nftables can
  drop a packet and cannot end a flow, and every guest shares one source
  address, so the firewall mark names the sandbox.
- A rename replaces a symlink and leaves its target alone, so a write through
  a planted link silently destroyed the link. With a template user the write
  now checks the target first and refuses.
- `reader` is not enforced per route by the gateway today, so the new routes
  have the same access as exec without any new code.

Verified before tagging: `just check`, and the kvm workflow green on the same
tree, P0 to P5 and sec. `TestSecAgentControls` in the sec gate drives the user,
the egress change and the processes on real microVMs. The same address can
serve a removed host and an allowed one on a CDN, and the policy cannot tell
them apart once the allowed name resolves. Not verified: a real preview on
c-dash, the deployment calls against GitHub, and two private repositories
building together, because they need the live host on this version. The live
host needs `conntrack` installed before it takes an egress change.

## 2026-10-30 — v0.9.1, a stop waits for the process to release its TAP

v0.9.0 is a tag with no release: its gates failed, so nothing published. Two
gates failed with `Resource busy` on the TAP at the snapshot boot of a build.
The two boots of a build share one TAP. When a guest overran the shutdown grace
the host killed it, and `processAlive` read a zombie main thread as a process
that had gone while its other threads still held the TAP. A zombie with other
threads now counts as alive, and a stop that cannot see the exit says so. The
bug is old, and this is the first time it was seen in the kept CI logs. The
timing fits a slow shutdown; the failure was not reproduced on demand.

## 2026-10-30 — CI spends less on repeat work

`kvm` no longer runs on a tag, because the release workflow runs the same
gates. A push that changes only `docs/`, `web/`, `sdk/`, `*.md` or
`.github/actions/preview/` runs `check` and skips the gates. The kvm jobs use
the Go cache. `gh workflow run kvm -f gate=sec` runs one gate. The release
workflow is unchanged, so every release still runs every gate from a clean
build.
One run per ref is kept, so a hand-run gate cancels a run in progress on the
same branch.

## 2026-10-30 — v0.9.2, a layer cache and a guest that ends without a kill

Done: `docs/specs/template-layer-cache.md`. The first c-dash preview with phase
timing spent 70 s of 130 s in `layers`, so each tenant now keeps its layers, up
to 4 GiB. It also spent 5.1 s in a `setup` phase with no commands, which was the
shutdown grace: Firecracker does not exit on a guest power off, so every boot of
every build was killed. `kilninit` now restarts the guest, and Firecracker exits
on that reset. This is also the likely source of the v0.9.0 `Resource busy`
failure, because it made the kill path the normal path. The kvm workflow went
from about 8 minutes to about 4.

## 2026-10-30 — v0.9.3, the layer cache holds plain tars

The v0.9.2 cache missed its goal. With the Docker build cache on, 8 of 9 layers
were cached and `layers` still took 89 s, against 105 s with none cached. The
cost is decompression, not the download: on the Dedibox one 53 MB layer takes
14 s to gunzip and 2.4 s to untar on tmpfs. tmpfs itself saves almost nothing.
The cache now holds each layer decompressed. The file name carries the digest
of the tar, because the layer digest covers bytes the cache no longer holds.

## 2026-10-30 — v0.9.4, a failed run removes its template, and the data disk

With the plain-tar cache, `layers` fell from about 90 s to 51 s, and a preview
from about 140 s to 100 s. The third test commit then failed with `exhausted`:
`/var/lib/kiln` was on the 20 GB root partition, and a run that failed at the
sandbox step left its 1.6 GB template behind, invisible to `prune.sh`. The
Action now removes the template of a failed run. On the Dedibox the data moved
to the 213 GB `/data` by bind mount, and `infra/README.md` says how.

## 2026-10-31 — v0.9.5, a snapshot of a large sandbox

Issue 18: one 2 minute client timeout covered each Firecracker call, and a
snapshot of a 4096 MB restored sandbox did not fit. Snapshot create and load now
get 2 minutes plus 120 ms per MiB of guest memory. Firecracker cannot abandon a
write, so a client that leaves no longer cancels the snapshot, and a resume no
longer depends on the context of that client. Before this, a cancel left the VM
paused.

## 2026-10-31 — v0.9.6, a snapshot loads its pages in blocks

A snapshot of a 4096 MB restored sandbox took 138 s. Firecracker reads all of
guest memory, and that caused 1,048,649 faults, one per 4 KiB page, at about
31 MB/s. `snapfault` now takes a SIGUSR1 just before a snapshot, a fork or a
sleep, and copies the untouched pages in 2 MiB blocks between faults. Restores
stay lazy. A `snapfault` from an older binary ignores the signal.

## 2026-10-31 — v0.9.7, metadata on a restore or a fork

Issue 19: a copy kept the metadata of its origin, which named the wrong
Factoree run. Restore and fork now take an optional `metadata` that replaces it.
Issue 20 is not a restart fault: the event log shows `sleeping` to `stopping`
with reason `destroy` at 06:36:23, from a `DELETE` the operator asked for after
a run filled the tenant cap. The last restart was at 06:28:22.

## 2026-10-31 — v0.10.0, a client called kiln and a host called kilnd

Done: `docs/specs/agent-cli.md`. The host binary and its unit are now `kilnd`,
with no alias, and `kiln ctl` is gone. `kiln` is a new client for Linux and
macOS with one command per API operation, read from `internal/apispec`. A
generator check fails on an operation with no command. It has a fixed exit code
table, `--wait`, `kiln preview up` and `down`, `kiln logs` over a new route, and
`kiln init`. The preview Action now downloads `kiln` and calls it, and
`payload.py` and `prune.sh` are gone. The `upgrade` gate runs the latest release
beside this build. `kiln egress` waits for `docs/specs/egress-guard.md`, and
service output in `kiln logs` waits for `docs/specs/template-services.md`.

The `sdk` workflow has failed on every tag so far. PyPI answers
`invalid-publisher` and npm answers E404, because no trusted publisher is
registered for `kiln-sdk` or `@alternayte/kiln`. That registration needs the
maintainer's PyPI and npm accounts.

## 2026-10-31 — template services, and a fork that keeps its disk

Done: `docs/specs/template-services.md`. A template names service images. Each
unpacks into its own root, kilninit runs it chrooted there, and the template
snapshot holds the running stack with its seed data.

The spec exposed an older fault. A fork, a restore of a listed snapshot and a
template all restored onto a blank drive, while a fork's memory had the original
drive mounted. Files survived only while the page cache held them, which is why
P4 passed. A snapshot now copies the drive while the guest is paused, and each
copy restores onto its own copy. A fork therefore pauses its origin for longer,
and the drive counts toward `max_snapshot_bytes`. Listed snapshots from before
this have no copy and keep the old fault.


## 2026-11-01 — egress guard

Done: `docs/specs/egress-guard.md`. Every port 443 connection of a sandbox goes
to `kilnd egressguard`, one process per sandbox, which reads the TLS name and
dials the address it resolved itself. The guard survives a daemon restart, a
dead guard fails closed and restarts, and a sandbox adopted from before the guard
keeps its address set until its next wake. The host keeps an egress record, read
at `GET /v1/sandboxes/{id}/egress/log` and with `kiln egress <id>`. `PUT .../egress`
takes `{"observe": true}`, and `kiln egress <id> --propose` prints a list.

Refused direct addresses come from a rate-limited nftables log line that the
daemon reads from `/dev/kmsg`, so these lines also reach the host journal.
Egress records also cap at 4096 destinations per sandbox, with the rest counted
as `dropped`, because a guest chooses any number of names.

Next: `docs/specs/credential-injection.md`. It terminates TLS in this guard.

Done: `docs/specs/credential-injection.md`. `PUT /v1/sandboxes/{id}/credentials`
binds a sealed credential to one exact host, and the guard of that sandbox
terminates TLS on that host alone and sets the header on each matching request.
A tenant holds one CA, a template build adds it to the application root, and
a template built before this returns `conflict` on the call. `GET
/v1/sandboxes/{id}/egress/requests` reads the per-request log. A fork or a restore
starts with no binding and takes its own `credentials` list.

Also fixed here: a failed row no longer counts as swept at each start; an operator
create names only its own tenant's template and answers `not_found`; a copy takes
the tenant of its template on an operator call; a reconciler failure detaches the
attachment, so its guard does not restart; a guard restart whose rules fail stops
that guard; an address family the host cannot route no longer uses a whole dial;
a call the agent of an old template does not know answers `conflict`.

Done: `docs/specs/sandbox-timeline.md`. `POST /v1/sandboxes/{id}/timeline` marks a
point on a sandbox of a template with `timeline: true`. The first point is a full
snapshot and later points are diffs. `GET` lists the chain, `DELETE` merges a point
into the next one, `rewind` restores a point under the same id and hostnames, and
`fork` takes `point`. A wake, a snapshot or a fork of the origin resets the dirty
pages, so the next point after one is a full snapshot. `kilnd init` installs
`snapshot-editor`. The measured numbers the spec asks for are not recorded yet.

Fixed: guests share one address and one gateway, so a tuple one sandbox used found
that sandbox's conntrack entry in another, and its DNS got no answer for up to 120
seconds. Each sandbox now has its own conntrack zone, and the `sec` gate asks from
one source port in two sandboxes. This is not proven to be the cause of the DNS
failure in release run 41352301252. The resolver now logs an upstream SERVFAIL, so
the next occurrence names that layer or rules it out.

Measured: `sandbox-timeline` on the Dedibox, v0.13.2, one run, a 4096 MB guest with
2 vCPUs and 3 GB held in a process.

| Step | Call | Guest paused | Point bytes |
|---|---|---|---|
| Full point | 35.4 s | 9.5 s, after 25.9 s of page fill | 4298 MB |
| Diff, nothing changed on purpose | 0.38 s | 0.33 s | 4 MB |
| Diff after 64 MiB | 1.08 s | 1.00 s | 76 MB |
| Diff after 256 MiB | 3.15 s | 3.09 s | 278 MB |
| Diff after 1024 MiB | 10.16 s | 10.10 s | 1085 MB |

Pause is the logged write time, which holds the guest paused and copies the drive.
A rewind that merged a chain of 5 points took 64.7 s, and a second rewind that read
the kept result took 1.28 s. A fork of one copy from that point took 0.74 s.

Dirty page tracking under load: three passes that write each page of 2048 MiB, from
Python, with no tracking and with it. Warm passes took 5.54 s and 5.63 s with no
tracking, and 6.16 s and 5.90 s with it, about 8% on this one pattern. The first
pass took about 50 s either way, because it faults in the memory.

Not recorded: how long Factoree model calls last, from its `AgentSession` rows, so
the model-wait parking condition has no answer yet.
