# Start health check

## What it does
A template with a `start` command can name a `health` request: a path on `port`, and the status it must answer. Creating a sandbox answers only once the application answers that request, not once the port accepts a connection. A preview link, when it appears, points at an application that serves pages, and a crash after the listen is reported as a failed create with the response the application gave.

## Decisions
- `POST /v1/templates` takes `health`, an object of `path` and optional `status`, beside `start` and `port` — a TCP accept proves a socket, and an application that accepts then answers 500 reads as a broken preview.
- `health` without `start` is refused — nothing would answer.
- The default `status` is 200; `path` must begin with `/` — one answer means ready.
- The host sends the request from the guest agent, inside the microVM, over `localhost` — the application listens where it listens, and egress rules stay untouched.
- The check runs every second until it passes or the start deadline of 120 seconds expires, and the deadline is shared with the listen wait — one budget for one start.
- A sandbox that never passes is `failed`, and its error names the path, the last status and the first 1 KiB of the last body — the author reads the application's own words.
- The check runs only at a restore from the template snapshot, like the start command — a wake and a fork restore memory that already holds the serving process.
- Nothing re-checks after the first pass — a crash the review caused is what the preview exists to reveal, and the existing exit watch reports it.
- The preview Action takes `health-path` and `health-status`, and the UI's template form takes the same — the API is the product.

## Out
- No supervisor, no restart, no periodic check.
- No check on `exec`, on a fork or on a wake.
- No request body, no headers, no TLS inside the guest.
- No readiness for services; a service port stays a TCP accept.

## How I know it works
- A template with `health: {"path": "/healthz"}` and an application that answers 200 after a two-second warm-up produces a sandbox that returns from create only after that, and the published hostname answers on its first request.
- An application that listens and answers 503 for ever produces a `failed` sandbox inside the deadline, and the error carries `/healthz`, `503` and the body.
- `health` on a template with no `start` is refused with `invalid`.
- A sandbox sleeps and wakes, and the guest shows no second health request.
- A preview from the Action with `health-path: /healthz` comments its link only once the application answered.
