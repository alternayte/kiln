# Kiln infrastructure

Pulumi program. It provisions a Hetzner host, copies this repo to it, and runs
`go build ./...` and `just check` there. With `kiln:gate` set it also runs
`kiln init` and one gate.

## What Hetzner allows

- **Dedicated server: yes.** Bare metal. Hetzner documents Linux KVM on
  dedicated servers. Firecracker needs `/dev/kvm`, so the KVM gates run here.
- **Cloud server: no.** Hetzner Cloud FAQ: "is nested virtualization
  possible?" — "No, this is not possible on cloud server." A Cloud server runs
  `just check` only.
- Pulumi has no Hetzner Robot provider. Order the dedicated server in the
  Hetzner web console. Pulumi manages everything after that.

## Path A: dedicated server, runs the gates

1. Order a dedicated server:
   - Open https://robot.hetzner.com → Server → Order, or the Server Market.
   - Choose Ubuntu 24.04 and add the public key `~/.ssh/id_rsa.pub`.
2. Wait for the server. Note its IPv4 address, for example `203.0.113.7`.
3. Check SSH from the laptop: `ssh root@203.0.113.7 true`
4. Run:
   ```sh
   cd infra
   pulumi login --local
   pulumi stack init kiln
   pulumi config set kiln:existingHost 203.0.113.7
   pulumi config set kiln:privateKeyPath ~/.ssh/id_rsa
   pulumi config set kiln:gate P1
   pulumi up
   ```
5. `pulumi up` installs packages, copies the repo, runs `just check`, runs
   `kiln init`, and runs `just gate P1`. It prints the gate output.

Drop `kiln:gate` to run the checks only.

## Path B: Cloud server, runs the checks only

1. Create an API token: https://console.hetzner.cloud → project → Security →
   API tokens → Generate (Read & Write).
2. Run:
   ```sh
   export HCLOUD_TOKEN=...
   cd infra
   pulumi login --local
   pulumi stack init kiln
   pulumi config set kiln:sshKeyPath ~/.ssh/id_rsa.pub
   pulumi up
   ```
3. `pulumi up` creates the SSH key, firewall and server, then runs `just check`
   on it.
4. `pulumi destroy` removes the server.

## Config

Only these keys matter. Defaults are in `main.go`.

| Key | Use |
|---|---|
| `kiln:existingHost` | dedicated server IP; skips all Cloud resources |
| `kiln:sshKeyPath` | public key for a Cloud server |
| `kiln:privateKeyPath` | private key for SSH; default `~/.ssh/id_rsa` |
| `kiln:gate` | gate to run after the checks, for example `P1` |

`pulumi up` copies the whole working tree, including untracked files. Gates run
under `sudo`, because jailer needs root.
