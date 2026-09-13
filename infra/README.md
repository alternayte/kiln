# Kiln infrastructure

Pulumi program. It copies this repo to a server, installs the host packages,
loads the KVM module, and runs `go build ./...` and `just check`. With
`kiln:gate` set it also runs `kiln init` and one gate.

The steps start from a server you order. Nothing here points at an existing
host: `kiln:host` names the server to bootstrap. Any x86_64 Ubuntu 24.04
machine with KVM works. The order steps use OVH Eco as the example.

## What OVH allows

- **Dedicated server: yes.** Bare metal, so `/dev/kvm` is available and the
  KVM gates run.
- **VPS: no.** No nested virtualization. OVH Public Cloud passes the vmx flag,
  but OVH warns that live migration can panic your kernel. Do not use either
  for gates.
- **Pulumi cannot order an OVH server.** The OVH provider has no checkout
  resource. Order the server on the OVH site; Pulumi manages everything after
  that.

## Recommended server

- **Eco KS-B: €9.99/month excl. VAT**, Intel Xeon E5-1620v2, bare metal. OVH
  bills monthly. The 12/24-month prepay is optional, so there is no lock-in.
- **Eco KS-2: €18.99/month excl. VAT** if the KS-B is out of stock, Xeon-D 1540.
- Eco stock is limited. Check https://www.ovhcloud.com/en/bare-metal-cloud/eco/

## Steps

1. Order the server:
   - Open https://www.ovhcloud.com/en/bare-metal-cloud/eco/
   - Choose KS-B, or KS-2 when the KS-B is sold out.
   - Choose a datacenter and the operating system **Ubuntu 24.04**.
   - Add your public key `~/.ssh/id_rsa.pub` during the order.
   - Pay. OVH installs the system and shows the IPv4 address.
2. Wait for the install, then check SSH from the laptop:
   `ssh root@<ip> true`
3. Run:
   ```sh
   cd infra
   pulumi login --local
   pulumi stack init kiln     # or: pulumi stack select kiln
   pulumi config set kiln:host <ip>
   pulumi config set kiln:privateKeyPath ~/.ssh/id_rsa
   pulumi config set kiln:gate P1
   pulumi up
   ```
4. `pulumi up` does the rest: packages, KVM module, repo copy, `just check`,
   `kiln init`, `just gate P1`. It prints the gate output.

Remove `kiln:gate` for a checks-only run. `pulumi up` again after a code change
copies the tree and re-runs the commands.

## Config

| Key | Default | Use |
|---|---|---|
| `kiln:host` | none, required | server IPv4 address |
| `kiln:sshUser` | `root` | SSH user |
| `kiln:privateKeyPath` | `~/.ssh/id_rsa` | private key for SSH |
| `kiln:remoteDir` | `/root/kiln` | repo destination on the host |
| `kiln:gate` | none | gate to run after the checks, for example `P1` |

`pulumi up` copies the whole working tree, including untracked files. Gates run
under `sudo`, because jailer needs root.
