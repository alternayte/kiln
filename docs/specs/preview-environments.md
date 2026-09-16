# Preview environments

## What it does
A pull request gets a running copy of its own application on a public hostname. A GitHub Action builds the application image, pushes it to the team's registry, and asks Kiln for a template and a sandbox from that commit. The Action comments the hostname on the pull request. A later commit replaces the preview, and closing the pull request destroys it. A preview that nobody opens sleeps, and the next request wakes it.

## Decisions
- One template per commit, and one sandbox from that template — a template is the unit a snapshot belongs to, so every preview gets a warm restore and its own rootfs.
- The host pulls the application image; the sandbox never pulls it — a preview runs the pull request's code, and that code must never hold the registry token.
- `POST /v1/registries` stores a registry hostname, a username and a token for one tenant, encrypted at rest with the host key. `GET /v1/registries` lists them without the token, and `DELETE /v1/registries/{host}` removes one — the host's own Docker credentials belong to the operator, and a tenant must not borrow them.
- A template build uses only the credentials of its own tenant, and falls back to an anonymous pull — a public image needs no credential, and one tenant's token must never pull for another.
- `POST /v1/templates` gains `ttl_seconds`. The reconcile loop deletes a template past its TTL with its snapshot and its sandboxes — a preview is created by a machine and forgotten by a human, so the host ends one with no client involved.
- The Action also deletes the template on `pull_request: closed` — the TTL is the floor, not the normal path.
- The preview sandbox carries `idle_seconds` and no `ttl_seconds`. It sleeps when nobody opens it, and the ingress wakes it on the next request — the template TTL ends the preview, and sleeping costs nothing in the meantime.
- The template is named `pr-{number}-{sha7}`, and the sandbox metadata carries `repository`, `pull_request` and `commit` — the Action finds the previous preview of one pull request without keeping state of its own.
- A new commit creates the new template and sandbox first, comments the new hostname, then deletes the old ones — the preview link never points at nothing.
- The Action lives in this repository at `.github/actions/preview`, and it is a composite action that calls the gateway with `curl` — the API is the product, and an action that needs a build step is an action that rots.
- The Action authenticates with a tenant API key in `KILN_API_KEY`, and reads the gateway address from `KILN_URL` — a key names one tenant, so a repository reaches nothing else.
- The Action takes the application port, the egress allow list and the setup commands as inputs, and passes them to `POST /v1/templates` — a preview needs a database host or an API host, and egress is deny by default.
- The Action fails the job when the template build fails, and it prints the build events from `GET /v1/events` — a silent preview failure wastes the reviewer's time, not the author's.
- A pull request from a fork gets a preview only after a maintainer adds the `preview` label, and the workflow runs on `pull_request_target` with the labelled commit checked out by SHA — a fork's commit is a stranger's code beside the tenant's API key, and a human decides when it runs.
- A new commit on a labelled fork pull request removes the label, and the preview stops until a maintainer labels it again — a label that survives a push authorises code nobody read.
- The comment is one line with the hostname, and the Action edits its own earlier comment instead of adding one — a pull request with thirty commits must not carry thirty comments.
- The gateway web UI shows the preview sandboxes of a tenant in its normal sandbox list, with the repository and pull request from the metadata — a preview is a sandbox, and a second screen would be a second model.

## Out
- No support for a repository with several applications in one preview. One image, one sandbox.
- No build of the application image by Kiln. The Action builds and pushes it, and the registry is the team's.
- No GitLab, no Bitbucket and no generic webhook. The API is public, so another forge needs no change here.
- No seeded database and no service dependencies. `egress_allow` reaches a real one.
- No custom domain per preview. The hostname comes from `publish`.

## How I know it works
- A tenant stores a `ghcr.io` credential through `POST /v1/registries`, and `GET /v1/registries` shows the hostname and the username and no token.
- A template that names a private image on that registry builds. The same template in another tenant fails with a pull error, and the host logs the refusal.
- A template that names `docker.io/library/nginx:alpine` builds with no credential stored.
- A pull request opens. The workflow runs, and a comment carries a hostname. The hostname serves the application.
- A second commit lands. The comment is edited, not repeated, and the new hostname serves the new build. The old sandbox and template are gone.
- The preview is left alone past its idle window and sleeps. The next request to the hostname answers, and the first request is slow.
- The pull request closes. The sandbox, the template and the hostname are gone within one reconcile pass.
- A template created with `ttl_seconds: 604800` and never deleted is gone after its TTL, and so are its sandboxes and its snapshot.
- A pull request from a fork runs no preview until a maintainer adds the `preview` label. The label starts one. A new commit on that pull request removes the label and stops the next run.
- A failed build fails the job, and the log holds the build events.
- The gateway UI lists the preview sandbox with its repository and pull request number.
