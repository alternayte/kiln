//go:build kvm

package p2gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/api"
	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

// TestGateP2 imports an image, runs setup in a build VM, stores the snapshot,
// serves the template API and proves default-deny egress. The gate recipe runs
// it under scripts/leak.sh.
func TestGateP2(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate P2 needs root: start it with sudo")
	}
	repo := repoRoot(t)
	runPreflight(t, repo)
	root := kilnRoot()
	kernel := kernelPath(t, repo, root)
	kilninit := filepath.Join(root, "bin", "kilninit")
	if _, err := os.Stat(kilninit); err != nil {
		t.Fatalf("kilninit %s: %v (run kiln init)", kilninit, err)
	}
	cfg := readConfig(t, root)
	if cfg.BearerToken == "" {
		t.Fatalf("%s has no bearer_token (run kiln init)", filepath.Join(root, "config.json"))
	}

	st, err := store.Open(filepath.Join(root, "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rt, err := runtime.New(runtime.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	nm := network.New()
	if err := nm.EnsureBase(ctx); err != nil {
		t.Fatal(err)
	}
	mgr := &template.Manager{
		Root:       root,
		Store:      st,
		Runtime:    rt,
		Network:    nm,
		Builder:    template.Builder{KilninitPath: kilninit},
		KernelPath: kernel,
	}
	srv := httptest.NewServer((&api.Server{Store: st, Templates: mgr, Token: cfg.BearerToken, Base: ctx}).Handler())
	t.Cleanup(srv.Close)
	cli := &client{t: t, base: srv.URL, token: cfg.BearerToken}

	// An aborted earlier run can leave a row and files behind. This phase has
	// no reconciler yet, so the gate clears its own names first.
	for _, name := range []string{"py312", "badbuild"} {
		if _, err := st.GetTemplate(ctx, name); err == nil {
			if rerr := os.RemoveAll(mgr.TemplateDir(store.DefaultTenant, name)); rerr != nil {
				t.Fatal(rerr)
			}
			if derr := st.DeleteTemplate(ctx, name); derr != nil {
				t.Fatal(derr)
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	}

	t.Run("BuildAndExec", func(t *testing.T) {
		testBuildAndExec(t, ctx, cli, mgr, rt, nm, kernel)
	})
	t.Run("Whiteout", func(t *testing.T) {
		testWhiteout(t, ctx, kilninit)
	})
	t.Run("DeterministicImport", func(t *testing.T) {
		testDeterministicImport(t, ctx, kilninit)
	})
	t.Run("FailedBuild", func(t *testing.T) {
		testFailedBuild(t, cli, mgr)
	})
	t.Run("Auth", func(t *testing.T) {
		resp, _ := cli.request(http.MethodGet, "/v1/templates", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no token: status %d, want 401", resp.StatusCode)
		}
		resp, _ = cli.request(http.MethodGet, "/v1/templates", "wrong", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong token: status %d, want 401", resp.StatusCode)
		}
	})
}

func testBuildAndExec(t *testing.T, ctx context.Context, cli *client, mgr *template.Manager, rt *runtime.Firecracker, nm *network.Manager, kernel string) {
	const name = "py312"
	req := map[string]any{
		"name":         name,
		"image":        "docker.io/library/python:3.12-slim",
		"vcpus":        2,
		"memory_mb":    512,
		"disk_mb":      4096,
		"setup":        []string{"pip install --no-cache-dir requests", "echo setup-ran > /setup-ran"},
		"egress_allow": []string{"pypi.org", "files.pythonhosted.org"},
	}
	t.Cleanup(func() { cli.deleteIfPresent(name) })

	if code := cli.start(name, req); code != http.StatusAccepted {
		t.Fatalf("create: status %d, want 202", code)
	}
	if v := cli.get(name); v.State != store.TemplateBuilding {
		t.Fatalf("state %q immediately after create, want building", v.State)
	}
	if code := cli.start(name, req); code != http.StatusConflict {
		t.Fatalf("duplicate create: status %d, want 409", code)
	}
	if code := cli.delete(name); code != http.StatusConflict {
		t.Fatalf("delete during build: status %d, want 409", code)
	}

	v := cli.poll(name, 10*time.Minute)
	if v.State != store.TemplateReady {
		t.Fatalf("state %q, want ready: %s", v.State, v.Error)
	}
	if v.ImageDigest == "" || !strings.HasPrefix(v.ImageDigest, "sha256:") {
		t.Fatalf("image_digest %q", v.ImageDigest)
	}
	if v.SnapshotBytes <= 0 {
		t.Fatalf("snapshot_bytes %d, want more than 0", v.SnapshotBytes)
	}
	if v.VCPUs != 2 || v.MemoryMB != 512 || v.DiskMB != 4096 || v.Sandboxes != 0 {
		t.Fatalf("resources %+v", v)
	}
	if !reflect.DeepEqual(v.EgressAllow, []string{"pypi.org", "files.pythonhosted.org"}) {
		t.Fatalf("egress_allow %v", v.EgressAllow)
	}

	dir := mgr.TemplateDir(store.DefaultTenant, name)
	for _, f := range []string{"rootfs.ext4", "mem", "state", "manifest.json"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if fi.Size() == 0 {
			t.Fatalf("%s is empty", f)
		}
	}
	manifest, err := template.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ImageDigest != v.ImageDigest || manifest.VCPUs != 2 || manifest.MemoryMB != 512 || manifest.DiskMB != 4096 {
		t.Fatalf("manifest %+v", manifest)
	}
	if !reflect.DeepEqual(manifest.EgressAllow, v.EgressAllow) {
		t.Fatalf("manifest egress %v", manifest.EgressAllow)
	}
	if _, err := debugfs(t, filepath.Join(dir, "rootfs.ext4"), "stat /setup-ran"); err != nil {
		t.Fatalf("setup did not run in the build VM: %v", err)
	}

	rootfs := filepath.Join(dir, "rootfs.ext4")
	att, err := nm.Attach(ctx, "p2-exec", []string{"pypi.org"})
	if err != nil {
		t.Fatal(err)
	}
	vm, err := rt.Start(ctx, runtime.Spec{
		ID:             "p2-exec",
		KernelPath:     kernel,
		RootfsPath:     rootfs,
		RootfsReadOnly: true,
		VCPUs:          2,
		MemoryMiB:      512,
		VsockCID:       3,
		VsockPort:      guestproto.Port,
		TAPName:        att.TAPName,
	})
	if err != nil {
		_ = att.Detach(context.WithoutCancel(ctx))
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := rt.Stop(stopCtx, vm); err != nil {
			t.Errorf("stop: %v", err)
		}
		if err := att.Detach(stopCtx); err != nil {
			t.Errorf("detach: %v", err)
		}
	})

	res := execIn(t, vm, "python", "-c", "print(2+2)")
	if res.Stdout != "4\n" {
		t.Fatalf("python stdout %q, want %q", res.Stdout, "4\n")
	}
	res = execIn(t, vm, "python", "-c", "import requests; print(requests.__version__)")
	if res.ExitCode != 0 {
		t.Fatalf("requests import: exit %d: %s", res.ExitCode, res.Stderr)
	}
	res = execIn(t, vm, "python", "-c", "import socket; socket.create_connection(('pypi.org', 443), timeout=10).close()")
	if res.ExitCode != 0 {
		t.Fatalf("allowlisted connect failed: exit %d: %s", res.ExitCode, res.Stderr)
	}
	res = execIn(t, vm, "python", "-c", "import socket; socket.create_connection(('example.com', 443), timeout=10)")
	if res.ExitCode == 0 {
		t.Fatal("a non-allowlisted connect succeeded")
	}
}

func testWhiteout(t *testing.T, ctx context.Context, kilninit string) {
	dir := t.TempDir()
	writeOCILayout(t, dir, "whtest", [][]layerEntry{
		{{Name: "keep.txt", Data: "keep"}, {Name: "gone.txt", Data: "gone"}},
		{{Name: ".wh.gone.txt"}},
	})
	out := filepath.Join(t.TempDir(), "rootfs.ext4")
	b := template.Builder{KilninitPath: kilninit}
	if _, err := b.Import(ctx, template.ImportSpec{
		Ref:        "ocidir://" + dir + ":whtest",
		RootfsPath: out,
		SizeMB:     64,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := debugfs(t, out, "stat /keep.txt"); err != nil {
		t.Fatalf("keep.txt is missing: %v", err)
	}
	if _, err := debugfs(t, out, "stat /gone.txt"); err == nil {
		t.Fatal("gone.txt survived the upper layer's whiteout")
	}
}

func testDeterministicImport(t *testing.T, ctx context.Context, kilninit string) {
	b := template.Builder{KilninitPath: kilninit}
	pathA := filepath.Join(t.TempDir(), "rootfs.ext4")
	pathB := filepath.Join(t.TempDir(), "rootfs.ext4")
	first, err := b.Import(ctx, template.ImportSpec{
		Ref:        "docker.io/library/busybox:latest",
		RootfsPath: pathA,
		SizeMB:     128,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Import(ctx, template.ImportSpec{
		Ref:        "docker.io/library/busybox:latest",
		RootfsPath: pathB,
		SizeMB:     128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digests differ: %s and %s", first.Digest, second.Digest)
	}
	listA := debugfsList(t, pathA)
	listB := debugfsList(t, pathB)
	if !reflect.DeepEqual(listA, listB) {
		t.Fatalf("file listings differ: %s", firstDifference(listA, listB))
	}
}

func testFailedBuild(t *testing.T, cli *client, mgr *template.Manager) {
	const name = "badbuild"
	base := map[string]any{
		"name":         name,
		"image":        "docker.io/library/busybox:latest",
		"vcpus":        1,
		"memory_mb":    128,
		"disk_mb":      128,
		"egress_allow": []string{},
	}
	t.Cleanup(func() { cli.deleteIfPresent(name) })

	bad := copyRequest(base)
	bad["setup"] = []string{"echo boom >&2; exit 3"}
	if code := cli.start(name, bad); code != http.StatusAccepted {
		t.Fatalf("create: status %d, want 202", code)
	}
	v := cli.poll(name, 5*time.Minute)
	if v.State != store.TemplateFailed {
		t.Fatalf("state %q, want failed", v.State)
	}
	if !strings.Contains(v.Error, "boom") {
		t.Fatalf("error %q does not carry the setup output", v.Error)
	}
	if _, err := os.Stat(mgr.TemplateDir(store.DefaultTenant, name)); !os.IsNotExist(err) {
		t.Fatalf("failed build left its directory behind: %v", err)
	}

	good := copyRequest(base)
	good["setup"] = []string{"true"}
	if code := cli.start(name, good); code != http.StatusAccepted {
		t.Fatalf("rebuild of a failed template: status %d, want 202", code)
	}
	v = cli.poll(name, 5*time.Minute)
	if v.State != store.TemplateReady {
		t.Fatalf("rebuilt state %q, want ready: %s", v.State, v.Error)
	}
	if code := cli.delete(name); code != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", code)
	}
	if resp, _ := cli.request(http.MethodGet, "/v1/templates/"+name, cli.token, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, want 404", resp.StatusCode)
	}
}

// client is a small API caller for the gate.
type client struct {
	t     *testing.T
	base  string
	token string
}

type templateView struct {
	Name          string   `json:"name"`
	Image         string   `json:"image"`
	ImageDigest   string   `json:"image_digest"`
	VCPUs         int      `json:"vcpus"`
	MemoryMB      int      `json:"memory_mb"`
	DiskMB        int      `json:"disk_mb"`
	EgressAllow   []string `json:"egress_allow"`
	State         string   `json:"state"`
	Error         string   `json:"error"`
	SnapshotBytes int64    `json:"snapshot_bytes"`
	Sandboxes     int      `json:"sandboxes"`
}

func (c *client) request(method, path, token string, body any) (*http.Response, []byte) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp, b
}

func (c *client) start(name string, body map[string]any) int {
	c.t.Helper()
	resp, out := c.request(http.MethodPost, "/v1/templates", c.token, body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusConflict {
		c.t.Fatalf("create %s: status %d: %s", name, resp.StatusCode, out)
	}
	return resp.StatusCode
}

func (c *client) get(name string) templateView {
	c.t.Helper()
	resp, out := c.request(http.MethodGet, "/v1/templates/"+name, c.token, nil)
	switch resp.StatusCode {
	case http.StatusNotFound:
		return templateView{State: "absent"}
	case http.StatusOK:
	default:
		c.t.Fatalf("get %s: status %d: %s", name, resp.StatusCode, out)
	}
	var v templateView
	if err := json.Unmarshal(out, &v); err != nil {
		c.t.Fatalf("get %s: %v: %s", name, err, out)
	}
	return v
}

// poll waits for a terminal state and returns it.
func (c *client) poll(name string, timeout time.Duration) templateView {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v := c.get(name)
		if v.State == store.TemplateReady || v.State == store.TemplateFailed {
			return v
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("template %s stuck in %q", name, v.State)
		}
		time.Sleep(time.Second)
	}
}

func (c *client) delete(name string) int {
	c.t.Helper()
	resp, out := c.request(http.MethodDelete, "/v1/templates/"+name, c.token, nil)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusNotFound {
		c.t.Fatalf("delete %s: status %d: %s", name, resp.StatusCode, out)
	}
	return resp.StatusCode
}

func (c *client) deleteIfPresent(name string) {
	// A failed subtest may end while its build is still running. Let the
	// build reach a terminal state first, so nothing is left behind.
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		switch c.get(name).State {
		case "absent", store.TemplateReady, store.TemplateFailed:
			resp, out := c.request(http.MethodDelete, "/v1/templates/"+name, c.token, nil)
			if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
				c.t.Errorf("cleanup delete %s: status %d: %s", name, resp.StatusCode, out)
			}
			return
		}
		time.Sleep(time.Second)
	}
	c.t.Errorf("cleanup delete %s: the build did not finish", name)
}

func copyRequest(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func execIn(t *testing.T, vm *runtime.VM, argv ...string) runtime.ExecResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := vm.Exec(ctx, runtime.ExecRequest{Cmd: argv, TimeoutSeconds: 120})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	return res
}

// debugfs inspects an image that a VM may have stopped without a clean
// unmount, so it replays the journal first.
func debugfs(t *testing.T, image, command string) (string, error) {
	t.Helper()
	check := exec.Command("e2fsck", "-p", image)
	if out, err := check.CombinedOutput(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() > 1 || exit.ExitCode() < 0 {
			return string(out), fmt.Errorf("e2fsck %s: %w: %s", image, err, strings.TrimSpace(string(out)))
		}
	}
	out, err := exec.Command("debugfs", "-R", command, image).CombinedOutput()
	text := string(out)
	if err != nil {
		return text, fmt.Errorf("debugfs %s: %w: %s", command, err, strings.TrimSpace(text))
	}
	// debugfs exits 0 when a lookup finds nothing.
	if strings.Contains(text, "File not found by ext2_lookup") {
		return text, fmt.Errorf("debugfs %s: no such file", command)
	}
	return text, nil
}

// debugfsList dumps the rootfs and returns its file listing, one line per
// entry with its kind and size.
func debugfsList(t *testing.T, image string) []string {
	t.Helper()
	out := t.TempDir()
	if _, err := debugfs(t, image, "rdump / "+out); err != nil {
		t.Fatalf("rdump: %v", err)
	}
	var files []string
	err := filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(out, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			files = append(files, fmt.Sprintf("%s symlink %s", rel, target))
		case info.IsDir():
			files = append(files, rel+"/")
		default:
			files = append(files, fmt.Sprintf("%s file %d", rel, info.Size()))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func firstDifference(a, b []string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("at %d: %q and %q", i, a[i], b[i])
		}
	}
	if len(a) != len(b) {
		return fmt.Sprintf("lengths %d and %d", len(a), len(b))
	}
	return "no difference"
}

func runPreflight(t *testing.T, repo string) {
	t.Helper()
	cmd := exec.Command("bash", "scripts/preflight.sh")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preflight failed: %v\n%s", err, out)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func kilnRoot() string {
	if root := os.Getenv("KILN_ROOT"); root != "" {
		return root
	}
	return runtime.ConfigRoot
}

func readConfig(t *testing.T, root string) struct {
	BearerToken string `json:"bearer_token"`
} {
	t.Helper()
	var cfg struct {
		BearerToken string `json:"bearer_token"`
	}
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatalf("config.json: %v (run kiln init)", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	return cfg
}

// kernelPath returns the pinned kernel under the Kiln root.
func kernelPath(t *testing.T, repo, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, "scripts", "versions.env"))
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "KERNEL_VERSION="); ok {
			version = strings.TrimSpace(v)
		}
	}
	if version == "" {
		t.Fatal("scripts/versions.env does not set KERNEL_VERSION")
	}
	path := filepath.Join(root, "kernel", "vmlinux-"+version)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("kernel %s: %v (run kiln init)", path, err)
	}
	return path
}
