// Command infra bootstraps an ordered server and runs the Kiln checks on it.
//
// Pulumi cannot order an OVH Eco server or a Scaleway Dedibox. Order and
// install the server by hand, then set kiln:host. See README.md.
package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func main() {
	pulumi.Run(run)
}

func run(ctx *pulumi.Context) error {
	cfg := config.New(ctx, "kiln")
	host := cfg.Get("host")
	if host == "" {
		return fmt.Errorf("kiln:host is required: order a server, then set its IPv4 address (see README.md)")
	}
	sshUser := or(cfg.Get("sshUser"), "root")
	remoteDir := or(cfg.Get("remoteDir"), "/root/kiln")
	conn := remote.ConnectionArgs{
		Host: pulumi.String(host),
		User: pulumi.String(sshUser),
	}
	// A key with a passphrase cannot be read here, so an unset path means the
	// SSH agent holds the key.
	if p := cfg.Get("privateKeyPath"); p != "" {
		privateKey, err := os.ReadFile(expand(p))
		if err != nil {
			return fmt.Errorf("kiln:privateKeyPath: %w", err)
		}
		conn.PrivateKey = pulumi.String(string(privateKey))
	} else if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn.AgentSocketPath = pulumi.String(sock)
	} else {
		return fmt.Errorf("no SSH key: set kiln:privateKeyPath, or load the key into an agent and set SSH_AUTH_SOCK")
	}
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	// CopyToRemote puts the source directory inside RemotePath, so copy into
	// the parent and require the repo directory name to match.
	if path.Base(remoteDir) != filepath.Base(repoRoot) {
		return fmt.Errorf("kiln:remoteDir %q must end in /%s, the repo directory name", remoteDir, filepath.Base(repoRoot))
	}
	copied, err := remote.NewCopyToRemote(ctx, "repo", &remote.CopyToRemoteArgs{
		Connection: conn,
		RemotePath: pulumi.String(path.Dir(remoteDir)),
		Source:     pulumi.NewFileArchive(repoRoot),
	})
	if err != nil {
		return err
	}
	check, err := remote.NewCommand(ctx, "check", &remote.CommandArgs{
		Connection: conn,
		Triggers:   pulumi.Array{copied.ID()},
		Create:     pulumi.String(checkScript(remoteDir)),
	})
	if err != nil {
		return err
	}
	ctx.Export("host", pulumi.String(host))
	ctx.Export("check", check.Stdout)

	if gate := cfg.Get("gate"); gate != "" {
		gateCmd, err := remote.NewCommand(ctx, "gate", &remote.CommandArgs{
			Connection: conn,
			Triggers:   pulumi.Array{check.ID()},
			Create:     pulumi.String(gateScript(remoteDir, gate)),
		})
		if err != nil {
			return err
		}
		ctx.Export("gate", gateCmd.Stdout)
	}
	return nil
}

// checkScript installs the host packages, loads the KVM module, then runs the
// fast checks.
func checkScript(remoteDir string) string {
	return fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq iproute2 nftables e2fsprogs sqlite3 curl ca-certificates tar gzip jq git make busybox-static
if ! command -v just >/dev/null 2>&1; then
  sudo apt-get install -y -qq just || curl -fsSL https://just.systems/install.sh | sudo bash -s -- --to /usr/local/bin
fi
cd %s
# CopyToRemote drops file modes, so restore the executable bits git records.
git ls-files -s | awk '$1 == "100755" { print $4 }' | xargs -r chmod +x
# Distribution Go packages lag go.mod, so install the exact version it names.
want="go$(sed -n 's/^go //p' go.mod)"
if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null || true)" != "$want" ]; then
  curl -fsSL "https://go.dev/dl/${want}.linux-amd64.tar.gz" -o /tmp/go.tgz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf /tmp/go.tgz
  rm /tmp/go.tgz
fi
sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
export PATH=/usr/local/go/bin:$PATH
sudo modprobe kvm_intel 2>/dev/null || sudo modprobe kvm_amd 2>/dev/null || true
sudo mkdir -p /var/lib/kiln
go build ./...
just check
`, remoteDir)
}

// gateScript installs Firecracker and the guest kernel, then runs kiln init
// and one gate. The gate needs root and /dev/kvm.
func gateScript(remoteDir, gate string) string {
	return fmt.Sprintf(`set -euo pipefail
# Debian keeps nft and mke2fs in sbin, which a non-root login PATH omits.
export PATH=/usr/local/go/bin:/usr/local/sbin:/usr/sbin:/sbin:$PATH
cd %s
[ -e /dev/kvm ] || { echo "gate %s: /dev/kvm is missing on this host" >&2; exit 1; }
sudo env "PATH=$PATH" bash scripts/dev-env.sh
sudo env "PATH=$PATH" go run ./cmd/kiln init
sudo env "PATH=$PATH" just gate %s
`, remoteDir, gate, gate)
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}
