package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alternayte/kiln/internal/hostca"
)

// cmdCA issues and revokes the client certificates a gateway uses to reach
// this host.
func cmdCA(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("ca needs a command: issue, revoke")
	}
	root := kilnRoot()
	ca := hostca.New(root)
	// A host that ran an older init has no CA yet, and creating one here
	// costs nothing when it already exists.
	if err := ca.Ensure(); err != nil {
		return err
	}
	switch args[0] {
	case "issue":
		fs := flag.NewFlagSet("ca issue", flag.ContinueOnError)
		name := fs.String("name", "", "name of the gateway this certificate belongs to")
		days := fs.Int("days", 365, "days until the certificate expires")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("ca issue: --name is required")
		}
		caPEM, certPEM, keyPEM, serial, err := ca.Issue(*name, *days)
		if err != nil {
			return err
		}
		fmt.Printf("# serial %s, expires in %d days. The key is printed once.\n", serial, *days)
		fmt.Printf("# KILN_CA_CERT\n%s\n# KILN_CLIENT_CERT\n%s\n# KILN_CLIENT_KEY\n%s", caPEM, certPEM, keyPEM)
		return nil
	case "revoke":
		fs := flag.NewFlagSet("ca revoke", flag.ContinueOnError)
		serial := fs.String("serial", "", "serial of the certificate to refuse from now on")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *serial == "" {
			return fmt.Errorf("ca revoke: --serial is required")
		}
		if err := ca.Revoke(*serial); err != nil {
			return err
		}
		fmt.Printf("revoked %s\n", *serial)
		return nil
	default:
		return fmt.Errorf("ca: unknown command %q", args[0])
	}
}

// cmdGatewayEnv prints everything one gateway needs, in one block the
// operator pastes into a credential store.
func cmdGatewayEnv(args []string) error {
	fs := flag.NewFlagSet("gateway-env", flag.ContinueOnError)
	name := fs.String("name", "gateway", "name of the gateway this certificate belongs to")
	days := fs.Int("days", 365, "days until the certificate expires")
	addr := fs.String("host", "", "address the gateway dials, for example kiln.example.com:8443")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := kilnRoot()
	cfg, err := readConfig(filepath.Join(root, "config.json"))
	if err != nil {
		return err
	}
	if cfg.BearerToken == "" {
		return fmt.Errorf("gateway-env: %s has no bearer_token; run kiln init", filepath.Join(root, "config.json"))
	}
	target := *addr
	if target == "" {
		target = cfg.GatewayAddr
	}
	if target == "" {
		return fmt.Errorf("gateway-env: --host is required while config.json has no gateway_addr")
	}
	ca := hostca.New(root)
	if err := ca.Ensure(); err != nil {
		return err
	}
	caPEM, certPEM, keyPEM, serial, err := ca.Issue(*name, *days)
	if err != nil {
		return err
	}
	fmt.Printf("# kiln gateway credentials for %q, serial %s. The key is printed once.\n", *name, serial)
	fmt.Printf("KILN_HOST_URL=https://%s\n", target)
	fmt.Printf("KILN_HOST_TOKEN=%s\n", cfg.BearerToken)
	fmt.Printf("KILN_CA_CERT=%q\n", caPEM)
	fmt.Printf("KILN_CLIENT_CERT=%q\n", certPEM)
	fmt.Printf("KILN_CLIENT_KEY=%q\n", keyPEM)
	return nil
}

func kilnRoot() string {
	if root := strings.TrimSpace(os.Getenv("KILN_ROOT")); root != "" {
		return root
	}
	return defaultRoot
}
