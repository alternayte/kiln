//go:build kvm

package p4gate

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
	"strconv"
	"strings"
	"sync"
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

// pageFactory hands out one page fault source per restore. The gate can make
// one restore fail to prove a partial fork destroys the copies it made.
type pageFactory struct {
	binary string

	mu     sync.Mutex
	calls  int
	failAt int
}

func (p *pageFactory) make() snapshot.PageFaultSource {
	p.mu.Lock()
	p.calls++
	n := p.calls
	failAt := p.failAt
	p.mu.Unlock()
	if failAt > 0 && n == failAt {
		return &failingPages{}
	}
	return &snapshot.Process{Binary: p.binary}
}

func (p *pageFactory) arm(failAt int) {
	p.mu.Lock()
	p.calls = 0
	p.failAt = failAt
	p.mu.Unlock()
}

type failingPages struct{}

func (f *failingPages) Start(context.Context, string, string) error {
	return errors.New("gate: injected page-source failure")
}

func (f *failingPages) Stop() error { return nil }

// TestGateP4 builds a 512 MB template, forks one sandbox into eight, and
// proves the copies diverge, carry fresh entropy, share memory, do not reach
// each other, and obey the egress allowlist. The gate recipe runs it under
// scripts/leak.sh.
func TestGateP4(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate P4 needs root: start it with sudo")
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
	pages := &pageFactory{binary: kilnBin}
	sbx := sandbox.New(sandbox.Config{
		Root:       root,
		Store:      st,
		Runtime:    rt,
		Network:    nm,
		Pages:      pages.make,
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

	const name = "p4-python"
	clear := func() {
		clearSandboxes(t, context.WithoutCancel(ctx), st, sbx, name)
		clearSnapshots(t, context.WithoutCancel(ctx), st, sbx, root, name)
		_ = tmgr.Delete(context.WithoutCancel(ctx), name)
	}
	clear()
	t.Cleanup(clear)

	t.Run("BuildTemplate", func(t *testing.T) {
		build := map[string]any{
			"name":         name,
			"image":        "docker.io/library/python:3.12-slim",
			"vcpus":        2,
			"memory_mb":    512,
			"disk_mb":      2048,
			"setup":        []string{},
			"egress_allow": []string{"pypi.org", "files.pythonhosted.org"},
		}
		if code := cli.startTemplate(name, build); code != http.StatusAccepted {
			t.Fatalf("create template: status %d, want 202", code)
		}
		v := cli.pollTemplate(name, 8*time.Minute)
		if v.State != store.TemplateReady {
			t.Fatalf("template state %q: %s", v.State, v.Error)
		}
	})

	source := cli.createSandbox(name, map[string]any{
		"template":     name,
		"lifecycle":    store.LifecycleEphemeral,
		"idle_seconds": 900,
		"metadata":     map[string]any{"gate": "p4"},
	})
	t.Cleanup(func() { cli.deleteSandboxIfPresent(source) })
	cli.putFile(source, "work/marker", "abc")
	if out := cli.exec(source, "cat", "/work/marker"); out.Stdout != "abc" {
		t.Fatalf("source marker: stdout %q", out.Stdout)
	}

	var forks []string

	// The copies must outlive this subtest: the egress checks drive them.
	outer := t
	t.Run("ForkEight", func(t *testing.T) {
		copies := cli.fork(source, 8, false)
		if len(copies) != 8 {
			t.Fatalf("fork returned %d copies, want 8", len(copies))
		}
		forks = make([]string, 0, len(copies))
		seen := map[string]bool{}
		for i, copy := range copies {
			if copy.State != store.SandboxRunning {
				t.Fatalf("copy %d state %q, want running", i, copy.State)
			}
			if seen[copy.ID] {
				t.Fatalf("copy %d repeats the id %s", i, copy.ID)
			}
			seen[copy.ID] = true
			forks = append(forks, copy.ID)
			id := copy.ID
			outer.Cleanup(func() { cli.deleteSandboxIfPresent(id) })
		}
		if len(cidSet(t, ctx, st, forks)) != 8 {
			t.Fatalf("fork copies do not hold eight distinct vsock cids")
		}

		// Every copy sees the file written before the fork.
		for i, id := range forks {
			if out := cli.exec(id, "cat", "/work/marker"); out.Stdout != "abc" {
				t.Fatalf("copy %d marker: stdout %q, want abc", i, out.Stdout)
			}
		}
		// Every copy writes its own value and reads only its own back.
		for i, id := range forks {
			value := fmt.Sprintf("copy-%d", i)
			if out := cli.exec(id, "sh", "-c", "echo "+value+" > /work/marker"); out.ExitCode != 0 {
				t.Fatalf("copy %d write: exit %d stderr %q", i, out.ExitCode, out.Stderr)
			}
		}
		for i, id := range forks {
			want := fmt.Sprintf("copy-%d\n", i)
			if out := cli.exec(id, "cat", "/work/marker"); out.Stdout != want {
				t.Fatalf("copy %d read %q, want %q: copies are not independent", i, out.Stdout, want)
			}
		}
		// Every copy has its own entropy.
		values := map[string]int{}
		for i, id := range forks {
			out := cli.exec(id, "python", "-c", "import os; print(os.urandom(16).hex())")
			if out.ExitCode != 0 {
				t.Fatalf("copy %d urandom: exit %d stderr %q", i, out.ExitCode, out.Stderr)
			}
			value := strings.TrimSpace(out.Stdout)
			if len(value) != 32 {
				t.Fatalf("copy %d urandom %q, want 32 hex digits", i, value)
			}
			values[value]++
		}
		if len(values) != 8 {
			t.Fatalf("eight copies produced %d distinct random values", len(values))
		}

		// Eight forks of one 512 MB template must not cost eight memories.
		var pssKiB int64
		for _, id := range forks {
			row, err := st.GetSandbox(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			pssKiB += processPSSKiB(t, row.PID)
		}
		if pssKiB == 0 {
			t.Fatal("no firecracker memory was measured")
		}
		if pssKiB > 2*1024*1024 {
			t.Fatalf("eight forks use %d MiB of memory, want under 2048 MiB", pssKiB/1024)
		}
		t.Logf("eight forks use %d MiB", pssKiB/1024)
	})

	t.Run("Egress", func(t *testing.T) {
		if len(forks) < 2 {
			t.Fatal("ForkEight did not leave copies to test")
		}
		a, b := forks[0], forks[1]

		// The allowlist lets an allowed name out.
		if out := cli.exec(a, "python", "-c",
			"import socket; socket.create_connection(('pypi.org', 443), timeout=15).close(); print('ok')"); out.ExitCode != 0 {
			t.Fatalf("allowlisted connection failed: exit %d stderr %q", out.ExitCode, out.Stderr)
		}
		// A name outside the allowlist is refused before it is resolved.
		if out := cli.exec(a, "python", "-c",
			"import socket; socket.create_connection(('example.com', 443), timeout=5)"); out.ExitCode == 0 {
			t.Fatal("a non-allowlisted connection succeeded")
		}
		// The metadata address stays dropped even when it is in the set.
		row, err := st.GetSandbox(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		set := "kiln_" + strings.TrimPrefix(row.TapName, "kiln-") + "_v4"
		if out, err := exec.Command("nft", "add", "element", "inet", "kiln", set, "{", "169.254.169.254", "timeout", "120s", "}").CombinedOutput(); err != nil {
			t.Fatalf("add metadata to the allow set: %v: %s", err, out)
		}
		if out := cli.exec(a, "python", "-c",
			"import socket; socket.create_connection(('169.254.169.254', 80), timeout=5)"); out.ExitCode == 0 {
			t.Fatal("the metadata address is reachable with it in the allow set")
		}

		// B serves a port; A must have no path to it.
		if out := cli.exec(b, "sh", "-c",
			"python -m http.server 9999 --bind 0.0.0.0 >/tmp/http.log 2>&1 &"); out.ExitCode != 0 {
			t.Fatalf("start listener in B: exit %d stderr %q", out.ExitCode, out.Stderr)
		}
		served := false
		for i := 0; i < 20 && !served; i++ {
			out := cli.exec(b, "python", "-c",
				"import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:9999', timeout=2).status)")
			served = out.ExitCode == 0
			if !served {
				time.Sleep(500 * time.Millisecond)
			}
		}
		if !served {
			t.Fatal("B never answered on its own port")
		}
		targets := []string{"172.31.0.1", hostUplinkIP(t)}
		for _, target := range targets {
			code := "import socket; socket.create_connection(('" + target + "', 9999), timeout=3)"
			if out := cli.exec(a, "python", "-c", code); out.ExitCode == 0 {
				t.Fatalf("sandbox A reached B's listener through %s", target)
			}
		}
	})

	t.Run("SnapshotRestore", func(t *testing.T) {
		snap := cli.snapshot(source, false)
		if snap.SnapshotID == "" || snap.SizeBytes <= 0 {
			t.Fatalf("snapshot %+v, want an id and a size", snap)
		}
		if !containsSnapshot(cli.listSnapshots(), snap.SnapshotID) {
			t.Fatalf("snapshot %s is not listed", snap.SnapshotID)
		}
		copies := cli.restore(snap.SnapshotID, 1, false)
		if len(copies) != 1 {
			t.Fatalf("restore returned %d copies, want 1", len(copies))
		}
		child := copies[0].ID
		t.Cleanup(func() { cli.deleteSandboxIfPresent(child) })
		if out := cli.exec(child, "cat", "/work/marker"); out.Stdout != "abc" {
			t.Fatalf("restored child marker: stdout %q, want abc", out.Stdout)
		}
		if code := cli.deleteSnapshot(snap.SnapshotID); code != http.StatusConflict {
			t.Fatalf("delete with a live child: status %d, want 409", code)
		}
		if code := cli.deleteSandbox(child); code != http.StatusNoContent {
			t.Fatalf("delete child: status %d", code)
		}
		if code := cli.deleteSnapshot(snap.SnapshotID); code != http.StatusNoContent {
			t.Fatalf("delete snapshot: status %d, want 204", code)
		}
		if resp, _ := cli.request(http.MethodGet, "/v1/snapshots/"+snap.SnapshotID, ""); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("get deleted snapshot: status %d, want 404", resp.StatusCode)
		}
	})

	t.Run("SecretFork", func(t *testing.T) {
		id := cli.createSandbox(name, map[string]any{
			"template":     name,
			"lifecycle":    store.LifecycleEphemeral,
			"idle_seconds": 900,
			"secrets":      []string{"TEST_SECRET"},
		})
		t.Cleanup(func() { cli.deleteSandboxIfPresent(id) })
		if out := cli.exec(id, "sh", "-c", "echo $TEST_SECRET"); out.Stdout != "s3cr3t\n" {
			t.Fatalf("secret not injected: stdout %q", out.Stdout)
		}
		if code, body := cli.forkRaw(id, 1, false); code != http.StatusConflict {
			t.Fatalf("secret fork without allow: status %d, want 409: %s", code, body)
		}
		copies := cli.fork(id, 1, true)
		child := copies[0].ID
		t.Cleanup(func() { cli.deleteSandboxIfPresent(child) })
		if out := cli.exec(child, "sh", "-c", "echo $TEST_SECRET"); out.Stdout != "s3cr3t\n" {
			t.Fatalf("fork lost the secret: stdout %q", out.Stdout)
		}

		// A snapshot of a secret-bearing sandbox is allowed and marked.
		snap := cli.snapshot(id, false)
		marked := false
		for _, item := range cli.listSnapshots() {
			if item.ID == snap.SnapshotID {
				marked = item.Secret
			}
		}
		if !marked {
			t.Fatalf("snapshot %s is not marked secret-bearing", snap.SnapshotID)
		}
		if code, body := cli.restoreRaw(snap.SnapshotID, 1, false); code != http.StatusConflict {
			t.Fatalf("secret restore without allow: status %d, want 409: %s", code, body)
		}
		restored := cli.restore(snap.SnapshotID, 1, true)
		t.Cleanup(func() { cli.deleteSandboxIfPresent(restored[0].ID) })
		if out := cli.exec(restored[0].ID, "sh", "-c", "echo $TEST_SECRET"); out.Stdout != "s3cr3t\n" {
			t.Fatalf("restore lost the secret: stdout %q", out.Stdout)
		}
	})

	t.Run("PartialFailure", func(t *testing.T) {
		before := liveSandboxes(t, ctx, st, name)
		pages.arm(5)
		code, body := cli.forkRaw(source, 8, false)
		pages.arm(0)
		if code != http.StatusInternalServerError {
			t.Fatalf("fork with a failed copy: status %d, want 500: %s", code, body)
		}
		after := liveSandboxes(t, ctx, st, name)
		if !sameSet(before, after) {
			t.Fatalf("live sandboxes after the failed fork: %v, want %v", after, before)
		}
		if out := cli.exec(source, "sh", "-c", "echo alive"); out.Stdout != "alive\n" {
			t.Fatalf("the source did not survive: %q", out.Stdout)
		}
	})

	t.Run("Sleep", func(t *testing.T) {
		id := cli.createSandbox(name, map[string]any{
			"template":     name,
			"lifecycle":    store.LifecyclePersistent,
			"idle_seconds": 900,
		})
		t.Cleanup(func() { cli.deleteSandboxIfPresent(id) })
		cli.putFile(id, "work/sleep-marker", "kept")
		snap := cli.snapshot(id, true)
		if snap.SnapshotID == "" {
			t.Fatalf("sleep snapshot %+v, want an id", snap)
		}
		if got := cli.getSandbox(id); got.State != store.SandboxSleeping {
			t.Fatalf("state %q after stop:true, want sleeping", got.State)
		}
		if _, err := os.Stat(filepath.Join(root, "sandboxes", id, "sleep", "mem")); err != nil {
			t.Fatalf("the sandbox's own sleep image is missing: %v", err)
		}
	})

	t.Run("NoSurvivors", func(t *testing.T) {
		clearSandboxes(t, ctx, st, sbx, name)
		clearSnapshots(t, ctx, st, sbx, root, name)
		if out, _ := exec.Command("pgrep", "-f", "kiln snapfault").Output(); len(bytes.TrimSpace(out)) != 0 {
			t.Fatalf("snapfault processes survived: %s", out)
		}
	})
}

// cidSet returns the set of vsock cids the rows hold. A missing cid is
// reported as the empty string, so a fork that skipped the pool fails.
func cidSet(t *testing.T, ctx context.Context, st store.Store, ids []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, id := range ids {
		row, err := st.GetSandbox(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.VsockCID == nil || *row.VsockCID < store.MinVsockCID {
			t.Fatalf("sandbox %s has cid %v", id, row.VsockCID)
		}
		out[strconv.Itoa(*row.VsockCID)] = true
	}
	return out
}

// liveSandboxes returns the ids of the sandboxes that still hold host
// resources for one template.
func liveSandboxes(t *testing.T, ctx context.Context, st store.Store, name string) []string {
	t.Helper()
	rows, err := st.ListSandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, row := range rows {
		if row.TemplateName == name && row.DestroyedAt == nil {
			out = append(out, row.ID)
		}
	}
	return out
}

// sameSet reports whether two id lists hold the same ids.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			return false
		}
	}
	return true
}

// processPSSKiB sums the proportional set size of one process. PSS counts a
// shared page once, so it is the honest measure of what the copies cost.
func processPSSKiB(t *testing.T, pid int) int64 {
	t.Helper()
	if pid <= 0 {
		t.Fatalf("sandbox has no pid")
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		t.Fatalf("smaps_rollup %d: %v", pid, err)
	}
	var total int64
	for _, line := range strings.Split(string(b), "\n") {
		value, ok := strings.CutPrefix(line, "Pss:")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			t.Fatalf("smaps_rollup Pss %q: %v", value, err)
		}
		total += n
	}
	return total
}

func hostUplinkIP(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("hostname", "-I").Output()
	if err != nil {
		t.Fatalf("hostname -I: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatal("hostname -I returned no address")
	}
	return fields[0]
}

// clearSandboxes destroys every live sandbox of one template and every
// snapshot row it left behind. This phase has no reconciler.
func clearSandboxes(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager, name string) {
	rows, err := st.ListSandboxes(ctx)
	if err != nil {
		t.Logf("cleanup: list sandboxes: %v", err)
		return
	}
	for _, row := range rows {
		if row.TemplateName == name && row.DestroyedAt == nil {
			if err := sbx.Destroy(ctx, row.ID); err != nil {
				t.Logf("cleanup: destroy %s: %v", row.ID, err)
			}
		}
	}
}

// clearSnapshots removes every snapshot row and directory of one template,
// including the unlisted images the manager hides.
func clearSnapshots(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager, root, name string) {
	entries, err := os.ReadDir(filepath.Join(root, "snapshots"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Logf("cleanup: read snapshots: %v", err)
		return
	}
	for _, entry := range entries {
		dir := filepath.Join(root, "snapshots", entry.Name())
		snap, err := st.GetSnapshot(ctx, entry.Name())
		switch {
		case errors.Is(err, store.ErrNotFound):
			_ = os.RemoveAll(dir)
		case err != nil:
			t.Logf("cleanup: get snapshot %s: %v", entry.Name(), err)
		case snap.TemplateName == name:
			if derr := sbx.DeleteSnapshot(ctx, snap.ID); derr != nil {
				// An unlisted image is not visible to the manager.
				if derr2 := st.DeleteSnapshot(ctx, snap.ID); derr2 != nil {
					t.Logf("cleanup: delete snapshot %s: %v", snap.ID, derr2)
				}
			}
			_ = os.RemoveAll(dir)
		}
	}
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

type snapshotCreated struct {
	SnapshotID string `json:"snapshot_id"`
	SizeBytes  int64  `json:"size_bytes"`
}

type snapshotListItem struct {
	ID       string `json:"id"`
	Bytes    int64  `json:"size_bytes"`
	Secret   bool   `json:"secret_bearing"`
	Template string `json:"template"`
}

type copyList struct {
	Sandboxes []sandboxView `json:"sandboxes"`
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
	return v.ID
}

func (c *client) getSandbox(id string) sandboxView {
	c.t.Helper()
	resp, out := c.request(http.MethodGet, "/v1/sandboxes/"+id, "")
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("get sandbox %s: status %d: %s", id, resp.StatusCode, out)
	}
	var v sandboxView
	if err := json.Unmarshal(out, &v); err != nil {
		c.t.Fatalf("get sandbox %s: %v: %s", id, err, out)
	}
	return v
}

func (c *client) deleteSandbox(id string) int {
	c.t.Helper()
	resp, _ := c.request(http.MethodDelete, "/v1/sandboxes/"+id, "")
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		c.t.Fatalf("delete sandbox %s: status %d", id, resp.StatusCode)
	}
	return resp.StatusCode
}

func (c *client) deleteSandboxIfPresent(id string) {
	resp, out := c.request(http.MethodDelete, "/v1/sandboxes/"+id, "")
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		c.t.Errorf("cleanup delete sandbox %s: status %d: %s", id, resp.StatusCode, out)
	}
}

func (c *client) exec(id string, argv ...string) execView {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"cmd": argv, "timeout_seconds": 120})
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

func (c *client) snapshot(id string, stop bool) snapshotCreated {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"stop": stop})
	if err != nil {
		c.t.Fatal(err)
	}
	resp, out := c.request(http.MethodPost, "/v1/sandboxes/"+id+"/snapshot", string(body))
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("snapshot %s: status %d: %s", id, resp.StatusCode, out)
	}
	var v snapshotCreated
	if err := json.Unmarshal(out, &v); err != nil {
		c.t.Fatalf("snapshot %s: %v: %s", id, err, out)
	}
	return v
}

func (c *client) fork(id string, count int, allow bool) []sandboxView {
	c.t.Helper()
	code, out := c.forkRaw(id, count, allow)
	if code != http.StatusCreated {
		c.t.Fatalf("fork %s: status %d: %s", id, code, out)
	}
	var v copyList
	if err := json.Unmarshal(out, &v); err != nil {
		c.t.Fatalf("fork %s: %v: %s", id, err, out)
	}
	return v.Sandboxes
}

func (c *client) forkRaw(id string, count int, allow bool) (int, []byte) {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"count": count, "allow_secret_fork": allow})
	if err != nil {
		c.t.Fatal(err)
	}
	resp, out := c.request(http.MethodPost, "/v1/sandboxes/"+id+"/fork", string(body))
	return resp.StatusCode, out
}

func (c *client) restore(snapshotID string, count int, allow bool) []sandboxView {
	c.t.Helper()
	code, out := c.restoreRaw(snapshotID, count, allow)
	if code != http.StatusCreated {
		c.t.Fatalf("restore %s: status %d: %s", snapshotID, code, out)
	}
	var v copyList
	if err := json.Unmarshal(out, &v); err != nil {
		c.t.Fatalf("restore %s: %v: %s", snapshotID, err, out)
	}
	return v.Sandboxes
}

func (c *client) restoreRaw(snapshotID string, count int, allow bool) (int, []byte) {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"count": count, "allow_secret_fork": allow})
	if err != nil {
		c.t.Fatal(err)
	}
	resp, out := c.request(http.MethodPost, "/v1/snapshots/"+snapshotID+"/restore", string(body))
	return resp.StatusCode, out
}

func (c *client) listSnapshots() []snapshotListItem {
	c.t.Helper()
	resp, out := c.request(http.MethodGet, "/v1/snapshots", "")
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("list snapshots: status %d: %s", resp.StatusCode, out)
	}
	var v []snapshotListItem
	if err := json.Unmarshal(out, &v); err != nil {
		c.t.Fatalf("list snapshots: %v: %s", err, out)
	}
	return v
}

func (c *client) deleteSnapshot(id string) int {
	c.t.Helper()
	resp, _ := c.request(http.MethodDelete, "/v1/snapshots/"+id, "")
	return resp.StatusCode
}

func containsSnapshot(list []snapshotListItem, id string) bool {
	for _, item := range list {
		if item.ID == id {
			return true
		}
	}
	return false
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
