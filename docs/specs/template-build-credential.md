# Template build credential

Closes the registry credential part of alternayte/kiln#15.

## What it does
`POST /v1/templates` takes an optional `credential` that pulls the images of that one build. The preview Action passes the job `GITHUB_TOKEN` this way. Two repositories in one tenant then build at the same moment without overwriting each other's credential, and no revoked job token stays on the host.

## Decisions
- `credential` holds `username` and `token`, and the registry is the host of the image reference — a second host field can only disagree with the reference.
- The host holds the credential in memory for the build and never writes it to the store, the manifest, an event or a log — the token has no use after the build.
- A build with `credential` uses it for the application image and for each image on the same registry host. Other hosts use the stored tenant credentials — a job token must not reach another registry, and `docs/specs/template-services.md` pulls service images in the same build.
- A build with no `credential` uses the stored tenant credentials, as today — Factoree and existing callers do not change.
- `POST /v1/registries` stays — a long-lived token still serves a template that a person or Factoree builds.
- The gateway audit log records that a credential was present and never its value — the gateway forwards this body.
- `GET /v1/templates/{name}` never returns the credential — a credential goes in and nothing reads it back.
- A later rebuild from a stored request does not exist — nothing in the host rebuilds a template, so an expired token has no effect.
- The Action gains `registry-token` and `registry-username`, which default to empty and to `github.actor` — a public image needs neither.
- The Action masks `registry-token` and sends it only in the template request — the sandbox never holds a registry token.
- The Action README replaces the "Once, per tenant" step with `registry-token: ${{ github.token }}`, and documents `POST /v1/registries` as the choice for a long-lived token — a stored job token is revoked when its job ends.
- `internal/apispec` and the SDKs carry the new field — the API contract states what the host accepts.

## Out
- A credential per sandbox. A sandbox never pulls.
- A credential on a service entry. The rule above covers the same registry, and a stored credential covers the others.
- Deleting existing stored credentials.

## How I know it works
- Two previews from two private repositories in one tenant start together, and both builds pull with their own `GITHUB_TOKEN`.
- After both runs, `GET /v1/registries` lists nothing new.
- A search of `kiln.db`, the template directory and the host journal for the token finds nothing.
- A template with a wrong `credential` fails with a pull error, even when a valid stored credential exists for that registry.
- A template with no `credential` still pulls with the stored credential.
