# Preview deployments

Closes alternayte/kiln#17.

## What it does
The preview Action creates a GitHub deployment for the head commit of each pull request. The deployment reports `in_progress`, then `success` with the preview URL or `failure`, and `inactive` when the preview goes. A pull request shows "View deployment", and Factoree reads the URL from `deployment_status`. The comment stays.

## Decisions
- The Action creates the deployment after the fork check and before the template build, with `ref` = head SHA, `environment` = `preview/pr-<n>`, `transient_environment: true`, `auto_merge: false` and `required_contexts: []` — Factoree accepts a status only when the deployment SHA equals the published commit.
- `success` carries `environment_url` = the preview URL and `log_url` = the run URL, and the Action sends it after publish — the URL is live when the status appears.
- A step with `if: failure()` sets `failure` when a deployment exists — `up.sh` exits at many points.
- After `success`, the Action sets `inactive` on every earlier deployment of the environment — GitHub skips transient environments in its automatic `inactive`, and `prune.sh` has already removed those previews.
- `down.sh` sets `inactive` on every deployment of the environment — closing the pull request removes the preview.
- A failed deployment call logs a warning, and the preview continues — a workflow with no `deployments: write` must not break on upgrade, and the preview matters more than its status.
- The environment stays in the repository settings after the pull request closes — `GITHUB_TOKEN` cannot delete an environment, and the README states this.
- The README and `docs/preview-environments.md` add `deployments: write` to the workflow permissions — the Action needs it to report a deployment.
- The `fork.sh` refusal creates no deployment — no preview exists to report.

## Out
- A production environment.
- Deleting environments or deployments.
- A Factoree change.

## How I know it works
- A pull request on alternayte/c-dash shows the preview under "Deployments" with "View deployment".
- `gh api repos/alternayte/c-dash/deployments --jq '.[0].sha'` equals the head SHA. Its latest status is `success`, with the preview URL in `environment_url`.
- A second commit sets the first deployment to `inactive`.
- Closing the pull request sets every deployment of that environment to `inactive`.
- A preview with a bad image sets `failure`.
- A workflow with no `deployments: write` still gets its preview and its comment, and its log holds the warning.
- A labelled pull request from a fork reports a deployment for its head SHA. Otherwise its log holds the warning, and the preview still runs.
