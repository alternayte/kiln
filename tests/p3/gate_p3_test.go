//go:build kvm

package p3gate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/api"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

// gateTimeout bounds the whole gate. The gate recipe also passes a go test
// timeout.
const gateTimeout = 18 * time.Minute

// TestGateP3 builds a template, restores sandboxes from it, execs commands,
// reads the clock and hostname, and runs 20 create and destroy cycles. The
// gate recipe runs it under scripts/leak.sh.
func TestGateP3(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate P3 needs root: start it with sudo")
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
	kilnBin := buildKiln(t, repo)

	st, err := store.Open(filepath.Join(root, "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rt, err := runtime.New(runtime.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	nm := network.New()
	if err := nm.EnsureBase(ctx); err != nil {
		t.Fatal(err)
	}
	tmgr := &template.Manager{
		Root:       root,
		Store:      st,
		Runtime:    rt,
		Network:    nm,
		Builder:    template.Builder{KilninitPath: kilninit},
		KernelPath: kernel,
	}
	sbx := sandbox.New(sandbox.Config{
		Root:       root,
		Store:      st,
		Runtime:    rt,
		Network:    nm,
		Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
		KernelPath: kernel,
		Secrets:    map[string]string{"TEST_SECRET": "s3cr3t"},
	})
	srv := httptest.NewServer((&api.Server{
		Store:     st,
		Templates: tmgr,
		Sandboxes: sbx,
		Token:     cfg.BearerToken,
		Base:      ctx,
	}).Handler())
	t.Cleanup(srv.Close)
	cli := &client{t: t, base: srv.URL, token: cfg.BearerToken}

	const name = "p3-busybox"
	t.Cleanup(func() {
		// Remove every sandbox this gate made, then the template.
		rows, err := st.ListSandboxes(context.WithoutCancel(ctx))
		if err == nil {
			for _, sb := range rows {
				if sb.TemplateName == name && sb.DestroyedAt == nil {
					_ = sbx.Destroy(context.WithoutCancel(ctx), sb.ID)
				}
			}
		}
		cli.deleteTemplateIfPresent(name)
	})
	// A previous aborted run may have left rows and files behind. This phase
	// has no reconciler, so the gate clears its own template first.
	if rows, err := st.ListSandboxes(ctx); err == nil {
		for _, sb := range rows {
			if sb.TemplateName == name && sb.DestroyedAt == nil {
				_ = sbx.Destroy(ctx, sb.ID)
			}
		}
	}
	_ = tmgr.Delete(ctx, name)

	t.Run("BuildTemplate", func(t *testing.T) {
		build := map[string]any{
			"name":         name,
			"image":        "docker.io/library/busybox:latest",
			"vcpus":        1,
			"memory_mb":    128,
			"disk_mb":      256,
			"setup":        []string{},
			"egress_allow": []string{},
		}
		if code := cli.startTemplate(name, build); code != http.StatusAccepted {
			t.Fatalf("create template: status %d, want 202", code)
		}
		v := cli.pollTemplate(name, 8*time.Minute)
		if v.State != store.TemplateReady {
			t.Fatalf("template state %q: %s", v.State, v.Error)
		}
	})

	t.Run("Sandbox", func(t *testing.T) {
		id := cli.createSandbox(name, map[string]any{
			"template":     name,
			"lifecycle":    store.LifecycleEphemeral,
			"idle_seconds": 300,
			"metadata":     map[string]any{"gate": "p3"},
			"secrets":      []string{"TEST_SECRET"},
		})
		t.Cleanup(func() { cli.deleteSandboxIfPresent(id) })

		if out := cli.exec(id, "sh", "-c", "echo hello"); out.Stdout != "hello\n" || out.ExitCode != 0 {
			t.Fatalf("exec hello: exit %d stdout %q stderr %q", out.ExitCode, out.Stdout, out.Stderr)
		}
		if out := cli.exec(id, "sh", "-c", "echo $TEST_SECRET"); out.Stdout != "s3cr3t\n" {
			t.Fatalf("secret not injected: stdout %q", out.Stdout)
		}
		if out := cli.exec(id, "sh", "-c", "grep -c ' / overlay' /proc/mounts"); strings.TrimSpace(out.Stdout) != "1" {
			t.Fatalf("root is not an overlay: %q", out.Stdout)
		}
		guest := guestUnix(t, cli.exec(id, "sh", "-c", "date +%s"))
		if skew := time.Since(time.Unix(guest, 0)); skew > 2*time.Second || skew < -2*time.Second {
			t.Fatalf("guest clock is %s from the host", skew)
		}
		if host := strings.TrimSpace(cli.exec(id, "hostname").Stdout); host != id {
			t.Fatalf("hostname %q, want %q", host, id)
		}

		cli.putFile(id, "work/marker", "abc")
		if got := cli.getFile(id, "work/marker"); got != "abc" {
			t.Fatalf("file read %q, want abc", got)
		}
		if out := cli.exec(id, "cat", "/work/marker"); out.Stdout != "abc" {
			t.Fatalf("cat: stdout %q", out.Stdout)
		}
		if code := cli.deleteSandbox(id); code != http.StatusNoContent {
			t.Fatalf("delete: status %d, want 204", code)
		}
		if code := cli.deleteSandbox(id); code != http.StatusNoContent {
			t.Fatalf("delete again: status %d, want 204", code)
		}
	})

	t.Run("Cycles", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			id := cli.createSandbox(name, map[string]any{
				"template":     name,
				"lifecycle":    store.LifecycleEphemeral,
				"idle_seconds": 300,
			})
			out := cli.exec(id, "sh", "-c", "echo cycle")
			if out.Stdout != "cycle\n" {
				t.Fatalf("cycle %d: stdout %q stderr %q", i, out.Stdout, out.Stderr)
			}
			if code := cli.deleteSandbox(id); code != http.StatusNoContent {
				t.Fatalf("cycle %d: delete status %d", i, code)
			}
		}
		if out, _ := exec.Command("pgrep", "-f", "kiln snapfault").Output(); len(bytes.TrimSpace(out)) != 0 {
			t.Fatalf("snapfault processes survived: %s", out)
		}
	})
}

// client is a small API caller for the gate.
type client struct {
	t     *testing.T
	base  string
	token string
}

type templateView struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Error   string `json:"error"`
	Digest  string `json:"image_digest"`
	Bytes   int64  `json:"snapshot_bytes"`
	Sandbox int    `json:"sandboxes"`
}

type sandboxView struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type execView struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
}

func (c *client) request(method, path, body string) (*http.Response, []byte) {
	c.t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
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

func (c *client) startTemplate(name string, body map[string]any) int {
	c.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, out := c.request(http.MethodPost, "/v1/templates", string(encoded))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusConflict {
		c.t.Fatalf("create template %s: status %d: %s", name, resp.StatusCode, out)
	}
	return resp.StatusCode
}

func (c *client) pollTemplate(name string, timeout time.Duration) templateView {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, out := c.request(http.MethodGet, "/v1/templates/"+name, "")
		if resp.StatusCode != http.StatusOK {
			c.t.Fatalf("get template %s: status %d: %s", name, resp.StatusCode, out)
		}
		var v templateView
		if err := json.Unmarshal(out, &v); err != nil {
			c.t.Fatalf("get template %s: %v: %s", name, err, out)
		}
		if v.State == store.TemplateReady || v.State == store.TemplateFailed {
			return v
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("template %s stuck in %q", name, v.State)
		}
		time.Sleep(time.Second)
	}
}

func (c *client) deleteTemplateIfPresent(name string) {
	resp, _ := c.request(http.MethodDelete, "/v1/templates/"+name, "")
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusConflict {
		c.t.Errorf("cleanup delete template %s: status %d", name, resp.StatusCode)
	}
}

func (c *client) createSandbox(template string, extra map[string]any) string {
	c.t.Helper()
	body := map[string]any{"template": template}
	for k, v := range extra {
		body[k] = v
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, out := c.request(http.MethodPost, "/v1/sandboxes", string(encoded))
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("create sandbox: status %d: %s", resp.StatusCode, out)
	}
	var v sandboxView
	if err := json.Unmarshal(out, &v); err != nil {
		c.t.Fatalf("create sandbox: %v: %s", err, out)
	}
	if v.State != store.SandboxRunning {
		c.t.Fatalf("created sandbox state %q, want running: %s", v.State, out)
	}
	if v.ID == "" {
		c.t.Fatalf("created sandbox has no id: %s", out)
	}
	return v.ID
}

func (c *client) deleteSandbox(id string) int {
	c.t.Helper()
	resp, out := c.request(http.MethodDelete, "/v1/sandboxes/"+id, "")
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		c.t.Fatalf("delete sandbox %s: status %d: %s", id, resp.StatusCode, out)
	}
	return resp.StatusCode
}

func (c *client) deleteSandboxIfPresent(id string) {
	resp, _ := c.request(http.MethodDelete, "/v1/sandboxes/"+id, "")
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		c.t.Errorf("cleanup delete sandbox %s: status %d", id, resp.StatusCode)
	}
}

func (c *client) exec(id string, argv ...string) execView {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"cmd": argv, "timeout_seconds": 60})
	if err != nil {
		c.t.Fatal(err)
	}
	resp, out := c.request(http.MethodPost, "/v1/sandboxes/"+id+"/exec", string(body))
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("exec %v: status %d: %s", argv, resp.StatusCode, out)
	}
	var v execView
	if err := json.Unmarshal(out, &v); err != nil {
		c.t.Fatalf("exec %v: %v: %s", argv, err, out)
	}
	return v
}

func (c *client) putFile(id, path, content string) {
	c.t.Helper()
	resp, out := c.request(http.MethodPut, "/v1/sandboxes/"+id+"/files/"+path, content)
	if resp.StatusCode != http.StatusNoContent {
		c.t.Fatalf("put file %s: status %d: %s", path, resp.StatusCode, out)
	}
}

func (c *client) getFile(id, path string) string {
	c.t.Helper()
	resp, out := c.request(http.MethodGet, "/v1/sandboxes/"+id+"/files/"+path, "")
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("get file %s: status %d: %s", path, resp.StatusCode, out)
	}
	return string(out)
}

func guestUnix(t *testing.T, res execView) int64 {
	t.Helper()
	trimmed := strings.TrimSpace(res.Stdout)
	sec, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		t.Fatalf("guest date %q: %v", trimmed, err)
	}
	return sec
}

func buildKiln(t *testing.T, repo string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "kiln")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/kiln")
	cmd.Dir = repo
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build kiln: %v: %s", err, b)
	}
	return out
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
