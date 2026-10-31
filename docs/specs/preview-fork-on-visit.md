# Preview fork on visit

## What it does
A published preview environment gives each viewer their own fork. Two viewers open the same hostname, change data, and never see each other's changes. Each starts from the same state, and the origin sandbox does not change. A viewer at the cap gets a page that says why.

## Decisions
- `POST /v1/sandboxes/{id}/publish` takes `viewer_fork: true` and a required `viewer_cap` — a hostname that starts microVMs must have a limit.
- `viewer_fork` needs `visibility: team`, and `public` returns `invalid` — an anonymous request must not start a microVM.
- The key is the viewer login, so one viewer has one fork across devices — the viewer login already identifies a person, and it needs no second cookie.
- A client with no viewer login gets the existing login redirect — this is the current `team` behaviour.
- The publish call takes one snapshot of the origin, and every fork restores from it — each viewer gets the same start state, and a visit does not pause the origin.
- The origin may sleep while forks serve — a restore does not read the origin.
- A secret-bearing origin returns `conflict` unless the publish call sets `allow_secret_fork: true` — a fork copies the memory that holds those values.
- A fork starts with no credential bindings, unless the publish call names `viewer_credentials` — `docs/specs/credential-injection.md` gives no copy a key by default.
- A fork keeps the egress policy in force in the snapshot — a copy must not widen.
- Every published port of the origin routes to the viewer's fork — one viewer has one copy of the application.
- The hostname stays the same — the application reads its `Host` header, and a new hostname breaks a login redirect or a cookie.
- A fork is `ephemeral`, with the `idle_seconds` of the origin — an unused fork must not hold memory.
- The next visit after a fork ends starts a new fork from the snapshot — a viewer gets a clean copy.
- `GET /v1/sandboxes/{id}/viewer-forks` lists each viewer and fork id, and a viewer page link resets a viewer's own fork — a reviewer needs a clean start with no API key.
- A request waits for the fork, as a request to a sleeping sandbox waits for a wake — this behaviour already exists.
- `viewer_cap` counts live forks. At the cap, or at the tenant `max_sandboxes` cap, the viewer gets a 503 page that names the limit — a silent failure looks like an application bug.
- A destroy of the origin or a retire of the port destroys every fork and the snapshot — a preview must leave nothing behind.
- Each fork has its own egress record and events, with the origin id in its metadata — a caller reads what one viewer's copy did.
- `internal/apispec`, the SDKs, the MCP tools and the preview Action carry this — the API contract states what the host accepts.
- The Action takes `viewer-fork` and `viewer-cap` inputs — a preview environment is where this applies.

## Out
- Forks for anonymous visitors.
- A fork per browser session or per tab.
- A merge of one viewer's data into another fork or into the origin.
- A hostname per viewer.
- A fork on visit for an API key. An agent calls the fork route.

## How I know it works
- A preview with `viewer_fork: true` and `viewer_cap: 2` publishes. A second call with `visibility: public` returns `invalid`.
- Viewer A writes a row. Viewer B opens the same hostname and does not see it. The origin does not hold it either.
- Viewer A opens the hostname from a second browser and sees the row.
- A third viewer at `viewer_cap: 2` gets the 503 page that names the cap.
- A secret-bearing origin returns `conflict` with no `allow_secret_fork`.
- After `idle_seconds`, the fork is gone. Viewer A's next visit shows the seeded state.
- The reset link gives viewer A the seeded state.
- The origin's `exec` of a page count shows no change after both visits, and the origin was not paused by a visit.
- `DELETE` on the origin leaves no fork and no snapshot in `kiln sandboxes list` and `GET /v1/snapshots`.
