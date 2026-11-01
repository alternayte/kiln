//go:build kvm

package secgate

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/reconcile"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

const (
	agentTemplate = "sec-agent"
	agentImage    = "docker.io/library/python:3.12-slim"
	// agentUID is the uid of nobody in the image.
	agentUID  = "65534"
	agentPort = 8000
)

// canReach is a guest program: it prints "reached" when it can connect to one
// host on port 443, and exits non-zero otherwise.
const canReach = "import socket,sys; socket.create_connection((sys.argv[1],443),timeout=8).close(); print('reached')"

// holdConnection opens a TLS connection, waits for /tmp/go, then tries to use
// it. The guard answers any connect, so only a finished handshake proves the
// connection reached the site. A cut connection answers nothing.
const holdConnection = `import os,socket,ssl,time
s=ssl.create_default_context().wrap_socket(socket.create_connection(('pypi.org',443),timeout=20),server_hostname='pypi.org')
print('connected',flush=True)
while not os.path.exists('/tmp/go'): time.sleep(0.2)
s.settimeout(15)
try:
    s.send(b'HEAD / HTTP/1.0\r\nHost: pypi.org\r\n\r\n')
    print('answered' if s.recv(64) else 'cut',flush=True)
except Exception as e:
    print('cut',flush=True)
`

// TestSecAgentControls proves what lets an untrusted agent run in a sandbox:
// the template user, the egress change on a running sandbox, and the detached
// processes. It also proves the build reports its phases.
func TestSecAgentControls(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate sec needs root: start it with sudo")
	}
	repo := repoRoot(t)
	root := kilnRoot()
	kernel := kernelPath(t, repo, root)
	kilninit := filepath.Join(root, "bin", "kilninit")
	kilnBin := buildKiln(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	st, err := store.Open(filepath.Join(root, "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rt, err := runtime.New(runtime.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	nm := network.New(kilnBin, root)
	if err := nm.EnsureBase(ctx); err != nil {
		t.Fatal(err)
	}
	tmgr := &template.Manager{
		Root: root, Store: st, Runtime: rt, Network: nm,
		Builder:    template.Builder{KilninitPath: kilninit},
		KernelPath: kernel,
	}
	newManager := func(net *network.Manager) *sandbox.Manager {
		return sandbox.New(sandbox.Config{
			Root: root, Store: st, Runtime: rt, Network: net,
			Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
			KernelPath: kernel,
			Zone:       zone,
		})
	}
	sbx := newManager(nm)
	destroyAll := func() {
		clean := context.WithoutCancel(ctx)
		rows, err := st.ListSandboxes(clean)
		if err != nil {
			t.Logf("cleanup: %v", err)
			return
		}
		for _, row := range rows {
			if row.TemplateName == agentTemplate && row.DestroyedAt == nil {
				_ = sbx.Destroy(clean, row.ID)
			}
		}
		snaps, _ := st.ListSnapshots(clean)
		for _, snap := range snaps {
			if snap.TemplateName == agentTemplate {
				_ = sbx.DeleteSnapshot(clean, snap.ID)
			}
		}
		_ = tmgr.Delete(clean, agentTemplate)
	}
	destroyAll()
	t.Cleanup(destroyAll)

	t.Run("BuildFailures", func(t *testing.T) { checkBuildFailures(t, ctx, st, tmgr) })

	lastEvent, err := st.LastEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	build(t, ctx, tmgr, template.BuildRequest{
		Name: agentTemplate, Image: agentImage, VCPUs: 2, MemoryMB: 512, DiskMB: 2048,
		Setup:       []string{"mkdir -p /opt/tools && touch /opt/tools/runner && chmod 755 /opt/tools"},
		EgressAllow: []string{"pypi.org"},
		Start:       []string{"python", "-m", "http.server", fmt.Sprint(agentPort)},
		StartPort:   agentPort,
		User:        "nobody",
	}, store.TemplateReady)
	source := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template: agentTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: 900,
	})

	t.Run("Phases", func(t *testing.T) {
		events, err := st.ListEventsSince(ctx, lastEvent, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, e := range events {
			if e.Phase != "" && (e.Template == agentTemplate || e.SandboxID == source.ID) {
				seen[e.Phase] = true
			}
		}
		for _, phase := range []string{"layers", "rootfs", "setup_boot", "setup", "snapshot_boot", "snapshot", "restore", "start"} {
			if !seen[phase] {
				t.Errorf("no event for the phase %q; saw %v", phase, seen)
			}
		}
	})
	t.Run("GuestEndsWithoutAKill", func(t *testing.T) {
		events, err := st.ListEventsSince(ctx, lastEvent, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Template == agentTemplate && e.Phase == "setup" && e.DurationMS >= 4500 {
				t.Errorf("setup took %d ms, which is the shutdown grace", e.DurationMS)
			}
		}
	})
	t.Run("User", func(t *testing.T) { checkUser(t, ctx, sbx, source) })
	t.Run("Egress", func(t *testing.T) { checkEgressChange(t, ctx, st, sbx, source) })
	t.Run("Processes", func(t *testing.T) { checkProcesses(t, ctx, st, sbx) })
	t.Run("Restart", func(t *testing.T) {
		// A new manager is what a restarted daemon has: no handles, and only
		// the row and the guest to go on.
		restartedNet := network.New(kilnBin, root)
		restarted := newManager(restartedNet)
		sweep := &reconcile.Reconciler{Root: root, Store: st, Sandboxes: restarted, Network: restartedNet, Templates: tmgr, Runtime: rt}
		if _, err := sweep.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if !restarted.Has(source.ID) {
			t.Fatal("the restart did not adopt the sandbox")
		}
		assertReach(t, ctx, restarted, source.ID, "www.python.org", true)
		assertReach(t, ctx, restarted, source.ID, "pypi.org", false)
	})
}

// build starts one template build and waits for the state it must end in.
func build(t *testing.T, ctx context.Context, tmgr *template.Manager, req template.BuildRequest, want string) template.Info {
	t.Helper()
	if err := tmgr.Start(ctx, req); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Minute)
	for {
		info, err := tmgr.TemplateInfo(ctx, req.Name)
		if err != nil {
			t.Fatal(err)
		}
		if info.State != store.TemplateBuilding {
			if info.State != want {
				t.Fatalf("template %s ended %q, want %q: %s", req.Name, info.State, want, info.Error)
			}
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("template %s stuck building", req.Name)
		}
		time.Sleep(time.Second)
	}
}

// A failed build shows where it stopped, and no credential reaches a row.
func checkBuildFailures(t *testing.T, ctx context.Context, st store.Store, tmgr *template.Manager) {
	base := template.BuildRequest{Image: agentImage, VCPUs: 1, MemoryMB: 256, DiskMB: 2048, EgressAllow: []string{}}
	after, err := st.LastEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	failing := base
	failing.Name = "sec-agent-setup"
	failing.Setup = []string{"exit 7"}
	build(t, ctx, tmgr, failing, store.TemplateFailed)
	t.Cleanup(func() { _ = tmgr.Delete(context.WithoutCancel(ctx), failing.Name) })
	events, err := st.ListEventsSince(ctx, after, 0)
	if err != nil {
		t.Fatal(err)
	}
	failedSetup := false
	for _, e := range events {
		if e.Template != failing.Name {
			continue
		}
		if e.Phase == "snapshot_boot" {
			t.Error("a failed setup still reached snapshot_boot")
		}
		if e.Phase == "setup" && strings.Contains(e.Reason, "exit 7") {
			failedSetup = true
		}
	}
	if !failedSetup {
		t.Error("the setup phase carries no error")
	}

	// The first build pulled every layer, so this one pulls none.
	second, err := st.LastEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	missing := base
	missing.Name = "sec-agent-nouser"
	missing.User = "no-such-user"
	info := build(t, ctx, tmgr, missing, store.TemplateFailed)
	t.Cleanup(func() { _ = tmgr.Delete(context.WithoutCancel(ctx), missing.Name) })
	if !strings.Contains(info.Error, "no-such-user") {
		t.Errorf("the missing user failed with %q", info.Error)
	}
	reused, err := st.ListEventsSince(ctx, second, 0)
	if err != nil {
		t.Fatal(err)
	}
	cachedAll := false
	for _, e := range reused {
		if e.Template != missing.Name || e.Phase != "layers" {
			continue
		}
		var hit, total int
		if _, err := fmt.Sscanf(e.Reason, "%d of %d layers cached", &hit, &total); err == nil && total > 0 && hit == total {
			cachedAll = true
		} else {
			t.Errorf("the second build reports %q", e.Reason)
		}
	}
	if !cachedAll {
		t.Error("the second build reports no cached layers")
	}

	const token = "sec-gate-wrong-token-5d1e"
	wrong := base
	wrong.Name = "sec-agent-cred"
	wrong.Credential = &template.Credential{Username: "kiln-gate-no-such-user", Token: token}
	info = build(t, ctx, tmgr, wrong, store.TemplateFailed)
	t.Cleanup(func() { _ = tmgr.Delete(context.WithoutCancel(ctx), wrong.Name) })
	if strings.Contains(info.Error, token) {
		t.Errorf("the template error holds the token: %q", info.Error)
	}
	all, err := st.ListEventsSince(ctx, after, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if strings.Contains(e.Reason, token) {
			t.Errorf("an event holds the token: %q", e.Reason)
		}
	}
	db, err := os.ReadFile(filepath.Join(kilnRoot(), "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(db), token) {
		t.Error("kiln.db holds the token")
	}
}

func checkUser(t *testing.T, ctx context.Context, sbx *sandbox.Manager, sb store.Sandbox) {
	if out := guestOutput(t, ctx, sbx, sb.ID, "id", "-u"); strings.TrimSpace(out) != agentUID {
		t.Errorf("exec runs as %q", out)
	}
	if out := guestOutput(t, ctx, sbx, sb.ID, "sh", "-c", "echo $HOME $USER"); strings.TrimSpace(out) != "/nonexistent nobody" {
		t.Errorf("exec environment is %q", out)
	}
	if res := execGuest(t, ctx, sbx, sb, "touch", "/opt/tools/runner"); res.ExitCode == 0 {
		t.Error("the user changed a root-owned file")
	}
	// The start command runs as the user too.
	whoRuns := `import os
for p in os.listdir('/proc'):
    if p.isdigit():
        try:
            if b'http.server' in open('/proc/'+p+'/cmdline','rb').read(): print(os.stat('/proc/'+p).st_uid)
        except OSError: pass`
	if out := guestOutput(t, ctx, sbx, sb.ID, "python", "-c", whoRuns); !strings.Contains(out, agentUID) {
		t.Errorf("the start command runs as %q", out)
	}

	if err := sbx.WriteFile(ctx, sb.ID, "/tmp/own", strings.NewReader("mine")); err != nil {
		t.Fatalf("write a file the user may write: %v", err)
	}
	if out := guestOutput(t, ctx, sbx, sb.ID, "stat", "-c", "%u", "/tmp/own"); strings.TrimSpace(out) != agentUID {
		t.Errorf("a written file belongs to %q", out)
	}
	if err := sbx.WriteFile(ctx, sb.ID, "/opt/tools/runner", strings.NewReader("shim")); err == nil {
		t.Error("the file API replaced a file in a root-owned directory")
	}
	// A planted symlink must not aim a root write at a root-owned file, nor
	// through a directory.
	guestOutput(t, ctx, sbx, sb.ID, "ln", "-s", "/opt/tools/runner", "/tmp/link")
	guestOutput(t, ctx, sbx, sb.ID, "ln", "-s", "/opt/tools", "/tmp/dir")
	if err := sbx.WriteFile(ctx, sb.ID, "/tmp/link", strings.NewReader("shim")); err == nil {
		t.Error("a write through a symlink to a root-owned file succeeded")
	}
	if err := sbx.WriteFile(ctx, sb.ID, "/tmp/dir/runner", strings.NewReader("shim")); err == nil {
		t.Error("a write through a symlinked directory succeeded")
	}
	if out := guestOutput(t, ctx, sbx, sb.ID, "cat", "/opt/tools/runner"); out != "" {
		t.Errorf("the root-owned file holds %q", out)
	}
	if out := guestOutput(t, ctx, sbx, sb.ID, "readlink", "/tmp/link"); strings.TrimSpace(out) != "/opt/tools/runner" {
		t.Errorf("the symlink became %q", out)
	}

	term, err := sbx.Terminal(ctx, sb.ID, guestproto.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if _, err := term.Write([]byte("id -u; exit\n")); err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(seen.String(), agentUID) {
		n, err := term.Read(buf)
		seen.Write(buf[:n])
		if err != nil {
			t.Errorf("the terminal ended without the uid: %q: %v", seen.String(), err)
			break
		}
	}
}

func assertReach(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id, host string, want bool) {
	t.Helper()
	res, err := sbx.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"python", "-c", canReach, host}, TimeoutSeconds: 60}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got := res.ExitCode == 0; got != want {
		t.Errorf("reach %s = %t, want %t: %s", host, got, want, res.Stderr)
	}
}

func checkEgressChange(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager, sb store.Sandbox) {
	assertReach(t, ctx, sbx, sb.ID, "pypi.org", true)
	assertReach(t, ctx, sbx, sb.ID, "www.python.org", false)

	// A connection that lives through the change, and the same probe with no
	// change, which proves the probe can tell the two apart.
	control := strings.Replace(holdConnection, "while not os.path.exists('/tmp/go'): time.sleep(0.2)\n", "", 1)
	if out := guestOutput(t, ctx, sbx, sb.ID, "python", "-c", control); !strings.Contains(out, "answered") {
		t.Fatalf("an open connection did not answer with no policy change: %q", out)
	}
	held, err := sbx.StartProcess(ctx, sb.ID, runtime.ProcessRequest{Cmd: []string{"python", "-c", holdConnection}, Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	waitProcessOutput(t, ctx, sbx, sb.ID, held, "connected")

	// A connection through the host to a published port stays.
	published, err := sbx.DialGuest(ctx, sb.ID, agentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()

	row, err := sbx.SetEgress(ctx, sb.ID, []string{"*.python.org"})
	if err != nil {
		t.Fatal(err)
	}
	if got := row.EgressAllow; len(got) != 1 || got[0] != "*.python.org" {
		t.Errorf("the row holds %v", got)
	}
	if err := sbx.WriteFile(ctx, sb.ID, "/tmp/go", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	waitProcessOutput(t, ctx, sbx, sb.ID, held, "cut")
	assertReach(t, ctx, sbx, sb.ID, "www.python.org", true)
	assertReach(t, ctx, sbx, sb.ID, "python.org", false)
	assertReach(t, ctx, sbx, sb.ID, "pypi.org", false)

	if err := published.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(published, "GET / HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatalf("the published connection ended: %v", err)
	}
	if resp, err := http.ReadResponse(bufio.NewReader(published), nil); err != nil {
		t.Errorf("the published connection ended: %v", err)
	} else {
		resp.Body.Close()
	}

	// The narrow policy holds in everything that restores.
	if err := sbx.Sleep(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	assertReach(t, ctx, sbx, sb.ID, "pypi.org", false)
	assertReach(t, ctx, sbx, sb.ID, "www.python.org", true)
	copies, err := sbx.Fork(ctx, sb.ID, 1, false, map[string]any{"run": "fork"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(copies[0].Metadata, `"run":"fork"`) {
		t.Errorf("fork metadata %q", copies[0].Metadata)
	}
	assertReach(t, ctx, sbx, copies[0].ID, "pypi.org", false)
	snap, err := sbx.Snapshot(ctx, sb.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := sbx.RestoreSnapshot(ctx, snap.ID, 1, false, map[string]any{"run": "restore"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(restored[0].Metadata, `"run":"restore"`) {
		t.Errorf("restore metadata %q", restored[0].Metadata)
	}
	assertReach(t, ctx, sbx, restored[0].ID, "pypi.org", false)

	// A sleeping sandbox takes a change without a wake, and the wake applies it.
	if err := sbx.Sleep(ctx, copies[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sbx.SetEgress(ctx, copies[0].ID, []string{"pypi.org"}); err != nil {
		t.Fatal(err)
	}
	if row, err := st.GetSandbox(ctx, copies[0].ID); err != nil || row.State != store.SandboxSleeping {
		t.Errorf("the change woke the sandbox: %q, %v", row.State, err)
	}
	assertReach(t, ctx, sbx, copies[0].ID, "pypi.org", true)
}

// waitProcessOutput polls a detached process until its stdout holds a word.
func waitProcessOutput(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id, pid, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := sbx.ReadProcess(ctx, id, pid, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.Stdout, want) {
			return
		}
		if !out.Running || time.Now().After(deadline) {
			t.Fatalf("process %s: no %q: running %t exit %d stdout %q stderr %q", pid, want, out.Running, out.ExitCode, out.Stdout, out.Stderr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func checkProcesses(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager) {
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template: agentTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: 4,
	})
	pid, err := sbx.StartProcess(ctx, sb.ID, runtime.ProcessRequest{
		Cmd: []string{"sh", "-c", "echo out; echo err >&2; id -u; sleep 3600"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitProcessOutput(t, ctx, sbx, sb.ID, pid, agentUID)
	first, err := sbx.ReadProcess(ctx, sb.ID, pid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Running || !strings.Contains(first.Stdout, "out\n") || first.Stderr != "err\n" {
		t.Errorf("first read: %+v", first)
	}
	next, err := sbx.ReadProcess(ctx, sb.ID, pid, first.StdoutOffset, first.StderrOffset)
	if err != nil {
		t.Fatal(err)
	}
	if next.Stdout != "" || next.Stderr != "" || next.StdoutOffset != first.StdoutOffset {
		t.Errorf("a read at the end returned output: %+v", next)
	}

	// Past the idle deadline, with nothing touching the sandbox.
	time.Sleep(12 * time.Second)
	row, err := st.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.SandboxRunning {
		t.Fatalf("the sandbox is %q with a process running", row.State)
	}

	// A sleep the caller asks for keeps the process, in memory.
	if err := sbx.Sleep(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	awake, err := sbx.ReadProcess(ctx, sb.ID, pid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !awake.Running || awake.Stderr != "err\n" {
		t.Errorf("after a sleep and a wake: %+v", awake)
	}

	if err := sbx.SignalProcess(ctx, sb.ID, pid, "TERM"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := sbx.ReadProcess(ctx, sb.ID, pid, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TERM did not end the process")
		}
		time.Sleep(time.Second)
	}
	if err := sbx.SignalProcess(ctx, sb.ID, pid, "KILL"); err != nil {
		t.Errorf("a signal to an ended process failed: %v", err)
	}
	if _, err := sbx.ReadProcess(ctx, sb.ID, "no-such-process", 0, 0); !runtime.IsExecError(err, guestproto.CodeNotFound) {
		t.Errorf("a missing process answered %v", err)
	}
	waitState(t, ctx, st, sb.ID, store.SandboxSleeping, 60*time.Second)

	// The same deadline must not destroy an ephemeral sandbox at work.
	eph := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template: agentTemplate, Lifecycle: store.LifecycleEphemeral, IdleSeconds: 4,
	})
	if _, err := sbx.StartProcess(ctx, eph.ID, runtime.ProcessRequest{Cmd: []string{"sleep", "3600"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(12 * time.Second)
	if row, err := st.GetSandbox(ctx, eph.ID); err != nil || row.DestroyedAt != nil {
		t.Errorf("the ephemeral sandbox ended with a process running: %+v, %v", row, err)
	}
	if err := sbx.Destroy(ctx, eph.ID); err != nil {
		t.Error(err)
	}
}
