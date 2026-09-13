# Kiln infrastructure

Pulumi program for a Hetzner host that runs the Kiln checks. It creates the
host resources, copies this repo to the host, and runs `go build ./...` and
`just check` there.

## Hetzner Cloud cannot run the KVM gates

Hetzner Cloud servers do not support nested virtualization. Hetzner's FAQ
answers "is nested virtualization possible?" with "No, this is not possible on
cloud server." Firecracker needs `/dev/kvm`, so `just gate P1` and every later
gate cannot run on a Cloud server.

Use one of these paths:

- **Dedicated server (the KVM path).** Order a Hetzner dedicated server (Robot
  or the server market) with the public key from `kiln:sshKeyPath`. Then set
  `kiln:existingHost` to its IP. Pulumi skips the Cloud resources and copies
  the repo to that host. `preflight.sh` and the KVM gates run there.
- **Cloud server (checks only).** Leave `kiln:existingHost` unset. Pulumi
  creates the Cloud resources and runs the fast checks. The KVM gates stay
  unavailable.

Pulumi has no provider for Hetzner Robot, so the dedicated server order is a
manual step. The server is then fully managed by this program.

## Config

| Key | Default | Meaning |
|---|---|---|
| `kiln:sshKeyPath` | required for Cloud | public key to register and inject |
| `kiln:privateKeyPath` | `~/.ssh/id_rsa` | private key for SSH commands |
| `kiln:existingHost` | none | IP or hostname of an existing host |
| `kiln:sshUser` | `root` | SSH user |
| `kiln:remoteDir` | `/root/kiln` | repo destination on the host |
| `kiln:serverType` | `cpx21` | Hetzner Cloud server type |
| `kiln:location` | `fsn1` | Hetzner Cloud location |
| `kiln:image` | `ubuntu-24.04` | Hetzner Cloud image |
| `kiln:gate` | none | gate to run after the checks, for example `P1` |

## Use

```sh
cd infra
pulumi stack init kiln
pulumi config set kiln:sshKeyPath ~/.ssh/id_rsa.pub

# Cloud server: checks only.
pulumi up

# Dedicated server: order it first, then point Pulumi at it.
pulumi config set kiln:existingHost <dedicated-ip>
pulumi config set kiln:gate P1
pulumi up
```

`pulumi up` copies the whole working tree, including untracked files. The
gate commands run under `sudo`, because jailer needs root.
