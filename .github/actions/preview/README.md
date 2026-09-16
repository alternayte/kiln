# Kiln preview environment

A pull request gets a running copy of its own application on a public
hostname.

Pin this Action to a tag, as the example below does. Inside the Kiln
repository itself, `./.github/actions/preview` is the local path. You build and push the image; Kiln builds a template from it, runs
one sandbox, publishes the port, and comments the link.

## Once, per tenant

Store the credential the host pulls your image with. The host pulls; the
sandbox never does, because a preview runs the pull request's code and that
code must not hold the token.

```sh
curl -X POST "$KILN_URL/v1/registries" \
  -H "Authorization: Bearer $KILN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"host":"ghcr.io","username":"YOUR_BOT","token":"YOUR_TOKEN"}'
```

The token is never returned again. `GET /v1/registries` lists the hostname
and the username only.

A public image needs none of this.

## The workflow

```yaml
name: preview
on:
  pull_request_target:
    types: [opened, synchronize, reopened, labeled, closed]

permissions:
  contents: read
  pull-requests: write

jobs:
  preview:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          # pull_request_target runs with your secrets, so a fork's code is
          # checked out by SHA and never trusted to change the workflow.
          ref: ${{ github.event.pull_request.head.sha }}

      # Build and push your image here, tagged with the commit SHA.

      # Pin a tag. @main tracks every push to Kiln, fixes and mistakes
      # alike, and a preview that breaks on somebody else's commit is worse
      # than one that lags a release.
      - uses: alternayte/kiln/.github/actions/preview@v0.6.0
        with:
          url: ${{ vars.KILN_URL }}
          api-key: ${{ secrets.KILN_API_KEY }}
          image: ghcr.io/${{ github.repository }}:${{ github.event.pull_request.head.sha }}
          port: "8000"
          start: /app/server --port 8000
          egress-allow: api.example.com
```

`KILN_API_KEY` names one tenant and reaches nothing else.

`start` is not optional. Kiln boots its own init and ignores what the image
says to run, so without it the sandbox holds your application's files and
serves nothing. Creating the sandbox waits until that command listens on
`port`, so the URL in the comment is live when it appears.

## Forks

A fork's commit is a stranger's code beside your API key, so a maintainer
decides when it runs. Add the label `preview` to a fork's pull request and
the next run builds it. A new commit takes the label away, and the preview
stops until a maintainer labels it again.

A pull request from a branch in your own repository needs no label.

## What it leaves behind

Nothing, on either path. Closing the pull request destroys the sandbox and
deletes the template. `ttl-seconds` is the floor under that: the host deletes
a template past its deadline with its sandboxes and its snapshots, whether or
not anything came back for it. The default is seven days.

A preview nobody opens sleeps after `idle-seconds` and wakes on the next
request to its hostname, so an open pull request costs nothing while nobody
is looking at it.
