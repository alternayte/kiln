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

Done: P6 public ingress on the branch `p6-public-ingress`. `internal/ingress`
(hostname routing, Auth-All viewer login at `/_kiln/auth/`, wake-on-request
with the request held, the two wake caps, header rules), the `published`
table and the publish/retire API, the read-only status page at `GET /`, the
`client` publish methods, `scripts/demo.sh` and `tests/p6/gate_p6_test.go`.
`just check` passes.

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
