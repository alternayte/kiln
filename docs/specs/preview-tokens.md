# Preview tokens

## What it does
A `team` preview opens without a browser. `POST /v1/sandboxes/{id}/publish` with `visibility: team` answers with a token beside the URL, and a request that carries it in the `X-Kiln-Preview-Token` header reaches the guest with no viewer session. The same call can mint a signed link that expires, for a person who has no viewer login. A CI job can then check a team preview, an agent can read the page it published, and a reviewer can get a link that dies at a time.

## Decisions
- The token is per published hostname, 128 bits from a CSPRNG, stored as a hash, and returned once at publish — a hostname is the unit of exposure, and one hostname's token must not open another.
- `POST /v1/sandboxes/{id}/publish/{port}/token` rotates it and returns the new one; the old one stops at once — a token that leaked into a log needs one call to retire.
- The header is `X-Kiln-Preview-Token`, checked at the ingress before the viewer session and before any guest traffic — a request that fails both never reaches the guest, as today.
- The header is stripped before the request is forwarded — the guest runs the code under review and must not learn a credential that opens its own hostname.
- `POST /v1/sandboxes/{id}/publish/{port}/link` with `expires_in_seconds` answers with `https://<host>/?kiln_token=<signed>`; the ingress verifies the signature and the expiry, sets a host-only cookie, and redirects to the path without the query — a link in a chat message must not keep working for ever, and the token must not stay in the browser history or the guest's logs.
- The signing key is the host's `seal_key` — one key to lose, already 0600, already a key the host holds.
- The maximum expiry is 24 hours, and the default is one hour — a link that lasts days is a viewer login without the audit.
- `public` hostnames take no token: the subdomain is the credential — a second credential on a public hostname is a lock on an open door.
- `GET /v1/sandboxes/{id}` reports whether each team hostname has a token and when its last link was minted, never the values — the caller knows what exists.
- The preview Action takes `visibility: team` as an input and prints the token as a masked output — a CI job that smoke-tests a team preview needs it in the next step.

## Out
- No per-viewer tokens and no per-token permissions. One hostname, one token.
- No token on `public` hostnames.
- No link that survives a rotate.
- No token for the control API. That is an API key.

## How I know it works
- A `team` hostname answers 401 with a challenge to a bare request, 200 to one with the token, and 401 to one with a rotated-away token.
- The guest's access log shows no request for the 401s and no `X-Kiln-Preview-Token` header on the 200.
- A signed link opens the preview in a fresh browser with no viewer login, and the same link answers 410 after its expiry.
- The redirect leaves the browser on the path with no `kiln_token` in the URL.
- The token of hostname A is refused on hostname B of the same sandbox.
- `grep` over `kiln.db` finds neither the token nor the signed link.
- A `curl` in a workflow step with the Action's masked output reaches the preview, and the log shows `***`.
