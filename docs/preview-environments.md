# Preview environments

A change gets a running copy of the application on a public hostname, so a
reviewer clicks a link instead of reading a diff and imagining the result.

Two things reach for this, and they use the same API.

- **CI**, on every pull request. See [the Action](../.github/actions/preview/README.md).
- **A coding agent**, which has finished a feature and wants to prove it runs.
  That is this page.

Kiln does not build application images. You build and push the image; Kiln
turns it into a microVM, runs it, and gives you the URL.

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
    "egress_allow": ["db.acme.internal", "api.stripe.com"],
    "ttl_seconds": 604800,
    "vcpus": 1, "memory_mb": 1024, "disk_mb": 4096
  }'

# 2. Wait for it. A build answers 202 and builds after the answer.
curl -s "$KILN_URL/v1/templates/feature-checkout-a1b2c3d" \
  -H "Authorization: Bearer $KILN_API_KEY" | jq -r .state   # ready | building | failed

# 3. One sandbox from it. The metadata is how you find this preview again;
#    the UI shows it beside the sandbox.
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

**Egress is deny by default.** A template with an empty `egress_allow`
reaches nothing, and DNS fails inside it. A preview that talks to a database
or a payment API needs those hostnames listed, and nothing beyond them is
reachable from the sandbox.

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
