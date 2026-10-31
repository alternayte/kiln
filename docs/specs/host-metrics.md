# Host metrics

## What it does
The host and the gateway each serve `GET /metrics` in Prometheus text format, and the operator token opens it. An operator sees capacity, sandbox states, restore and fork durations, and egress guard failures.

## Decisions
- The format is Prometheus text — an operator on one box already has a scraper for it, or can read it with `curl`.
- The route needs the operator token — the counts describe every tenant.
- The route is outside `/v1`, and the gateway never forwards the host route — a tenant API key must not reach it.
- No metric has a sandbox, tenant or template label — the series count must stay bounded on a host with many sandboxes.
- The host reports sandboxes by state, restore duration, fork duration, snapshot duration, sleep and wake counts, template build failures, and disk and memory left under `/var/lib/kiln` — these answer whether the box has room and whether it is healthy.
- The host reports egress guard restarts and dropped egress records — a gap in the record must be visible to the operator.
- Duration metrics are histograms with one fixed bucket list — an operator compares values across versions.
- The gateway reports requests by route template and status, and its host connection failures — a raw path holds sandbox ids.
- The fork duration histogram is the measurement for a later decision on a fork with no pause — a fork of a large sandbox snapshots its origin first, and no number exists today.

## Out
- A per-tenant dashboard or a tenant-readable metric.
- Bundled Grafana dashboards and alert rules.
- OpenTelemetry export.
- Billing or metering.
- A fork with no pause of the origin.

## How I know it works
- `curl -H "Authorization: Bearer $TOKEN" localhost:8080/metrics` returns Prometheus text with `sandboxes by state`, and the running count equals `kiln sandboxes list`.
- The same call with no token, and with a tenant API key, returns 401.
- `promtool check metrics` passes on the host and the gateway output.
- A fork adds one sample to the fork duration histogram.
- `kill -9` on an egress guard adds 1 to its restart count.
- After 100 sandboxes are created and destroyed, the series count does not grow.
- A request to `/v1/sandboxes/<id>` appears under the route template, and the output holds no sandbox id.
