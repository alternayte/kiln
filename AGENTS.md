# Kiln

## What this is
Kiln runs isolated Linux microVMs on one host, built from OCI images and forked into copies. The build authority is `docs/specs/`; no application code exists yet.

## Run
Nothing starts; no code exists.

## Test
`just check` runs every check in `checks/` and is the gate.
`just check-slow` runs the read-only commands this file quotes.

## Stack rules
- Isolation is a microVM with its own kernel, launched under jailer. No host containers.
- Storage is one SQLite file. No Postgres.
- One host, one binary, systemd. No Kubernetes, no clustering.
- Three interfaces are permitted: runtime.Runtime, snapshot.PageFaultSource, store.Store. No others.
- `docs/internal/sdd.md` is frozen. Never edit it to match code.

## Domain words
- sandbox: a Firecracker microVM created from a template snapshot. Avoid: container, instance.
- template: an OCI image built once into a rootfs plus memory and state, from which sandboxes restore. Avoid: base image.
- snapshot: a memory and state copy that restores into one or more sandboxes. Avoid: backup, checkpoint.
- fork: one snapshot restored into several new sandboxes. Avoid: clone.
- sleep: snapshot a sandbox and stop it while keeping its id and published hostnames. Avoid: suspend, hibernate.
- publish: serve a guest port on a public hostname. Avoid: expose, port-forward.
