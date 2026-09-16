# Template start command

## What it does
A template names the command that runs its application, and the port that
command listens on. A sandbox created from that template comes up with the
application already running, so a published hostname serves it on the first
request. A sandbox that wakes or forks resumes the application it already
held. When the command exits, the sandbox records why and keeps running.

## Decisions
- `POST /v1/templates` takes `start` and `port` — Kiln boots kilninit as PID 1 and ignores what the image says to run, so without these a sandbox holds the application's files and runs nothing.
- The start command runs only when a sandbox restores from a template snapshot — a template snapshot comes from a fresh VM, while a wake and a fork restore memory that already holds the running process, and a second copy would fight for the port.
- `POST /v1/sandboxes` answers only once the guest accepts a connection on the template's port — the caller publishes and hands out the URL the moment create returns, and a URL that answers 502 reads as a broken preview rather than a slow one.
- A sandbox whose application never listens within the start deadline is `failed`, and its error names the command and the port — a create that hangs tells the caller nothing, and a preview that silently never starts wastes the reviewer's time.
- The start deadline is 120 seconds — a runtime that boots and binds a port fits, and a build that belongs in `setup` does not.
- kilninit gains a `start` op that runs the command detached, in its own session, and answers as soon as it has started it — `exec` kills its process group when the call ends, so the application would die with the request that started it.
- The host watches the started command and records its exit as an event carrying the exit code and the last output it wrote — a reviewer whose link stops answering needs to know the application died and what it printed.
- Nothing restarts the application — a crash the pull request caused is what the preview exists to reveal, and a restart hides it.
- The start command takes the sandbox's injected secrets and the guest's base environment, exactly as `exec` does — an application reads its configuration from the environment, and a second rule for one command would surprise.
- The start command runs after the resume hooks — it needs the hostname and the secrets the hooks place.
- `start` and `port` are optional and travel together — a template that names one without the other is refused, because a start with no port has no readiness and a port with no start never listens.
- `publish` keeps its own port argument and does not default to the template's — one is what the guest listens on, the other is what the caller chooses to serve, and a template may listen on several.
- The sandbox screen shows the start command's exit in its event list — the events stream already carries it, and a second screen would be a second model.

## Out
- No supervisor. No restart, no backoff, no health checking after the first listen.
- No log store. The terminal reads the application's output inside the sandbox.
- No readiness beyond a port that accepts a connection. Kiln does not know what a healthy response looks like.
- No start command on a sandbox. One image runs one way.
- No change to `exec`. It still kills its process group when the call ends.

## How I know it works
- A template built with `start` and `port` produces a sandbox that answers on its published hostname on the first request, with no exec in between.
- `POST /v1/sandboxes` on such a template takes longer than one without, and returns with the sandbox already running.
- A template whose start command exits at once produces a `failed` sandbox, and the error names the command and the port.
- A sandbox sleeps and wakes, and the application answers again without the start command running twice. The guest shows one process.
- A running sandbox forks into three, and each copy answers on its own hostname with one process each.
- The application is killed inside the sandbox. The sandbox stays running, the hostname stops answering, and `GET /v1/events` carries the exit code and the last output.
- A template that names `start` and no `port` is refused with `invalid`.
- A preview built by `.github/actions/preview` serves the application with no change to the Action.
