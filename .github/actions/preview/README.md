# Kiln preview environment

A pull request gets a running copy of its own application on a public
hostname.

Pin this Action to a tag, as the example below does. Inside the Kiln
repository itself, `./.github/actions/preview` is the local path. You build and push the image; Kiln builds a template from it, runs
one sandbox, publishes the port, and comments the link. The pull request also
shows the preview under "Deployments", with a "View deployment" button.

## The registry login

`registry-token: ${{ github.token }}` pulls your image for this build. The host
holds the token in memory for the build and stores nothing. The sandbox never
holds it, because a preview runs the pull request's code. Two repositories in
one tenant can build at the same moment, because each build carries its own
login. A public image needs none of this.

`POST /v1/registries` is the choice for a long-lived token, such as a bot's
token shared by every build of a tenant. Do not store the job token this way:
GitHub revokes it when the job ends.

```sh
curl -X POST "$KILN_URL/v1/registries" \
  -H "Authorization: Bearer $KILN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"host":"ghcr.io","username":"YOUR_BOT","token":"YOUR_TOKEN"}'
```

The token is never returned again. `GET /v1/registries` lists the hostname
and the username only. `registry-token` wins over a stored credential on the
same registry.

## The workflow

```yaml
name: preview
on:
  pull_request_target:
    types: [opened, synchronize, reopened, labeled, closed]

permissions:
  contents: read
  pull-requests: write
  deployments: write
  packages: read

jobs:
  preview:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          # pull_request_target runs with your secrets, so a fork's code is
          # checked out by SHA and never trusted to change the workflow.
          ref: ${{ github.event.pull_request.head.sha }}

      # Build and push your image here, tagged with the commit SHA. To cache
      # the build, set these on docker/build-push-action:
      #   cache-from: type=gha
      #   cache-to: type=gha,mode=max

      # Pin a tag. @main tracks every push to Kiln, fixes and mistakes
      # alike, and a preview that breaks on somebody else's commit is worse
      # than one that lags a release.
      - uses: alternayte/kiln/.github/actions/preview@v0.9.5
        with:
          url: ${{ vars.KILN_URL }}
          api-key: ${{ secrets.KILN_API_KEY }}
          image: ghcr.io/${{ github.repository }}:${{ github.event.pull_request.head.sha }}
          registry-token: ${{ github.token }}
          port: "8000"
          start: /app/server --port 8000
          egress-allow: api.example.com
```

`KILN_API_KEY` names one tenant and reaches nothing else.

`start` is not optional. Kiln boots its own init and ignores what the image
says to run, so without it the sandbox holds your application's files and
serves nothing. Creating the sandbox waits until that command listens on
`port`, so the URL in the comment is live when it appears.

## Configuration

`env` takes one `KEY=value` per line. Fill values from your secrets:

```yaml
          env: |
            DATABASE_URL=${{ secrets.PREVIEW_DATABASE_URL }}
            FEATURE_X=on
```

Kiln handles every value like the host secrets. It reaches the sandbox memory and
nothing else: the host never writes it to disk, `GET /v1/sandboxes/{id}`
returns the key names only, and the Action masks each value in the log. A
line with no `=` fails the run before anything is built. A value cannot span
lines; encode one that does, for example in base64.

A database for each pull request is yours to create before this step and to
drop when the pull request closes. Put its host in `egress-allow`.

## More than one port

`publish` names other ports, space separated:

```yaml
          port: "3000"
          publish: "8080 9000"
```

`port` gets `<stem>.<zone>` and stays the `url` output. Each other port gets
`<stem>-<port>.<zone>`, so a frontend finds its API from its own `Host`
header. The comment lists every URL, and the `urls` output is a JSON object
from port to URL. Creating the sandbox waits for `port` only.

## Forks

A fork's commit is a stranger's code beside your API key, so a maintainer
decides when it runs. Add the label `preview` to a fork's pull request and
the next run builds it. A new commit takes the label away, and the preview
stops until a maintainer labels it again.

A pull request from a branch in your own repository needs no label.

A fork's preview never gets `env`. The label approves the code, not what the
code can reach, and a value in the sandbox can leave through its own
published port. The log says so when `env` was set.

## Deployments

Each commit creates a GitHub deployment for its SHA in the environment
`preview/pr-<n>`. It reports `in_progress`, then `success` with the preview URL
in `environment_url`, or `failure`. A newer commit sets the older deployments
to `inactive`, and so does closing the pull request. Tools that read the
`deployment_status` event, as they do for Vercel and Netlify, read this one.

A workflow with no `deployments: write` still gets its preview and its comment,
and its log holds a warning. The environment stays in the repository settings
after the pull request closes, because `GITHUB_TOKEN` cannot delete one.

## What it leaves behind

Nothing, on either path. Closing the pull request destroys the sandbox and
deletes the template. `ttl-seconds` is the floor under that: the host deletes
a template past its deadline with its sandboxes and its snapshots, whether or
not anything came back for it. The default is seven days.

A preview nobody opens sleeps after `idle-seconds` and wakes on the next
request to its hostname, so an open pull request costs nothing while nobody
is looking at it.
