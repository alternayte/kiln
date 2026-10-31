# Template services

## What it does
A template names service images, such as `postgres:16` and `redis:7`, beside its application image. Every image runs in the one microVM of a sandbox and shares its network, so the application reaches a service on `localhost`. The services start and the seed commands run before the template snapshot, so every sandbox restores with the services running and the seeded data loaded. A template with a `port` and no `start` runs the command its image names.

## Decisions
- All images run in one microVM — one snapshot then holds the whole stack, and a restore, a fork or a sleep captures the application and its data at one point.
- `POST /v1/templates` takes `services`, a list of `name`, `image`, `env`, an optional `port` and an optional `start` — a team ships an application image and uses stock images for everything else.
- Kiln does not read `docker-compose.yml` — compose carries builds, volumes, networks and conditions, and a file that works in part fails in ways the team cannot predict.
- The host pulls each service image with the tenant's registry credentials, exactly as it pulls the application image — the sandbox never holds a registry token.
- Each image unpacks into its own root under the rootfs, and kilninit runs each service chrooted into its root — images carry conflicting libraries, and one merged tree breaks one of them.
- Kilninit applies ENV, WORKDIR and USER from each image config, and runs the command the image config names when `start` is absent — a stock image does not start without its own environment and user.
- A service always runs its command. A service `port` is its readiness, and a service with no port is ready once it starts — a background job listens on nothing.
- A service takes an optional `ready`, a command that runs in its root until it exits 0, in addition to `port` — an open port does not always mean a service answers, and `pg_isready` is the accurate check for Postgres.
- Services start in list order, and each waits for its port and its `ready` before the next starts, within the 120 second start deadline each — a later service may depend on an earlier one.
- Each service `name` becomes an `/etc/hosts` entry for `127.0.0.1` in every root — recipes reach a service by name, such as `postgres:5432` (alternayte/kiln#2).
- Two services on one port fail the build, and the error names both — they share one network.
- Services start in the snapshot boot, before the template snapshot — a restore then takes milliseconds, not the boot of a database.
- `seed` is a list of commands that run in the application root, in the snapshot boot, after every service is ready and before the snapshot — migrations and seed scripts need a live service.
- `setup` keeps its meaning and its boot — current templates keep working unchanged.
- The template keeps the snapshot boot overlay, and each sandbox restores onto its own copy of it — the snapshot memory holds files open on that overlay, and a blank overlay would not match it.
- A service takes `env` from the template only, never from sandbox `env` — it is running before any sandbox exists.
- The application start command still runs at restore, after the resume hooks — it needs the sandbox `env`, the secrets and the hostname.
- The application runs its image command only when the template names `port` — with no `port`, a terminal-only template such as `ubuntu:24.04` would run `bash` with no TTY and fail.
- `port` without `start` is now valid. `start` without `port` is still refused — a start with no port has no readiness.
- No service is published by default. `publish` names every port that gets a hostname — a database must not answer on the internet.
- `GET /v1/sandboxes/{id}/logs` and `kiln logs` also return each service with its output and how it ended — the route exists, and a preview fails most often in a service.
- Nothing restarts a service. The host records each service exit as an event with its name, exit code and last output — a crash the pull request caused is what the preview exists to show.
- The preview Action takes `services` and `seed` as YAML inputs and passes them through — the API is the product.
- The Action takes `fork-count` and outputs one sandbox id per fork — a CI job runs one test shard per fork from one seeded stack.
- The docs give the shard recipe as one fork per shard, and the caller's test runner splits the tests — a per-test copy of a Postgres template database is faster than a fork per test.

## Out
- No compose import.
- No service in a separate sandbox and no private network between sandboxes.
- No per-sandbox service configuration.
- No volumes and no data that outlives the sandbox.
- No supervisor, no restart and no health check after the first listen.
- No image build by Kiln.

## How I know it works
- A template with an application image, `services: [postgres:16 with port 5432]` and `seed: ["npm run migrate", "npm run seed"]` builds. A sandbox from it serves a page that reads seeded rows on the first request.
- `GET /v1/sandboxes/{id}` returns in the time of a restore. The guest shows one Postgres process that was never started inside the sandbox.
- The sandbox forks into three. A row written in one copy does not appear in the other two.
- The sandbox sleeps and wakes. The rows written before the sleep are still there.
- A request to the published hostname reaches the application. No hostname answers on port 5432.
- A template with `port: 80` and no `start` on `nginx:alpine` serves nginx.
- A template on `ubuntu:24.04` with no `port` builds, and its terminal opens.
- Postgres is killed inside a sandbox. `GET /v1/events` carries the service name, the exit code and the last output. Nothing restarts it.
- A service image that is private in another tenant fails the build with a pull error.
- A service that never listens fails the build, and the error names the service and the port.
- The application connects to `postgres:5432`, and `getent hosts postgres` prints `127.0.0.1`.
- A service with `ready: ["pg_isready"]` starts the next service only after that command exits 0. A `ready` that never exits 0 fails the build, and the error names the service.
- Two services with `port: 5432` fail the build, and the error names both.
- `fork-count: 3` outputs three sandbox ids, and a row written in one is absent from the other two.
- A pull request with `services` and `seed` in the Action gets a preview that reads seeded data.
