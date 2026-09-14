//go:build kvm

package p5gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alternayte/kiln/client"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/store"
)

// gateTimeout bounds the whole gate. The gate recipe also passes a go test
// timeout.
const gateTimeout = 18 * time.Minute

// TestGateP5 drives a real `kiln serve` process: the daemon owns the store,
// the timers and the reconciler. The gate sleeps a persistent sandbox and
// wakes it with its files and memory intact, lets an ephemeral one die at its
// TTL, kills the daemon mid-create and proves the restart converges with no
// orphans. The gate recipe runs it under scripts/leak.sh.
func TestGateP5(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate P5 needs root: start it with sudo")
	}
	repo := repoRoot(t)
	runPreflight(t, repo)
	root := kilnRoot()
	kernelPath(t, repo, root)
	if _, err := os.Stat(filepath.Join(root, "bin", "kilninit")); err != nil {
		t.Fatalf("kilninit: %v (run kiln init)", err)
	}
	cfg := readConfig(t, root)
	kilnBin := buildKiln(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	cli := client.New("http://"+cfg.ControlAddr, cfg.BearerToken)

	var daemon *daemon
	t.Cleanup(func() {
		// The store must go first, while a daemon is alive to destroy it.
		// Start one when the test died with the previous one killed.
		if daemon == nil || daemon.exited() {
			daemon = startServe(t, kilnBin, root)
			waitHealth(t, cli, 90*time.Second)
		}
		clearAll(t, cli, templateName)
		if daemon != nil {
			daemon.stop(t)
		}
	})
	daemon = startServe(t, kilnBin, root)
	waitHealth(t, cli, 90*time.Second)

	st, err := store.Open(filepath.Join(root, "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clearAll(t, cli, templateName)

	t.Run("BuildTemplate", func(t *testing.T) {
		build := client.TemplateRequest{
			Name:        templateName,
			Image:       "docker.io/library/python:3.12-slim",
			VCPUs:       2,
			MemoryMB:    512,
			DiskMB:      2048,
			Setup:       []string{},
			EgressAllow: []string{"pypi.org", "files.pythonhosted.org"},
		}
		err := cli.CreateTemplate(ctx, build)
		if err != nil && !client.Conflict(err) {
			t.Fatalf("create template: %v", err)
		}
		deadline := time.Now().Add(10 * time.Minute)
		for {
			v, err := cli.Template(ctx, templateName)
			if err != nil {
				t.Fatalf("get template: %v", err)
			}
			if v.State == store.TemplateReady {
				break
			}
			if v.State == store.TemplateFailed {
				t.Fatalf("template build failed: %s", v.Error)
			}
			if time.Now().After(deadline) {
				t.Fatalf("template stuck in %q", v.State)
			}
			time.Sleep(time.Second)
		}
	})

	t.Run("IdleSleepAndWake", func(t *testing.T) {
		id := createSandbox(t, cli, persistentSandbox(3))
		t.Cleanup(func() { deleteSandbox(t, cli, id) })
		assertLimits(t, id, 512)
		putFile(t, cli, id, "work/marker", "sleepy")
		// /tmp is a tmpfs: this value lives in the guest's memory only.
		putFile(t, cli, id, "tmp/marker", "in-memory")
		startBackgroundServer(t, cli, id)

		waitState(t, cli, id, store.SandboxSleeping, 90*time.Second)
		if _, err := os.Stat(filepath.Join(root, "sandboxes", id, "sleep", "mem")); err != nil {
			t.Fatalf("sleep image missing: %v", err)
		}
		// A status read must not wake the sandbox.
		time.Sleep(time.Second)
		if got, err := cli.Sandbox(ctx, id); err != nil || got.State != store.SandboxSleeping {
			t.Fatalf("get while sleeping: %+v, %v", got, err)
		}

		if out := execIn(t, cli, id, "cat", "/work/marker"); out.Stdout != "sleepy" {
			t.Fatalf("file after wake: %q", out.Stdout)
		}
		if out := execIn(t, cli, id, "cat", "/tmp/marker"); out.Stdout != "in-memory" {
			t.Fatalf("memory after wake: %q", out.Stdout)
		}
		waitServer(t, cli, id, 30*time.Second)
		if got, err := cli.Sandbox(ctx, id); err != nil || got.State != store.SandboxRunning {
			t.Fatalf("state after wake: %+v, %v", got, err)
		}
		assertEventTrail(t, st, id, [][2]string{
			{store.SandboxCreating, store.SandboxRunning},
			{store.SandboxRunning, store.SandboxSleeping},
			{store.SandboxSleeping, store.SandboxWaking},
			{store.SandboxWaking, store.SandboxRunning},
		})
	})

	t.Run("EphemeralTTL", func(t *testing.T) {
		ttl := 4
		id := createSandbox(t, cli, client.SandboxRequest{
			Template:    templateName,
			Lifecycle:   store.LifecycleEphemeral,
			IdleSeconds: 900,
			TTLSeconds:  &ttl,
		})
		waitState(t, cli, id, store.SandboxDestroyed, 60*time.Second)
		row, err := cli.Sandbox(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.DestroyedAt == nil {
			t.Fatalf("destroyed row has no destroyed_at: %+v", row)
		}
		assertNoHostResources(t, root, id)
	})

	t.Run("RestartAdoption", func(t *testing.T) {
		ids := make([]string, 0, 3)
		for i := 0; i < 3; i++ {
			id := createSandbox(t, cli, client.SandboxRequest{
				Template:    templateName,
				Lifecycle:   store.LifecycleEphemeral,
				IdleSeconds: 900,
				TTLSeconds:  intPtr(900),
			})
			putFile(t, cli, id, fmt.Sprintf("work/marker-%d", i), fmt.Sprintf("value-%d", i))
			ids = append(ids, id)
		}

		// A device no row owns must be swept on the next start.
		if out, err := exec.Command("ip", "tuntap", "add", "dev", "kiln-deadbeef", "mode", "tap").CombinedOutput(); err != nil {
			t.Fatalf("create the stray tap: %v: %s", err, out)
		}

		// One create is in flight when the daemon dies. The kill follows the
		// first sight of the creating row as closely as possible.
		createDone := make(chan error, 1)
		go func() {
			_, err := cli.CreateSandbox(ctx, client.SandboxRequest{
				Template:    templateName,
				Lifecycle:   store.LifecycleEphemeral,
				IdleSeconds: 900,
				TTLSeconds:  intPtr(900),
			})
			createDone <- err
		}()
		midCreate := waitForCreating(t, st, templateName, ids, 60*time.Second)
		daemon.kill(t)
		if err := <-createDone; err == nil {
			t.Fatalf("the create request survived the kill")
		}
		if row, err := st.GetSandbox(context.Background(), midCreate); err != nil || row.State != store.SandboxCreating {
			t.Fatalf("mid-create row after the kill: %+v, %v", row, err)
		}

		daemon = startServe(t, kilnBin, root)
		waitHealth(t, cli, 90*time.Second)

		waitState(t, cli, midCreate, store.SandboxFailed, 60*time.Second)
		assertEventHasReason(t, st, midCreate)
		assertNoHostResources(t, root, midCreate)

		for i, id := range ids {
			waitState(t, cli, id, store.SandboxRunning, 30*time.Second)
			if out := execIn(t, cli, id, "cat", fmt.Sprintf("/work/marker-%d", i)); out.Stdout != fmt.Sprintf("value-%d", i) {
				t.Fatalf("adopted sandbox %d lost its file: %q", i, out.Stdout)
			}
			assertLimits(t, id, 512)
		}
		if _, err := os.Stat("/sys/class/net/kiln-deadbeef"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the stray tap survived the sweep: %v", err)
		}
		if got := countProcesses(t, "firecracker"); got != 3 {
			t.Fatalf("firecracker processes after adoption: %d, want 3", got)
		}
		if got := countProcesses(t, "snapfault"); got != 3 {
			t.Fatalf("page fault sources after adoption: %d, want 3", got)
		}

		for _, id := range ids {
			deleteSandbox(t, cli, id)
			assertNoHostResources(t, root, id)
		}
		if got := countProcesses(t, "firecracker"); got != 0 {
			t.Fatalf("firecracker processes after destroy: %d, want 0", got)
		}
		if got := countProcesses(t, "snapfault"); got != 0 {
			t.Fatalf("page fault sources after destroy: %d, want 0", got)
		}
	})
}

const templateName = "p5-python"

// daemon is one `kiln serve` child process.
type daemon struct {
	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
}

func startServe(t *testing.T, kilnBin, root string) *daemon {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "serve.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(kilnBin, "serve")
	cmd.Env = append(os.Environ(), "KILN_ROOT="+root)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Fatalf("start kiln serve: %v", err)
	}
	d := &daemon{cmd: cmd, log: f, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(d.done)
	}()
	return d
}

func (d *daemon) exited() bool {
	if d == nil {
		return true
	}
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// stop asks the daemon to exit and waits for it.
func (d *daemon) stop(t *testing.T) {
	t.Helper()
	if d.exited() {
		d.log.Close()
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(30 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.done
	}
	d.log.Close()
}

// kill sends SIGKILL. Host resources of the running sandboxes must survive.
func (d *daemon) kill(t *testing.T) {
	t.Helper()
	if err := d.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill kiln serve: %v", err)
	}
	select {
	case <-d.done:
	case <-time.After(30 * time.Second):
		t.Fatal("kiln serve did not die")
	}
	// Read the log before closing it, so a failure reports why it was running.
	if b, err := os.ReadFile(d.log.Name()); err == nil && len(b) > 0 {
		t.Logf("kiln serve log:\n%s", b)
	}
	d.log.Close()
}

func waitHealth(t *testing.T, cli *client.Client, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		health, err := cli.Health(ctx)
		cancel()
		if err == nil && health.OK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("kiln serve did not become healthy: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func clearAll(t *testing.T, cli *client.Client, name string) {
	t.Helper()
	ctx := context.Background()
	rows, err := cli.Sandboxes(ctx)
	if err != nil {
		t.Logf("cleanup: list sandboxes: %v", err)
	}
	for _, row := range rows {
		if row.Template == name {
			if err := cli.DeleteSandbox(ctx, row.ID); err != nil && !client.NotFound(err) {
				t.Logf("cleanup: destroy %s: %v", row.ID, err)
			}
		}
	}
	snaps, err := cli.Snapshots(ctx)
	if err != nil {
		t.Logf("cleanup: list snapshots: %v", err)
	}
	for _, snap := range snaps {
		if snap.Template == name {
			if err := cli.DeleteSnapshot(ctx, snap.ID); err != nil && !client.NotFound(err) {
				t.Logf("cleanup: delete snapshot %s: %v", snap.ID, err)
			}
		}
	}
	if err := cli.DeleteTemplate(ctx, name); err != nil && !client.NotFound(err) && !client.Conflict(err) {
		t.Logf("cleanup: delete template %s: %v", name, err)
	}
}

func persistentSandbox(idle int) client.SandboxRequest {
	return client.SandboxRequest{
		Template:    templateName,
		Lifecycle:   store.LifecyclePersistent,
		IdleSeconds: idle,
	}
}

func intPtr(v int) *int { return &v }

func createSandbox(t *testing.T, cli *client.Client, req client.SandboxRequest) string {
	t.Helper()
	ctx := context.Background()
	sb, err := cli.CreateSandbox(ctx, req)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sb.State != store.SandboxRunning {
		t.Fatalf("created sandbox state %q, want running", sb.State)
	}
	return sb.ID
}

func deleteSandbox(t *testing.T, cli *client.Client, id string) {
	t.Helper()
	if err := cli.DeleteSandbox(context.Background(), id); err != nil && !client.NotFound(err) {
		t.Fatalf("delete sandbox %s: %v", id, err)
	}
}

func putFile(t *testing.T, cli *client.Client, id, path, content string) {
	t.Helper()
	if err := cli.WriteFile(context.Background(), id, path, strings.NewReader(content)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func execIn(t *testing.T, cli *client.Client, id string, argv ...string) client.ExecResult {
	t.Helper()
	out, err := cli.Exec(context.Background(), id, client.ExecRequest{Cmd: argv, TimeoutSeconds: 180})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	return out
}

func waitState(t *testing.T, cli *client.Client, id, state string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		row, err := cli.Sandbox(context.Background(), id)
		if err != nil {
			t.Fatalf("get sandbox %s: %v", id, err)
		}
		if row.State == state {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox %s stuck in %q, want %q", id, row.State, state)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// startBackgroundServer runs an HTTP server inside the guest. Its process
// lives in the sandbox memory, so a wake must find it still serving.
func startBackgroundServer(t *testing.T, cli *client.Client, id string) {
	t.Helper()
	code := "cd /work && python -m http.server 8123 --bind 127.0.0.1 >/tmp/http.log 2>&1 &"
	if out := execIn(t, cli, id, "sh", "-c", code); out.ExitCode != 0 {
		t.Fatalf("start server: exit %d stderr %q", out.ExitCode, out.Stderr)
	}
	waitServer(t, cli, id, 30*time.Second)
}

func waitServer(t *testing.T, cli *client.Client, id string, timeout time.Duration) {
	t.Helper()
	code := "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8123', timeout=2).status)"
	deadline := time.Now().Add(timeout)
	for {
		if out := execIn(t, cli, id, "python", "-c", code); out.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the in-memory server did not answer")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitForCreating returns the id of the sandbox the in-flight create inserted.
func waitForCreating(t *testing.T, st store.Store, name string, known []string, timeout time.Duration) string {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range known {
		seen[id] = true
	}
	deadline := time.Now().Add(timeout)
	for {
		rows, err := st.ListSandboxes(context.Background())
		if err != nil {
			t.Fatalf("list sandboxes: %v", err)
		}
		for _, row := range rows {
			if row.TemplateName == name && row.State == store.SandboxCreating && !seen[row.ID] {
				return row.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no create reached the creating state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertLimits proves the sandbox cgroup carries the template's memory and
// vcpu limits. The memory limit keeps a fixed headroom above the guest size.
func assertLimits(t *testing.T, id string, memoryMB int) {
	t.Helper()
	dir := runtime.CgroupDir(id)
	mem, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		t.Fatalf("sandbox %s memory limit: %v", id, err)
	}
	if got := strings.TrimSpace(string(mem)); got != strconv.FormatInt(runtime.SandboxMemoryLimit(memoryMB), 10) {
		t.Fatalf("memory.max = %s, want %d", got, runtime.SandboxMemoryLimit(memoryMB))
	}
	cpu, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil {
		t.Fatalf("sandbox %s cpu limit: %v", id, err)
	}
	if got := strings.TrimSpace(string(cpu)); got != "200000 100000" {
		t.Fatalf("cpu.max = %s, want 200000 100000", got)
	}
}

// assertNoHostResources proves nothing is left named after one sandbox.
func assertNoHostResources(t *testing.T, root, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, "sandboxes", id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sandbox %s directory still exists: %v", id, err)
	}
	tap := "kiln-" + network.ShortID(id)
	if _, err := os.Stat("/sys/class/net/" + tap); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tap %s still exists: %v", tap, err)
	}
	if _, err := os.Stat(runtime.CgroupDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sandbox %s cgroup still exists: %v", id, err)
	}
	out, err := exec.Command("nft", "list", "table", "inet", "kiln").CombinedOutput()
	if err == nil && strings.Contains(string(out), "kiln_"+network.ShortID(id)+"_") {
		t.Errorf("sandbox %s chains still exist", id)
	}
}

// assertEventTrail proves the state transitions appear in order.
func assertEventTrail(t *testing.T, st store.Store, id string, want [][2]string) {
	t.Helper()
	events, err := st.ListEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for _, e := range events {
		if i < len(want) && e.FromState == want[i][0] && e.ToState == want[i][1] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("event trail for %s stopped at %v; events: %+v", id, want[i], events)
	}
}

// assertEventHasReason proves a failed row records why.
func assertEventHasReason(t *testing.T, st store.Store, id string) {
	t.Helper()
	events, err := st.ListEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ToState == store.SandboxFailed && strings.TrimSpace(e.Reason) != "" {
			return
		}
	}
	t.Fatalf("no event records why sandbox %s failed: %+v", id, events)
}

func countProcesses(t *testing.T, name string) int {
	t.Helper()
	var cmd *exec.Cmd
	switch name {
	case "firecracker":
		cmd = exec.Command("pgrep", "-c", "firecracker")
	case "snapfault":
		cmd = exec.Command("pgrep", "-c", "-f", "kiln snapfault")
	default:
		t.Fatalf("countProcesses: unknown process %q", name)
	}
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return 0
		}
		t.Fatalf("pgrep %s: %v", name, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("pgrep %s: %q", name, out)
	}
	return n
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
	ControlAddr string `json:"control_addr"`
} {
	t.Helper()
	var cfg struct {
		BearerToken string `json:"bearer_token"`
		ControlAddr string `json:"control_addr"`
	}
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatalf("config.json: %v (run kiln init)", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	if cfg.ControlAddr == "" {
		t.Fatalf("config.json has no control_addr")
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
