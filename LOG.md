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
