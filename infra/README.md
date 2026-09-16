# Kiln infrastructure

Pulumi program. It copies this repo to a server, installs the host packages,
loads the KVM module, and runs `go build ./...` and `just check`. With
`kiln:gate` set it also runs `kiln init` and one gate.

The steps start from a server you order. Nothing here points at an existing
host: `kiln:host` names the server to bootstrap. Any x86_64 Ubuntu 24.04 or
Debian 12 machine with KVM works. `pulumi up` installs the Go version that
`go.mod` names, and the gate run installs Firecracker with
`scripts/dev-env.sh`. Two providers are documented: OVH Eco and Scaleway
Dedibox. Choose one, order the server, then follow its steps. The Pulumi steps
are the same for both.

## Install from a release

A tagged release carries the host binary, so a box needs no Go and no source:

```sh
curl -fsSLO https://github.com/alternayte/kiln/releases/latest/download/kiln
curl -fsSLO https://github.com/alternayte/kiln/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS
sudo install -m755 kiln /usr/local/bin/kiln
sudo kiln init --zone example.com --acme-email you@example.com
sudo kiln serve
```

The binary carries the guest agent, so `kiln init` writes it out. The steps
below bootstrap a box from source instead, which is what a change to Kiln
itself needs.

## OVH Eco

- **Dedicated server: yes.** Bare metal, so `/dev/kvm` is available and the
  KVM gates run.
- **VPS: no.** No nested virtualization. OVH Public Cloud passes the vmx flag,
  but OVH warns that live migration can panic your kernel. Do not use either
  for gates.
- **Pulumi cannot order an OVH server.** The OVH provider has no checkout
  resource. Order the server on the OVH site; Pulumi manages everything after
  that.

### Recommended server

- **Eco KS-B: €9.99/month excl. VAT**, Intel Xeon E5-1620v2, bare metal. OVH
  bills monthly. The 12/24-month prepay is optional, so there is no lock-in.
- **Eco KS-2: €18.99/month excl. VAT** if the KS-B is out of stock, Xeon-D 1540.
- Eco stock is limited. Check https://www.ovhcloud.com/en/bare-metal-cloud/eco/

### Steps

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

## Scaleway Dedibox

- **Dedibox: yes.** Bare metal, so `/dev/kvm` is available.
- **Scaleway Instances: no.** Do not use them for gates.
- **Pulumi cannot order or install a Dedibox.** No Pulumi provider covers the
  Dedibox console. Order and install the system in the console; Pulumi manages
  everything after that.
- **Start-2-S-SATA (€4.99/month) does not work.** Its Atom C2338 or C2350 has
  no XSAVE. Firecracker then fails every start with `Missing KVM capabilities:
  0x38` (KVM_CAP_XCRS). The CPU needs the `xsave` flag in `/proc/cpuinfo`.
- **Start-2-L (€22.99/month)**: Xeon D-1531, 32 GB, SSD. The cheapest Dedibox
  whose CPU has XSAVE.

### Steps

1. Order the server at https://www.scaleway.com/en/dedibox/ and choose Paris
   or Amsterdam.
2. Add your SSH key: console https://console.online.net, **Account >
   Credentials > SSH keys**, paste `~/.ssh/id_rsa.pub`.
3. Install the system: **Server > Server list > Manage > Install**.
   - Distribution: **Debian 12**. The Dedibox list offers no Ubuntu newer
     than 20.04. Do not use Ubuntu 20.04: its 5.4 kernel is older than the
     host kernels Firecracker supports.
   - Partitioning: keep the default.
   - User: set a user name, for example `kiln`, and a password. Set a root
     password too; step 6 needs it if `sudo` is not installed.
   - SSH keys: select the key from step 2.
   - Start the install. It wipes the disk.
4. When the console shows the server as installed, note its IPv4 address.
5. Check SSH and KVM from the laptop:
   ```sh
   ssh kiln@<ip> 'ls /dev/kvm; grep -cE "vmx|svm" /proc/cpuinfo; grep -cw xsave /proc/cpuinfo'
   ```
   `/dev/kvm` must exist and both counts must not be 0. If the `vmx|svm`
   count is 0, open a ticket. If the `xsave` count is 0, the CPU cannot run
   Firecracker: cancel and order another plan.
6. The Dedibox install makes a non-root user and may omit `sudo`. Pulumi runs
   `sudo` without a terminal, so give that user passwordless sudo. Type these
   commands in a real terminal, one at a time. Do not paste them as one line:
   a wrapped paste writes a broken sudoers file.
   ```sh
   ssh kiln@<ip>
   su -                      # root password from step 3
   apt-get install -y sudo
   echo 'kiln ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/kiln
   visudo -c                 # must print: /etc/sudoers.d/kiln: parsed OK
   exit
   exit
   ```
   Check from the laptop: `ssh kiln@<ip> sudo -n true` returns with no prompt.
7. If your SSH key has a passphrase, load it into an agent. Pulumi uses the
   agent when `kiln:privateKeyPath` is unset:
   ```sh
   eval "$(ssh-agent -s)"
   ssh-add --apple-use-keychain ~/.ssh/id_rsa
   ```
8. Run, in the same terminal:
   ```sh
   cd infra
   pulumi login --local
   export PULUMI_CONFIG_PASSPHRASE=   # the kiln stack has no secrets
   pulumi stack init kiln     # or: pulumi stack select kiln
   pulumi config set kiln:host <ip>
   pulumi config set kiln:sshUser kiln
   pulumi config set kiln:remoteDir /home/kiln/kiln
   pulumi config set kiln:gate P1
   pulumi up
   ```
   The last line must be `gate P1` output with `ok`. A new server with the
   same stack needs only `pulumi config set kiln:host <new ip>` and `pulumi up`.

## After `pulumi up`

Remove `kiln:gate` for a checks-only run. `pulumi up` again after a code change
copies the tree and re-runs the commands.

## Run as a service

`infra/kiln.service` runs `kiln serve` under systemd. Install it after `kiln
init`, on the server, from the repo:

```sh
go build -o /tmp/kiln ./cmd/kiln
sudo install -m 755 /tmp/kiln /var/lib/kiln/bin/kiln
sudo install -m 644 infra/kiln.service /etc/systemd/system/kiln.service
sudo systemctl daemon-reload
sudo systemctl enable --now kiln
journalctl -u kiln -f
```

`KillMode=process` stops only the daemon. A restart adopts the running
microVMs, so `systemctl restart kiln` keeps every sandbox. Stop the service
before a gate or `just demo` with a daemon of its own: both bind the same
ports. After a code change, build and install the binary again, then
`sudo systemctl restart kiln`.

## Config

| Key | Default | Use |
|---|---|---|
| `kiln:host` | none, required | server IPv4 address |
| `kiln:sshUser` | `root` | SSH user |
| `kiln:privateKeyPath` | none | private key file without a passphrase. Unset: use the agent in `SSH_AUTH_SOCK` |
| `kiln:remoteDir` | `/root/kiln` | repo destination on the host |
| `kiln:gate` | none | gate to run after the checks, for example `P1` |

`pulumi up` copies the whole working tree, including untracked files. Gates run
under `sudo`, because jailer needs root.

## Preview setup (P6)

`just gate P6` and `just demo` need a public hostname, wildcard DNS and an
ACME account. One-time setup after the server has its IP:

1. Buy a domain and put its DNS on Cloudflare. The one DNS client v1 imports
   is `github.com/libdns/cloudflare`.
2. Create an API token with `Zone:Read` and `Zone:DNS:Edit` for that zone.
3. Add a wildcard record: type `A`, name `*`, content the server IP, proxy
   off (DNS only). The wildcard certificate is `*.<zone>`, so every preview
   hostname is covered.
4. Open TCP 443. Nothing else is needed inbound: the ACME challenge is a TXT
   record written through the Cloudflare API.
5. On the server, from the repo:

```sh
export KILN_ROOT=/var/lib/kiln
export CLOUDFLARE_DNS_API_TOKEN=...        # the token from step 2
export KILN_VIEWER_EMAIL=you@example.com   # the first viewer account
export KILN_VIEWER_PASSWORD=...            # at least 8 characters
sudo -E go run ./cmd/kiln init --zone example.com --acme-email you@example.com
sudo env "PATH=$PATH" just gate P6
sudo env "PATH=$PATH" just demo
```

`gate P6` creates its own viewer account and cleans up after itself. The
demo only asserts the team preview's challenge, so a viewer is for the
operator, not for the script.

The wildcard certificate is cached under `$KILN_ROOT/acme`. Let's Encrypt
limits duplicate certificates to five a week, so keep that directory between
runs.
