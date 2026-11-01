//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/alternayte/kiln/internal/guestproto"
)

const (
	// trustDir holds what a template build added for the tenant CA. It lies in
	// the application root, so a snapshot and each fork hold it already.
	trustDir = "/etc/kiln"
	// trustCA is the tenant CA alone, for a client that adds to its own roots.
	trustCA = trustDir + "/ca.pem"
	// trustBundle is the system roots with the tenant CA, for a client that
	// replaces its roots with one file.
	trustBundle = trustDir + "/ca-bundle.pem"
)

// systemBundles are the trust stores common images ship. A link between two of
// them counts once.
var systemBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// trustEnv points the clients that do not always read the system store at the
// tenant CA. It is empty on a template built before credential bindings.
func trustEnv() map[string]string {
	if _, err := os.Stat(trustBundle); err != nil {
		return nil
	}
	return map[string]string{
		"NODE_EXTRA_CA_CERTS": trustCA,
		"SSL_CERT_FILE":       trustBundle,
		"REQUESTS_CA_BUNDLE":  trustBundle,
	}
}

func runTrustCA(conn net.Conn, req *guestproto.Request) {
	if err := installTrust(req.CertPEM, systemBundles, trustDir); err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	writeResult(conn, guestproto.Result{OK: true})
}

// installTrust appends a certificate to each trust store of the root, and creates
// the first one when the image ships none.
func installTrust(certPEM string, bundles []string, dir string) error {
	if certPEM == "" {
		return fmt.Errorf("no certificate")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte(certPEM), 0o644); err != nil {
		return err
	}
	seen := map[string]bool{}
	first := ""
	for _, path := range bundles {
		real, err := filepath.EvalSymlinks(path)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		if first == "" {
			first = real
		}
		if err := appendCert(real, certPEM); err != nil {
			return err
		}
	}
	if first == "" {
		first = bundles[0]
		if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
			return err
		}
		if err := appendCert(first, certPEM); err != nil {
			return err
		}
	}
	whole, err := os.ReadFile(first)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ca-bundle.pem"), whole, 0o644)
}

func appendCert(path, certPEM string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + certPEM)
	return err
}
