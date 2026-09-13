package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultRoot = "/var/lib/kiln"

// configFile is the on-disk config. Later phases add fields.
type configFile struct {
	BearerToken string `json:"bearer_token"`
	ControlAddr string `json:"control_addr"`
	IngressAddr string `json:"ingress_addr"`
}

// cmdInit is first-run setup. It fetches the pinned Firecracker tarball and
// kernel, verifies each checksum, installs firecracker and jailer under the
// Kiln root, and writes config.json with mode 0600.
func cmdInit(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("init takes no arguments")
	}
	root := os.Getenv("KILN_ROOT")
	if root == "" {
		root = defaultRoot
	}
	binDir := filepath.Join(root, "bin")
	kernelDir := filepath.Join(root, "kernel")
	for _, dir := range []string{root, binDir, kernelDir} {
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

	cfgPath := filepath.Join(root, "config.json")
	if err := writeConfig(cfgPath); err != nil {
		return err
	}

	fmt.Printf("firecracker %s -> %s\n", firecrackerVersion, filepath.Join(binDir, "firecracker"))
	fmt.Printf("jailer %s -> %s\n", firecrackerVersion, filepath.Join(binDir, "jailer"))
	fmt.Printf("kernel %s -> %s\n", kernelVersion, kernelPath)
	fmt.Printf("kilninit -> %s\n", kilninitPath)
	fmt.Printf("config -> %s (mode 0600)\n", cfgPath)
	return nil
}

// installKilninit builds the guest init for linux/amd64. The build needs the
// source tree; a host without it keeps an existing binary.
func installKilninit(dst string) error {
	_, goMod := os.Stat("go.mod")
	_, source := os.Stat(filepath.Join("guest", "kilninit"))
	if goMod == nil && source == nil {
		cmd := exec.Command("go", "build", "-o", dst, "./guest/kilninit")
		cmd.Env = append(withoutEnv(os.Environ(), "GOOS", "GOARCH", "CGO_ENABLED"),
			"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build kilninit: %w: %s", err, out)
		}
		return nil
	}
	if fileExists(dst) {
		return nil
	}
	return fmt.Errorf("kilninit: guest source not found and %s does not exist; run kiln init from the repo", dst)
}

// withoutEnv drops the named variables so the cross-compile settings win.
func withoutEnv(env []string, keys ...string) []string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k+"="] = true
	}
	out := env[:0:0]
	for _, e := range env {
		skip := false
		for prefix := range drop {
			if strings.HasPrefix(e, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
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
// clients. Missing listeners take the fixed v1 addresses.
func writeConfig(path string) error {
	cfg := configFile{}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}
	if cfg.BearerToken == "" {
		token, err := newToken()
		if err != nil {
			return err
		}
		cfg.BearerToken = token
	}
	if cfg.ControlAddr == "" {
		cfg.ControlAddr = "127.0.0.1:8080"
	}
	if cfg.IngressAddr == "" {
		cfg.IngressAddr = "0.0.0.0:443"
	}
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

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
