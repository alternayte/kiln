// Command infra bootstraps an OVH server and runs the Kiln checks on it.
//
// Pulumi cannot order an OVH server: the OVH provider has no checkout
// resource. Order the server on the OVH site, then set kiln:host. See
// README.md.
package main

import (
	"fmt"
	"os"
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
		return fmt.Errorf("kiln:host is required: order an OVH server, then set its IPv4 address (see README.md)")
	}
	sshUser := or(cfg.Get("sshUser"), "root")
	remoteDir := or(cfg.Get("remoteDir"), "/root/kiln")
	privateKeyPath := or(expand(cfg.Get("privateKeyPath")), expand("~/.ssh/id_rsa"))
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return fmt.Errorf("kiln:privateKeyPath: %w", err)
	}

	conn := remote.ConnectionArgs{
		Host:       pulumi.String(host),
		User:       pulumi.String(sshUser),
		PrivateKey: pulumi.String(string(privateKey)),
	}
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	copied, err := remote.NewCopyToRemote(ctx, "repo", &remote.CopyToRemoteArgs{
		Connection: conn,
		RemotePath: pulumi.String(remoteDir),
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
if ! command -v go >/dev/null 2>&1; then
  sudo apt-get install -y -qq golang-go
fi
sudo modprobe kvm_intel 2>/dev/null || sudo modprobe kvm_amd 2>/dev/null || true
sudo mkdir -p /var/lib/kiln
cd %s
go build ./...
just check
`, remoteDir)
}

// gateScript runs kiln init and one gate. The gate needs root and /dev/kvm.
func gateScript(remoteDir, gate string) string {
	return fmt.Sprintf(`set -euo pipefail
cd %s
[ -e /dev/kvm ] || { echo "gate %s: /dev/kvm is missing on this host" >&2; exit 1; }
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
