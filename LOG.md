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
matrix entry in `.github/workflows/kvm.yml`.
