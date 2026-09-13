// Command infra provisions a Hetzner host and runs the Kiln checks on it.
//
// See README.md. Hetzner Cloud does not support nested virtualization, so the
// KVM gates need an existing dedicated server; set kiln:existingHost for that.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi-hcloud/sdk/go/hcloud"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func main() {
	pulumi.Run(run)
}

func run(ctx *pulumi.Context) error {
	cfg := config.New(ctx, "kiln")
	sshUser := cfg.Get("sshUser")
	if sshUser == "" {
		sshUser = "root"
	}
	remoteDir := cfg.Get("remoteDir")
	if remoteDir == "" {
		remoteDir = "/root/kiln"
	}

	privateKeyPath := expand(cfg.Get("privateKeyPath"))
	if privateKeyPath == "" {
		privateKeyPath = expand("~/.ssh/id_rsa")
	}
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return fmt.Errorf("kiln:privateKeyPath: %w", err)
	}

	var host pulumi.StringOutput
	if existing := cfg.Get("existingHost"); existing != "" {
		ctx.Log.Info("using the existing host "+existing, nil)
		host = pulumi.String(existing).ToStringOutput()
	} else {
		pub, err := os.ReadFile(expand(cfg.Require("sshKeyPath")))
		if err != nil {
			return fmt.Errorf("kiln:sshKeyPath: %w", err)
		}
		key, err := hcloud.NewSshKey(ctx, "kiln", &hcloud.SshKeyArgs{
			Name:      pulumi.String("kiln"),
			PublicKey: pulumi.String(strings.TrimSpace(string(pub))),
		})
		if err != nil {
			return err
		}
		fw, err := hcloud.NewFirewall(ctx, "kiln", &hcloud.FirewallArgs{
			Name: pulumi.String("kiln"),
			Rules: hcloud.FirewallRuleArray{
				hcloud.FirewallRuleArgs{
					Description: pulumi.String("ssh"),
					Direction:   pulumi.String("in"),
					Protocol:    pulumi.String("tcp"),
					Port:        pulumi.String("22"),
					SourceIps:   pulumi.StringArray{pulumi.String("0.0.0.0/0"), pulumi.String("::/0")},
				},
			},
		})
		if err != nil {
			return err
		}
		serverType := or(cfg.Get("serverType"), "cpx21")
		location := or(cfg.Get("location"), "fsn1")
		image := or(cfg.Get("image"), "ubuntu-24.04")
		srv, err := hcloud.NewServer(ctx, "kiln", &hcloud.ServerArgs{
			Name:       pulumi.String("kiln-dev"),
			ServerType: pulumi.String(serverType),
			Image:      pulumi.String(image),
			Location:   pulumi.String(location),
			SshKeys:    pulumi.StringArray{key.Name},
			FirewallIds: pulumi.IntArray{
				fw.ID().ApplyT(func(id string) (int, error) { return strconv.Atoi(id) }).(pulumi.IntOutput),
			},
			PublicNets: hcloud.ServerPublicNetArray{
				hcloud.ServerPublicNetArgs{
					Ipv4Enabled: pulumi.Bool(true),
					Ipv6Enabled: pulumi.Bool(true),
				},
			},
			Labels: pulumi.StringMap{"kiln": pulumi.String("dev")},
		})
		if err != nil {
			return err
		}
		host = srv.Ipv4Address
	}

	conn := remote.ConnectionArgs{
		Host:       host,
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
		Create:     pulumi.String(bootstrapScript(remoteDir)),
	})
	if err != nil {
		return err
	}
	ctx.Export("host", host)
	ctx.Export("check", check.Stdout)

	if gate := cfg.Get("gate"); gate != "" {
		gateCmd, err := remote.NewCommand(ctx, "gate", &remote.CommandArgs{
			Connection: conn,
			Triggers:   pulumi.Array{check.ID()},
			Create: pulumi.String(fmt.Sprintf(`set -euo pipefail
cd %s
sudo env "PATH=$PATH" go run ./cmd/kiln init
sudo env "PATH=$PATH" just gate %s
`, remoteDir, gate)),
		})
		if err != nil {
			return err
		}
		ctx.Export("gate", gateCmd.Stdout)
	}
	return nil
}

// bootstrapScript installs the host packages, then runs the fast checks.
func bootstrapScript(remoteDir string) string {
	return fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo apt-get install -y -qq iproute2 nftables e2fsprogs sqlite3 curl ca-certificates tar gzip jq git make busybox-static
  command -v just >/dev/null 2>&1 || sudo apt-get install -y -qq just
  command -v go >/dev/null 2>&1 || sudo apt-get install -y -qq golang-go
fi
sudo mkdir -p /var/lib/kiln
cd %s
go build ./...
just check
`, remoteDir)
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
