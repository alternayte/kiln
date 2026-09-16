# Release artifacts

## What it does
A tag publishes the two things Kiln runs as: one static binary for a KVM host,
and a gateway image. An operator installs a host by downloading the binary and
running `kiln init`, with no Go toolchain and no source. The binary carries
the guest agent inside it, so the agent a host installs always matches the host
that installed it.

## Decisions
- The `kiln` binary embeds `kilninit` — one artifact to download and verify, and a guest agent can never be a different version from the host that wrote it, which is the skew that makes a sandbox refuse an operation its host offers.
- `kiln init` writes the embedded agent and never runs `go build` — the current fallback keeps whatever binary already sits in the bin directory when the source is missing, so a host runs a guest agent nobody chose.
- `kiln init` behaves the same in a repository and on a bare host — one install path is one thing to explain and one thing that can break.
- A check rebuilds `kilninit` and fails when the embedded copy differs — an edit to guest code that nobody embedded must not reach main, exactly as a stale web bundle must not.
- A release publishes the `kiln` binary for linux/amd64 and a `SHA256SUMS` file — `scripts/versions.env` pins x86_64, so a second architecture would claim support nobody has tested.
- A release publishes the gateway image on ghcr, tagged with the version — the gateway is deployed from an image, and a deployment that pulls a tested one beats a deployment that rebuilds from source every time.
- Pushing a tag runs `just check` and every gate, and publishes only when they pass — a release binary is the one thing people run without reading the code, so a tag that skipped the gates could ship a `kiln` that never booted a microVM.
- The binary reports `apispec.Version` — that constant already changes with every release, so nothing is stamped at build time and nothing can disagree with the document the gateway serves.
- The release notes name the artifacts and the one command that installs a host — a release nobody can act on is a changelog.

## Out
- No `kiln-gateway` binary. The gateway is deployed from its image, and an untested artifact invites an untested deployment.
- No arm64, and no darwin. The host needs KVM and the pinned Firecracker is x86_64.
- No package repository, no apt, no brew, no installer script that pipes to a shell.
- No signing and no attestation. The checksums file is what a person verifies.
- No change to how `kiln init` fetches Firecracker and the kernel. Those stay pinned URLs with their own checksums.
- No automatic upgrade. An operator replaces the binary and restarts the service.

## How I know it works
- A tag push produces a release carrying `kiln` for linux/amd64 and `SHA256SUMS`, and the checksum in that file matches the binary.
- A Debian or Ubuntu box with KVM, no Go and no source downloads the binary, runs `kiln init` and then `kiln serve`, and `GET /v1/health` answers `"kvm": true`.
- `/var/lib/kiln/bin/kilninit` exists on that box after `kiln init`, and a template built there produces a sandbox whose terminal opens.
- `kiln version` prints the tag that produced the binary.
- `docker pull ghcr.io/alternayte/kiln-gateway:<tag>` answers, and what runs from it serves the web UI and `GET /healthz`.
- An edit to `guest/kilninit` with no re-embed fails `just check`.
- A tag pushed on a commit whose gates fail publishes no release, and the failure names the gate.
