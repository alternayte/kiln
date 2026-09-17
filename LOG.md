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
