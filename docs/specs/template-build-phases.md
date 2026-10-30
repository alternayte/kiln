# Template build phases

Closes the measurement and polling parts of alternayte/kiln#15.

## What it does
The host records how long each phase of a template build and a sandbox start takes, as events. The preview Action prints these times in its log and polls every 2 seconds. A person reads one preview run and sees where the time went. The numbers select the cache, if a cache is necessary.

## Decisions
- The host adds one event per phase, with the phase name and `duration_ms` — events already exist per tenant, so this needs no new table and no new route.
- The template phases are `layers`, `rootfs`, `setup_boot`, `setup`, `snapshot_boot` and `snapshot` — the setup boot, the snapshot boot and the memory snapshot run in every build, and the issue did not count them.
- `layers` is one phase for pull and apply — `pull` streams each blob directly into `applyLayer`, and separate times need a buffer that a real build does not have.
- The sandbox phases are `restore` and `start` — `start` ends when the start command listens on the port, and a publish call reports its own time in the Action log.
- A failed phase records its event with the error — a failed build must show where it stopped.
- `up.sh` polls the template every 2 seconds and keeps its 30 minute limit — a 10 second poll adds up to 10 seconds to every preview.
- `up.sh` prints the phase events of its template and its sandbox after publish, and filters the tenant event stream by name and id — the tenant stream also holds other work.
- The Action README documents `cache-from: type=gha` and `cache-to: type=gha,mode=max` — the Docker build on GitHub also has no cache.

## Out
- A layer blob cache and an extracted tree cache. The phase numbers decide these in a separate spec.
- A change to the size of the rootfs.
- A metrics endpoint or a dashboard.

## How I know it works
- A preview run on c-dash prints a duration for each of the eight phases, and the sum is near the time from "building template" to "preview at".
- `GET /v1/events?after=0` holds one event per phase for that template and that sandbox.
- A template with a failing `setup` command shows a `setup` event with the error and no `snapshot_boot` event.
- A preview run has no `sleep 10` wait: the time between build end and sandbox create in the log is under 3 seconds.
