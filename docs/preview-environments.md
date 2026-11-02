# Preview environments

A change gets a running copy of the application on a public hostname, so a
reviewer clicks a link instead of reading a diff and imagining the result.

Two things reach for this, and they use the same API.

- **CI**, on every pull request. See [the Action](../.github/actions/preview/README.md), which a workflow pins by tag: `alternayte/kiln/.github/actions/preview@v0.10.0`. The workflow needs `deployments: write`, because the preview reports a GitHub deployment with its URL.
- **A coding agent**, which has finished a feature and wants to prove it runs.
  That is this page.

Kiln does not build application images. You build and push the image; Kiln
turns it into a microVM, runs it, and gives you the URL.

## The one command

`kiln preview up` runs the four calls below, waits for the build, and prints the
URL. A second `up` in the same directory replaces the first. `kiln logs <id>`
reads a failure.

```sh
kiln preview up --image ghcr.io/acme/shop:a1b2c3d --port 8000 --start '/app/server --port 8000'
kiln preview down
```

## A stack with services

A template names service images beside its application image. They all run in
one microVM, and each service name resolves to `127.0.0.1`. The services start
and the seed commands run before the template snapshot, so every sandbox
restores with the database running and its data loaded.

```yaml
# services.yaml
- name: postgres
  image: postgres:16
  port: 5432
  ready: ["pg_isready"]
  env:
    POSTGRES_HOST_AUTH_METHOD: trust
```

```sh
kiln preview up --image ghcr.io/acme/shop:a1b2c3d --port 8000 \
  --services-file services.yaml --seed 'npm run migrate' --seed 'npm run seed'
```

No service gets a hostname unless you publish its port. Nothing restarts a
service: `GET /v1/events` and `kiln logs <id>` report how it ended. A template
with `port` and no `start` runs the command its image names.

Two services cannot share a port, because they share one network. A service's
`env` comes from the template only, because it runs before any sandbox exists.
Do not put a production credential in it: the value is in the template snapshot.

## One test shard per fork

A fork holds the running stack and its own copy of the data, so one seeded stack
serves every shard of a test suite. Run one fork per shard, and let your test
runner split the tests. For isolation per test, a per-test copy of a Postgres
template database is faster than a fork.

```sh
kiln preview up --image ghcr.io/acme/shop:a1b2c3d --port 8000 \
  --services-file services.yaml --seed 'npm run seed' --fork-count 3 --json | jq -r '.forks[]'
```

The Action takes the same as `services`, `seed` and `fork-count`, and outputs
`fork-ids`.

## Once per tenant: the registry credential

The host pulls your image. The sandbox never does, because the sandbox runs
the code under review and that code must not hold a credential that reaches
your registry.

```sh
curl -X POST "$KILN_URL/v1/registries" \
  -H "Authorization: Bearer $KILN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"host":"ghcr.io","username":"BOT","token":"TOKEN"}'
```

The token is sealed on the host and never returned. `GET /v1/registries`
lists the hostname and the username only. A public image needs none of this.

## The four calls

```sh
# 1. A template from the image this change produced. The TTL is the floor:
#    the host deletes the template, its sandboxes and its snapshots at the
#    deadline, whether or not anything comes back for them.
curl -X POST "$KILN_URL/v1/templates" \
  -H "Authorization: Bearer $KILN_API_KEY" -H 'Content-Type: application/json' \
  -d '{
    "name": "feature-checkout-a1b2c3d",
    "image": "ghcr.io/acme/shop:a1b2c3d",
    "start": ["/app/server", "--port", "8000"],
    "port": 8000,
    "egress_allow": ["db.acme.com", "api.stripe.com"],
    "ttl_seconds": 604800,
    "vcpus": 1, "memory_mb": 1024, "disk_mb": 4096
  }'

# 2. Wait for it. A build answers 202 and builds after the answer.
curl -s "$KILN_URL/v1/templates/feature-checkout-a1b2c3d" \
  -H "Authorization: Bearer $KILN_API_KEY" | jq -r .state   # ready | building | failed

# 3. One sandbox from it. This answers only once the start command listens
#    on the port, so the URL you publish next is a live one. The metadata is
#    how you find this preview again; the UI shows it beside the sandbox.
curl -X POST "$KILN_URL/v1/sandboxes" \
  -H "Authorization: Bearer $KILN_API_KEY" -H 'Content-Type: application/json' \
  -d '{
    "template": "feature-checkout-a1b2c3d",
    "lifecycle": "persistent",
    "idle_seconds": 900,
    "metadata": {"repository": "acme/shop", "pull_request": "412", "commit": "a1b2c3d"}
  }'

# 4. The URL.
curl -X POST "$KILN_URL/v1/sandboxes/$ID/publish" \
  -H "Authorization: Bearer $KILN_API_KEY" -H 'Content-Type: application/json' \
  -d '{"port": 8000, "visibility": "public"}'
```

The last call answers with the hostname. That is what you give the person.

## What an agent should tell the person

The link, and what it costs them to click it. The first request after the
idle deadline wakes the sandbox, so it is slower than the rest. Nothing else
needs explaining.

An agent driving Kiln over MCP has the same operations as tools, at
`$KILN_URL/mcp`. `$KILN_URL/llms.txt` carries this recipe in the form an
agent reads first.

## What it costs while nobody looks

Nothing that matters. The sandbox sleeps after `idle_seconds` and wakes on
the next request to its hostname, keeping its id and its URL. The template
dies at `ttl_seconds` and takes its sandboxes and snapshots with it, so a
preview nobody closed does not become a bill.

Set `ttl_seconds` on every preview. It is the only thing that ends a preview
whose author forgot it.

## Cleaning up on purpose

```sh
curl -X DELETE "$KILN_URL/v1/sandboxes/$ID"     -H "Authorization: Bearer $KILN_API_KEY"
curl -X DELETE "$KILN_URL/v1/templates/$NAME"   -H "Authorization: Bearer $KILN_API_KEY"
```

The sandbox goes first: a template with a live sandbox refuses to be deleted.

## Two things to get right

**Egress names hosts, not addresses.** Each sandbox has an egress guard, a host
process that reads the TLS name of every port 443 connection, resolves that name
itself, and refuses a name outside `egress_allow`. `kiln egress <id>` prints what
a sandbox reached and what the host refused. A guard reads only the unencrypted
TLS name, so it does not see the `Host` header inside the connection. An allowed
host that fronts other sites can still reach them. The guard refuses a name that
resolves to a private, loopback, link-local or metadata address, and it serves
port 443 only, so a database inside your own network needs a service in the
template or a public TLS endpoint.

**Egress is deny by default.** A template with an empty `egress_allow`
reaches nothing, and DNS fails inside it. A preview that talks to a database
or a payment API needs those hostnames listed, and nothing beyond them is
reachable from the sandbox.

**Name the start command.** Kiln boots its own init and ignores what the
image says to run, so a template with no `start` gives you a sandbox holding
your application's files with nothing listening, and a published hostname
that answers nothing.

**One template per commit.** The template is the unit a snapshot belongs to,
so a preview per commit gets its own rootfs and a warm restore. Reusing one
template across commits gives every preview the same code.

## When a preview will not start

- `exhausted` means a cap of the tenant is reached. The message names which.
- A build that fails records why on the template: `GET /v1/templates/{name}`
  returns `state: failed` and an `error`. `GET /v1/events` carries the same
  story as it happens.
- A private image with no stored credential fails to pull. Store the
  credential for that registry and build again.

## One fork for each viewer

A team preview can give each viewer login its own fork, so viewers do not see each
other's changes.

```sh
kiln preview up --image X --port 8000 --viewer-fork --visibility team --viewer-cap 10
```

The Action takes `viewer-fork` and `viewer-cap`. A preview that holds env values also
needs `--allow-secret-fork` on `kiln preview up`, because each fork copies that memory.

- The publish call takes one snapshot. Each fork restores from it, and a visit does not
  pause the origin. The origin may sleep.
- A viewer keeps one fork across devices. An idle fork ends, and the next visit starts a
  clean one.
- A viewer past `viewer_cap`, or past the tenant's `max_sandboxes`, gets a 503 page that
  names the limit.
- `/_kiln/fork` on the hostname is the viewer's reset page, and the publish answer names
  it as `viewer_reset_url`. `GET /v1/sandboxes/{id}/viewer-forks` lists each viewer and fork.
- A fork starts with no credential bindings, unless the publish call names
  `viewer_credentials`.
- Destroying the origin, or retiring any of its ports, retires every port and destroys
  every fork and the snapshot. A plain publish of a viewer fork origin returns `conflict`.
