# P6 public ingress

## What it does
Serves published sandbox ports over HTTPS on a random hostname, with a wildcard certificate. A request to a sleeping sandbox's hostname wakes it and waits. Team previews require a viewer session. `just demo` runs end to end.

## Decisions
- The ingress listener binds `0.0.0.0:443` and serves published hostnames only. A control path on that listener returns 404 and is never routed.
- The control listener stays on `127.0.0.1:8080` and is never bound to a public address.
- Certificates come from ACME DNS-01 through CertMagic. `config.json` also holds `acme.email`, `acme.dns_provider` and that provider's credentials. v1 imports the one lego provider the operator uses.
- Hostnames are `<random>.<zone>` for the first published port and `<random>-<port>.<zone>` for the others. `<random>` is 128 bits from a CSPRNG. It is never derived from a sandbox ID, branch name, PR number or commit. A retired hostname 404s forever and is never reused.
- `POST /v1/sandboxes/{id}/publish` takes `port` and required `visibility` (`public` or `team`). Republishing the same port is idempotent and returns the same URL. `GET /v1/sandboxes/{id}` returns the published ports with their URLs and visibility. `DELETE /v1/sandboxes/{id}/publish/{port}` retires the hostname. Publishing dies with the sandbox.
- `public` needs no credential. The URL is the credential.
- `team` requires a viewer session. Auth-all runs in this process against the same SQLite file and mounts at `/_kiln/auth/` on every published hostname. The session cookie is scoped to `.<zone>`, so one login covers every preview. The operator creates the first admin at init; there is no self-signup and no email flow. The proxy checks the session before it forwards, so an unauthenticated request never reaches the guest.
- A request to the published hostname of a sleeping sandbox is held while the sandbox wakes, then forwarded. The client sees one slow request. A failed wake returns 503.
- Wakes are limited per subdomain to 10 per rolling minute and host-wide to 4 restores in flight. Over either limit the proxy returns 503 with `Retry-After: 30` and queues nothing.
- The proxy strips hop-by-hop headers in both directions and forwards no host credential, cookie or control token into the guest.
- Every preview response carries `Content-Security-Policy: sandbox` and `X-Robots-Tag: noindex`.
- `GET /` is a read-only HTML status page with sandboxes, templates and resource use. No controls, no forms, no JavaScript build step.
- `scripts/demo.sh` runs the full sequence and prints the create wall time against the last recorded value. It asserts no timing bound.
- The `published` table lands here.

## Out
- No public control API. That is v1.1.
- No per-preview viewer lists. Every viewer account may open every team preview.

## How I know it works
- `just gate P6` on the VPS: a public preview is reachable with no credential; a team preview challenges, and the guest never sees the unauthenticated request; a retired hostname 404s; `/v1/sandboxes` on port 443 returns 404; the control listener is not public; the guest cannot reach the control API; every response carries `noindex`; a request over the wake cap returns 503.
- `just demo` passes unmodified on the VPS.
