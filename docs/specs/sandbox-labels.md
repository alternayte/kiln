# Sandbox labels

## What it does
A template and a sandbox carry labels: a flat map of string keys to string values. `GET /v1/sandboxes` and `GET /v1/templates` take `?label=key=value`, repeated, and return the rows that carry every pair. CI finds the preview of one pull request with one call, and an agent finds the sandbox it made for one task without scanning the list.

## Decisions
- Labels are a map of at most 32 pairs, keys matching `^[A-Za-z0-9_.-]{1,64}$` and values of at most 256 bytes — a label is an index, and `metadata` stays the free-form document a caller reads back.
- `metadata` stays as it is — the preview Action and the UI read it today, and a second meaning for one field would break both.
- `POST /v1/sandboxes` and `POST /v1/templates` take `labels`; a fork and a restore copy the parent sandbox's labels — a copy made for the same task is found the same way.
- `GET /v1/sandboxes/{id}` and `GET /v1/templates/{name}` return `labels` — the caller sees what it set.
- `?label=key=value` may repeat, and every pair must match — one filter shape, no query language.
- A label filter is an SQL predicate over a `labels` table of `(tenant_id, kind, id, key, value)` rows with an index on `(tenant_id, key, value)` — the list stays one query at a thousand sandboxes.
- The preview Action sets `kiln.io/repository`, `kiln.io/pull-request` and `kiln.io/commit`, and `prune.sh` lists by them — the Action keeps no state of its own, and stops reading every sandbox of the tenant.
- The UI shows labels on the sandbox and template screens and filters the lists by them — the lists are where a person looks.
- The MCP tools and the SDKs take `labels` on create and on list, from the one API description — nothing restates the field.

## Out
- No label on a snapshot. A snapshot belongs to a sandbox or a template, and is found through them.
- No label update on a running sandbox. Destroy and create, or fork.
- No label on a published hostname.
- No wildcard or prefix match.

## How I know it works
- A sandbox created with `labels: {"task": "42"}` appears in `GET /v1/sandboxes?label=task=42` and not in `?label=task=43`.
- Two labels in one query return only the rows that carry both.
- A fork of that sandbox carries `task=42`.
- A label with a key of 65 characters is refused with `invalid` naming the key.
- `GET /v1/sandboxes?label=task=42` from another tenant's key returns an empty list.
- The preview Action, on a second commit, destroys the earlier preview found by label and no other sandbox.
- A thousand sandboxes with distinct labels: the filtered list answers in the time of an indexed query, not of a scan, as the query plan shows.
