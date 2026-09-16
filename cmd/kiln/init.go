package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alternayte/kiln/internal/guestbin"
	"github.com/alternayte/kiln/internal/hostca"
	"github.com/alternayte/kiln/internal/ingress"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/store"
)

const defaultRoot = "/var/lib/kiln"

// configFile is the on-disk config. Later phases add fields.
type configFile struct {
	BearerToken string `json:"bearer_token"`
	ControlAddr string `json:"control_addr"`
	IngressAddr string `json:"ingress_addr"`
	// GatewayAddr is the mTLS listener a gateway dials. An empty value keeps
	// the host private to its control listener.
	GatewayAddr string `json:"gateway_addr,omitempty"`
	// GatewayNames are the hostnames and addresses a gateway dials. They go
	// into the listener's certificate, so a gateway can verify the host it
	// reaches. An empty list takes the host part of GatewayAddr.
	GatewayNames []string `json:"gateway_names,omitempty"`
	// SealKey seals a tenant's registry token before it reaches kiln.db.
	// The database is read by more than the operator; this file is not. A
	// lost key loses every token sealed with it.
	SealKey string            `json:"seal_key,omitempty"`
	Zone    string            `json:"zone,omitempty"`
	ACME    acmeSettings      `json:"acme"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// acmeSettings is the certificate part of the config. v1 imports the
// Cloudflare DNS client, so dns_provider is "cloudflare".
type acmeSettings struct {
	Email       string            `json:"email,omitempty"`
	DNSProvider string            `json:"dns_provider,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
}

// cmdInit is first-run setup. It creates the Kiln root, fetches the pinned
// Firecracker tarball and kernel, verifies each checksum, installs the
// binaries, applies the nftables base, creates the database and the viewer
// tables, and writes config.json with mode 0600.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	zone := fs.String("zone", "", "preview zone for published sandboxes, for example example.com")
	email := fs.String("acme-email", "", "ACME account email for the wildcard certificate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("init takes no positional arguments")
	}
	root := os.Getenv("KILN_ROOT")
	if root == "" {
		root = defaultRoot
	}
	binDir := filepath.Join(root, "bin")
	kernelDir := filepath.Join(root, "kernel")
	for _, dir := range []string{
		root, binDir, kernelDir,
		filepath.Join(root, "templates"),
		filepath.Join(root, "sandboxes"),
		filepath.Join(root, "snapshots"),
		filepath.Join(root, "acme"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if !firecrackerAtPin(filepath.Join(binDir, "firecracker")) || !fileExists(filepath.Join(binDir, "jailer")) {
		archive := filepath.Join(root, "firecracker-"+firecrackerVersion+"-"+arch+".tgz")
		if err := fetch(firecrackerURL, archive, firecrackerSHA256, 0o600); err != nil {
			return err
		}
		if err := extractBinaries(archive, binDir); err != nil {
			return err
		}
		if err := os.Remove(archive); err != nil {
			return err
		}
	}

	kernelPath := filepath.Join(kernelDir, "vmlinux-"+kernelVersion)
	if err := fetch(kernelURL, kernelPath, kernelSHA256, 0o644); err != nil {
		return err
	}

	kilninitPath := filepath.Join(binDir, "kilninit")
	if err := installKilninit(kilninitPath); err != nil {
		return err
	}

	// The nftables base is host state, not sandbox state: one table, the
	// mark-copy chain, forwarding and the reverse path filter.
	if err := network.New().EnsureBase(context.Background()); err != nil {
		return err
	}
	// Opening the store applies every migration, then closes. The daemon
	// opens it again at serve time.
	st, err := store.Open(filepath.Join(root, "kiln.db"))
	if err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	// The CA signs the certificate of every gateway. A second init keeps the
	// CA a gateway already trusts.
	if err := hostca.New(root).Ensure(); err != nil {
		return err
	}

	cfgPath := filepath.Join(root, "config.json")
	cfg, err := writeConfig(cfgPath, *zone, *email)
	if err != nil {
		return err
	}
	if err := initViewer(context.Background(), root, cfg.Zone); err != nil {
		return err
	}

	fmt.Printf("firecracker %s -> %s\n", firecrackerVersion, filepath.Join(binDir, "firecracker"))
	fmt.Printf("jailer %s -> %s\n", firecrackerVersion, filepath.Join(binDir, "jailer"))
	fmt.Printf("kernel %s -> %s\n", kernelVersion, kernelPath)
	fmt.Printf("kilninit -> %s\n", kilninitPath)
	fmt.Printf("nftables base -> table inet kiln\n")
	fmt.Printf("database -> %s\n", filepath.Join(root, "kiln.db"))
	fmt.Printf("gateway CA -> %s\n", filepath.Join(root, hostca.Dir))
	if cfg.Zone != "" {
		fmt.Printf("preview zone -> %s\n", cfg.Zone)
	}
	if cfg.ACME.Email != "" {
		fmt.Printf("acme email -> %s (%s)\n", cfg.ACME.Email, cfg.ACME.DNSProvider)
	}
	fmt.Printf("config -> %s (mode 0600)\n", cfgPath)
	return nil
}

// initViewer opens the viewer store on the Kiln database, applies its
// migrations, and creates the first viewer from the environment. There is no
// self-signup, so the operator makes the first account here.
func initViewer(ctx context.Context, root, zone string) error {
	auth, db, err := ingress.NewAuth(ctx, filepath.Join(root, "kiln.db"), zone)
	if err != nil {
		return err
	}
	defer db.Close()
	address := strings.TrimSpace(os.Getenv("KILN_VIEWER_EMAIL"))
	password := os.Getenv("KILN_VIEWER_PASSWORD")
	if address == "" && password == "" {
		fmt.Printf("viewer -> none. Set KILN_VIEWER_EMAIL and KILN_VIEWER_PASSWORD and re-run kiln init to create the first viewer.\n")
		return nil
	}
	if address == "" || password == "" {
		return fmt.Errorf("init: KILN_VIEWER_EMAIL and KILN_VIEWER_PASSWORD must be set together")
	}
	exists, err := ingress.ViewerExists(ctx, auth, address)
	if err != nil {
		return fmt.Errorf("init: viewer lookup: %w", err)
	}
	if exists {
		fmt.Printf("viewer -> %s (already exists)\n", address)
		return nil
	}
	if err := ingress.CreateViewer(ctx, auth, address, password); err != nil {
		return fmt.Errorf("init: create viewer: %w", err)
	}
	fmt.Printf("viewer -> %s\n", address)
	return nil
}

// installKilninit writes the guest init this binary carries. It behaves the
// same in a repository and on a bare host, so one install path is the only
// path, and the agent a host runs always matches the host that wrote it.
func installKilninit(dst string) error {
	return guestbin.Write(dst)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firecrackerAtPin(path string) bool {
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	return len(fields) > 0 && fields[len(fields)-1] == firecrackerVersion
}

var httpClient = &http.Client{Timeout: 15 * time.Minute}

// fetch downloads url to dst unless dst already has the pinned SHA256. A bad
// checksum removes the download and fails. Nothing lands at dst unverified.
func fetch(url, dst, wantSHA string, mode os.FileMode) error {
	if got, err := fileSHA256(dst); err == nil && got == wantSHA {
		return nil
	}
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %s", url, resp.Status)
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmp)
		if copyErr != nil {
			return fmt.Errorf("fetch %s: %w", url, copyErr)
		}
		return closeErr
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != wantSHA {
		os.Remove(tmp)
		return fmt.Errorf("fetch %s: checksum mismatch: got %s, want %s", url, got, wantSHA)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Chmod(dst, mode)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinaries installs firecracker and jailer from the release tarball.
// It matches entry names by prefix, so a version bump needs no code change.
func extractBinaries(archive, binDir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if strings.Contains(base, ".debug") {
			continue
		}
		var name string
		switch {
		case strings.HasPrefix(base, "firecracker-v"):
			name = "firecracker"
		case strings.HasPrefix(base, "jailer-v"):
			name = "jailer"
		default:
			continue
		}
		if err := writeExecutable(filepath.Join(binDir, name), tr); err != nil {
			return err
		}
		found[name] = true
	}
	for _, name := range []string{"firecracker", "jailer"} {
		if !found[name] {
			return fmt.Errorf("%s: no %s entry", archive, name)
		}
	}
	return nil
}

func writeExecutable(dst string, r io.Reader) error {
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// writeConfig keeps an existing bearer token, so a re-run does not lock out
// clients. Missing listeners take the fixed v1 addresses. The zone, the ACME
// email and the Cloudflare API token come from the arguments and the
// environment.
func writeConfig(path, zone, email string) (configFile, error) {
	cfg := configFile{}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return configFile{}, fmt.Errorf("%s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return configFile{}, err
	}
	if cfg.BearerToken == "" {
		token, err := newToken()
		if err != nil {
			return configFile{}, err
		}
		cfg.BearerToken = token
	}
	if cfg.SealKey == "" {
		key, err := store.NewKey()
		if err != nil {
			return configFile{}, err
		}
		cfg.SealKey = base64.StdEncoding.EncodeToString(key)
	}
	if cfg.ControlAddr == "" {
		cfg.ControlAddr = "127.0.0.1:8080"
	}
	if cfg.IngressAddr == "" {
		cfg.IngressAddr = "0.0.0.0:443"
	}
	if zone != "" {
		cfg.Zone = zone
	}
	if email != "" {
		cfg.ACME.Email = email
	}
	if token := os.Getenv(ingress.CloudflareTokenKey); token != "" {
		if cfg.ACME.Credentials == nil {
			cfg.ACME.Credentials = map[string]string{}
		}
		cfg.ACME.Credentials[ingress.CloudflareTokenKey] = token
		cfg.ACME.DNSProvider = ingress.CloudflareProvider
	}
	if err := saveConfig(path, cfg); err != nil {
		return configFile{}, err
	}
	return cfg, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// saveConfig writes config.json atomically, mode 0600. It holds the bearer
// token and the sealing key, so it is the operator's file and nobody else's.
func saveConfig(path string, cfg configFile) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".part"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}
