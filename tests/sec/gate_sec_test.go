//go:build kvm

// Package secgate is the security gate: `just gate sec`. The phase gates
// prove each feature works; this gate re-runs the controls in the SDD
// section 5 after a change that touches internal/network, the jailer setup
// in internal/runtime, or the resume hooks in guest/kilninit.
//
// It needs KVM and root, and it does not need a zone, wildcard DNS or an
// ACME account: the ingress handler runs in this process behind a
// httptest TLS server, and the client sends the published Host by hand.
// The gate recipe runs it under scripts/leak.sh.
package secgate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

	authall "github.com/alternayte/auth-all"

	"github.com/alternayte/kiln/internal/ingress"
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

const (
	templateName = "sec-python"
	zone         = "sec.example"
	secretName   = "SEC_TEST_SECRET"
	secretValue  = "sec-secret-value-8f3a"
	viewerEmail  = "gate-sec@example.invalid"
	viewerPass   = "gate-sec-password"
)

// TestSecControls proves the controls C1 to C9 that the SDD requires. Every
// control runs here, even where a phase gate already covers it, because this
// gate is what a change to the network, the jailer or the resume hooks must
// re-run.
func TestSecControls(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate sec needs root: start it with sudo")
	}
	repo := repoRoot(t)
	runPreflight(t, repo)
	root := kilnRoot()
	kernel := kernelPath(t, repo, root)
	kilninit := filepath.Join(root, "bin", "kilninit")
	if _, err := os.Stat(kilninit); err != nil {
		t.Fatalf("kilninit %s: %v (run kiln init)", kilninit, err)
	}
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
		Secrets:    map[string]string{secretName: secretValue},
		Zone:       zone,
	})
	clearSandboxes(t, ctx, st, sbx)
	clearSnapshots(t, ctx, st, sbx, root)
	_ = tmgr.Delete(ctx, templateName)
	t.Cleanup(func() {
		clearCtx := context.WithoutCancel(ctx)
		clearSandboxes(t, clearCtx, st, sbx)
		clearSnapshots(t, clearCtx, st, sbx, root)
		_ = tmgr.Delete(clearCtx, templateName)
	})

	buildTemplate(t, ctx, tmgr)

	// One source sandbox carries the controls that need a live guest.
	source := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template:    templateName,
		Lifecycle:   store.LifecycleEphemeral,
		IdleSeconds: 900,
	})
	t.Cleanup(func() { _ = sbx.Destroy(context.WithoutCancel(ctx), source.ID) })
	startGuestServer(t, ctx, sbx, source.ID)

	t.Run("C1Jail", func(t *testing.T) {
		checkJail(t, ctx, st, sbx, source)
	})
	t.Run("C2DefaultDenyEgress", func(t *testing.T) {
		checkEgress(t, ctx, st, sbx, source)
	})
	t.Run("C3Secrets", func(t *testing.T) {
		checkSecrets(t, ctx, root, sbx, source)
	})
	t.Run("C4EntropyC5Clock", func(t *testing.T) {
		checkEntropyAndClock(t, ctx, sbx, source)
	})
	t.Run("C6HostnamesAndSession", func(t *testing.T) {
		checkHostnamesAndSession(t, ctx, st, sbx)
	})
	t.Run("C6aWakeCap", func(t *testing.T) {
		checkWakeCap(t, ctx, st, sbx)
	})
	t.Run("C7CgroupLimits", func(t *testing.T) {
		checkCgroupLimits(t, source)
	})
	t.Run("C8ControlPlane", func(t *testing.T) {
		checkControlPlane(t, ctx, sbx, source)
	})
	t.Run("C9PreviewHeaders", func(t *testing.T) {
		checkPreviewHeaders(t, ctx, st, sbx)
	})
}

// C1: every Firecracker runs under jailer, in its own jail and cgroup, with
// dropped privileges and seccomp on, and it cannot see the host filesystem.
func checkJail(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager, sb store.Sandbox) {
	t.Helper()
	row, err := st.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	pid := row.PID
	if pid <= 0 {
		t.Fatal("the sandbox row has no pid")
	}
	root, err := os.Readlink(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/root: %v", pid, err)
	}
	// The jail root holds the kernel and the drives that jailer linked in,
	// and it has no host /etc. The readlink target alone is not reliable:
	// a process in its own mount namespace reports "/".
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		t.Fatalf("list the jail root %s: %v", root, err)
	}
	inside := map[string]bool{}
	for _, entry := range entries {
		inside[entry.Name()] = true
	}
	for _, name := range []string{"vmlinux", "rootfs.ext4"} {
		if !inside[name] {
			t.Fatalf("the jail root %s is missing %s: %v", root, name, inside)
		}
	}
	if inside["etc"] {
		t.Fatalf("the jail root %s carries the host /etc; the process is not jailed", root)
	}
	cgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cgroup), "/kiln/"+sb.ID) {
		t.Fatalf("firecracker cgroup %q, want /kiln/%s", strings.TrimSpace(string(cgroup)), sb.ID)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	text := string(status)
	if !strings.Contains(text, "Seccomp:\t2") {
		t.Fatalf("seccomp is not on:\n%s", grepLines(text, "Seccomp:"))
	}
	if !strings.Contains(text, "Uid:\t65534\t65534\t65534\t65534") {
		t.Fatalf("firecracker did not drop privileges:\n%s", grepLines(text, "Uid:"))
	}
	out := execGuest(t, ctx, sbx, sb, "sh", "-c", "test -e /var/lib/kiln && echo leak || echo clean")
	if strings.TrimSpace(out.Stdout) != "clean" {
		t.Fatalf("the guest sees the host root: %q", out.Stdout)
	}
}

// C2: egress is default-deny. Only the resolved allowlist reaches port 443,
// and the metadata address is dropped before any allow rule.
func checkEgress(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager, sb store.Sandbox) {
	t.Helper()
	allowed := "import socket; socket.create_connection(('pypi.org',443),timeout=30).close(); print('ok')"
	if out := execGuest(t, ctx, sbx, sb, "python", "-c", allowed); !strings.Contains(out.Stdout, "ok") {
		t.Fatalf("the allowlisted host was not reachable: exit %d stderr %q", out.ExitCode, out.Stderr)
	}
	denied := "import socket; socket.create_connection(('example.com',443),timeout=5); print('reached')"
	if out := execGuest(t, ctx, sbx, sb, "python", "-c", denied); out.ExitCode == 0 {
		t.Fatalf("a non-allowlisted host was reachable: %q", out.Stdout)
	}
	// A name outside the allowlist gets no DNS answer either.
	dns := "import socket; print(socket.gethostbyname('example.com'))"
	if out := execGuest(t, ctx, sbx, sb, "python", "-c", dns); out.ExitCode == 0 {
		t.Fatalf("the resolver answered a non-allowlisted name: %q", out.Stdout)
	}
	// The metadata address stays dropped even when it is in the allow set.
	row, err := st.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	set := "kiln_" + strings.TrimPrefix(row.TapName, "kiln-") + "_v4"
	if out, err := exec.CommandContext(ctx, "nft", "add", "element", "inet", "kiln", set,
		"{", "169.254.169.254", "timeout", "120s", "}").CombinedOutput(); err != nil {
		t.Fatalf("add the metadata address to the allow set: %v: %s", err, out)
	}
	metadata := "import socket; socket.create_connection(('169.254.169.254',80),timeout=5)"
	if out := execGuest(t, ctx, sbx, sb, "python", "-c", metadata); out.ExitCode == 0 {
		t.Fatal("the metadata address is reachable with it in the allow set")
	}
}

// C3: a secret is injected after the restore, never into a template. A
// secret-bearing sandbox is not forked without the explicit flag.
func checkSecrets(t *testing.T, ctx context.Context, root string, sbx *sandbox.Manager, source store.Sandbox) {
	t.Helper()
	dir := filepath.Join(root, "templates", templateName)
	for _, name := range []string{"manifest.json", "state", "mem", "rootfs.ext4"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, "grep", "-c", "-F", secretValue, path).CombinedOutput()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				continue
			}
			t.Fatalf("grep %s: %v: %s", path, err, out)
		}
		if strings.TrimSpace(string(out)) != "0" {
			t.Fatalf("the template file %s holds the secret value", name)
		}
	}
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template:    templateName,
		Lifecycle:   store.LifecycleEphemeral,
		IdleSeconds: 900,
		Secrets:     []string{secretName},
	})
	t.Cleanup(func() { _ = sbx.Destroy(context.WithoutCancel(ctx), sb.ID) })
	if out := execGuest(t, ctx, sbx, sb, "sh", "-c", "printf '%s' \"$"+secretName+"\""); out.Stdout != secretValue {
		t.Fatalf("the secret was not injected: exit %d stdout %q", out.ExitCode, out.Stdout)
	}
	if _, err := sbx.Fork(ctx, sb.ID, 1, false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("forking a secret-bearing sandbox returned %v, want conflict", err)
	}
}

// C4 and C5: every resume reseeds entropy and resets the clock.
func checkEntropyAndClock(t *testing.T, ctx context.Context, sbx *sandbox.Manager, source store.Sandbox) {
	t.Helper()
	snap, err := sbx.Snapshot(ctx, source.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sbx.DeleteSnapshot(context.WithoutCancel(ctx), snap.ID) })
	copies, err := sbx.RestoreSnapshot(ctx, snap.ID, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, copyRow := range copies {
		t.Cleanup(func() { _ = sbx.Destroy(context.WithoutCancel(ctx), copyRow.ID) })
	}
	values := map[string]bool{}
	hostNow := time.Now().Unix()
	for i, copyRow := range copies {
		out := execGuest(t, ctx, sbx, copyRow, "python", "-c", "import os; print(os.urandom(16).hex())")
		value := strings.TrimSpace(out.Stdout)
		if len(value) != 32 {
			t.Fatalf("copy %d urandom %q", i, value)
		}
		values[value] = true
		clock := execGuest(t, ctx, sbx, copyRow, "python", "-c", "import time; print(int(time.time()))")
		guestNow, err := strconv.ParseInt(strings.TrimSpace(clock.Stdout), 10, 64)
		if err != nil {
			t.Fatalf("copy %d clock %q", i, clock.Stdout)
		}
		if delta := guestNow - hostNow; delta > 2 || delta < -2 {
			t.Fatalf("copy %d clock differs by %ds", i, delta)
		}
	}
	if len(values) != 3 {
		t.Fatalf("three copies produced %d distinct random values", len(values))
	}
}

// C6: a hostname holds 128 bits that are not derived from the sandbox, a
// retired hostname is never reused, and a team preview checks the session
// before the guest sees anything.
func checkHostnamesAndSession(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager) {
	t.Helper()
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template:    templateName,
		Lifecycle:   store.LifecycleEphemeral,
		IdleSeconds: 900,
	})
	t.Cleanup(func() { _ = sbx.Destroy(context.WithoutCancel(ctx), sb.ID) })
	startGuestServer(t, ctx, sbx, sb.ID)

	public, err := sbx.Publish(ctx, sb.ID, 7000, store.VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	team, err := sbx.Publish(ctx, sb.ID, 7001, store.VisibilityTeam)
	if err != nil {
		t.Fatal(err)
	}
	if len(public.Subdomain) != 32 {
		t.Fatalf("public hostname %q, want 32 hex characters", public.Subdomain)
	}
	if _, err := hex.DecodeString(public.Subdomain); err != nil {
		t.Fatalf("public hostname %q is not hex", public.Subdomain)
	}
	if want := public.Subdomain + "-7001"; team.Subdomain != want {
		t.Fatalf("team hostname %q, want %q", team.Subdomain, want)
	}
	if strings.Contains(public.Subdomain, sb.ID) {
		t.Fatal("the hostname is derived from the sandbox id")
	}

	// A team preview challenges before the guest sees the request.
	auth := newAuth(t, ctx)
	ts := newPreview(t, ctx, st, sbx, auth)
	before := guestRequestCount(t, ctx, sbx, sb.ID)
	status, header, _ := fetch(t, ts, publicHost(team.Subdomain), "/", "")
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("the team preview answered %d without a session", status)
	}
	assertPreviewHeaders(t, header)
	if after := guestRequestCount(t, ctx, sbx, sb.ID); after != before {
		t.Fatal("the unauthenticated request reached the guest")
	}
	// A viewer session opens it.
	if err := ingress.CreateViewer(ctx, auth, viewerEmail, viewerPass); err != nil {
		t.Fatal(err)
	}
	cookie := signIn(t, ts, publicHost(team.Subdomain))
	status, header, body := fetch(t, ts, publicHost(team.Subdomain), "/", cookie)
	if status != http.StatusOK || body != "sec-"+sb.ID {
		t.Fatalf("the team preview with a session answered %d %q", status, body)
	}
	assertPreviewHeaders(t, header)
	if after := guestRequestCount(t, ctx, sbx, sb.ID); after <= before {
		t.Fatal("the authenticated request did not reach the guest")
	}
	// The public hostname needs no session, and the retired one is gone.
	if status, header, body = fetch(t, ts, publicHost(public.Subdomain), "/", ""); status != http.StatusOK || body != "sec-"+sb.ID {
		t.Fatalf("the public preview answered %d %q", status, body)
	}
	assertPreviewHeaders(t, header)
	// A retired hostname 404s, even for a viewer, and leaves a tombstone.
	if err := sbx.Retire(ctx, sb.ID, 7001); err != nil {
		t.Fatal(err)
	}
	if status, _, _ = fetch(t, ts, publicHost(team.Subdomain), "/", cookie); status != http.StatusNotFound {
		t.Fatalf("the retired hostname answered %d, want 404", status)
	}
	other := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template:    templateName,
		Lifecycle:   store.LifecycleEphemeral,
		IdleSeconds: 900,
	})
	t.Cleanup(func() { _ = sbx.Destroy(context.WithoutCancel(ctx), other.ID) })
	if err := st.PublishSandbox(ctx, store.Published{
		SandboxID: other.ID, GuestPort: 9000, Subdomain: team.Subdomain,
		Visibility: store.VisibilityPublic, CreatedAt: time.Now(),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reusing the retired hostname returned %v, want conflict", err)
	}
}

// C6a: a burst of requests to a sleeping sandbox is capped host-wide, and
// over the cap the proxy answers 503 with Retry-After and queues nothing.
func checkWakeCap(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager) {
	t.Helper()
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template:    templateName,
		Lifecycle:   store.LifecyclePersistent,
		IdleSeconds: 900,
	})
	t.Cleanup(func() { _ = sbx.Destroy(context.WithoutCancel(ctx), sb.ID) })
	startGuestServer(t, ctx, sbx, sb.ID)
	published, err := sbx.Publish(ctx, sb.ID, 7000, store.VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	auth := newAuth(t, ctx)
	ts := newPreview(t, ctx, st, sbx, auth)
	if _, err := sbx.Snapshot(ctx, sb.ID, true); err != nil {
		t.Fatal(err)
	}
	waitState(t, ctx, st, sb.ID, store.SandboxSleeping, 60*time.Second)

	type result struct {
		status int
		retry  string
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
			if err != nil {
				results <- result{}
				return
			}
			req.Host = publicHost(published.Subdomain)
			resp, err := ts.Client().Do(req)
			if err != nil {
				results <- result{}
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			results <- result{status: resp.StatusCode, retry: resp.Header.Get("Retry-After")}
		}()
	}
	wg.Wait()
	close(results)
	var refused, served int
	for r := range results {
		switch {
		case r.status == http.StatusServiceUnavailable:
			refused++
			if r.retry != "30" {
				t.Fatalf("a refused wake carries Retry-After %q, want 30", r.retry)
			}
		case r.status == http.StatusOK:
			served++
		}
	}
	if refused == 0 {
		t.Fatalf("the wake burst was not capped: %d served, 0 refused", served)
	}
	if served == 0 {
		t.Fatal("every request in the wake burst was refused")
	}
}

// C7: every sandbox cgroup carries the template's memory and cpu limits.
func checkCgroupLimits(t *testing.T, sb store.Sandbox) {
	t.Helper()
	dir := runtime.CgroupDir(sb.ID)
	memory, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		t.Fatalf("read the memory limit: %v", err)
	}
	if got, want := strings.TrimSpace(string(memory)), strconv.FormatInt(runtime.SandboxMemoryLimit(512), 10); got != want {
		t.Fatalf("memory.max = %s, want %s", got, want)
	}
	cpu, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil {
		t.Fatalf("read the cpu limit: %v", err)
	}
	if got := strings.TrimSpace(string(cpu)); got != "200000 100000" {
		t.Fatalf("cpu.max = %s, want 200000 100000", got)
	}
}

// C8: a control path on a published hostname is never routed, and the guest
// cannot reach the control port even while a listener answers there.
func checkControlPlane(t *testing.T, ctx context.Context, sbx *sandbox.Manager, source store.Sandbox) {
	t.Helper()
	hostIP := hostUplinkIP(t)
	// Bind the control port on the host's own address, so an allowed packet
	// would find a listener. kiln serve may hold 127.0.0.1:8080.
	ln, err := net.Listen("tcp", net.JoinHostPort(hostIP, "8080"))
	if err != nil {
		ln, err = net.Listen("tcp", "0.0.0.0:8080")
	}
	if err != nil {
		t.Fatalf("listen on the control port: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	code := fmt.Sprintf("import socket; socket.create_connection((%q, 8080), timeout=5); print('reached')", hostIP)
	if out := execGuest(t, ctx, sbx, source, "python", "-c", code); out.ExitCode == 0 {
		t.Fatalf("the guest reached the control listener at %s:8080: %q", hostIP, out.Stdout)
	}
}

// C9: every preview response is unindexable and sandboxed, the guest never
// sees a host credential, and a guest cookie comes back host-only.
func checkPreviewHeaders(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager) {
	t.Helper()
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template:    templateName,
		Lifecycle:   store.LifecycleEphemeral,
		IdleSeconds: 900,
	})
	t.Cleanup(func() { _ = sbx.Destroy(context.WithoutCancel(ctx), sb.ID) })
	startGuestServer(t, ctx, sbx, sb.ID)
	published, err := sbx.Publish(ctx, sb.ID, 7000, store.VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	auth := newAuth(t, ctx)
	ts := newPreview(t, ctx, st, sbx, auth)
	host := publicHost(published.Subdomain)

	status, header, body := fetchWithHeaders(t, ts, host, "/", "", map[string]string{
		"Cookie":        "host=credential",
		"Authorization": "Bearer control-token",
	})
	if status != http.StatusOK || body != "sec-"+sb.ID {
		t.Fatalf("the preview answered %d %q", status, body)
	}
	assertPreviewHeaders(t, header)
	seen := strings.ToLower(guestRequestLog(t, ctx, sbx, sb.ID))
	for _, banned := range []string{"credential", "control-token"} {
		if strings.Contains(seen, banned) {
			t.Fatalf("the guest saw %q in its request log", banned)
		}
	}
	// A guest cookie is host-only: the proxy drops its Domain attribute.
	if status, header, _ := fetch(t, ts, host, "/cookie", ""); status != http.StatusOK {
		t.Fatalf("the cookie path answered %d", status)
	} else if setCookie := header.Get("Set-Cookie"); strings.Contains(strings.ToLower(setCookie), "domain=") {
		t.Fatalf("the guest cookie kept its Domain attribute: %q", setCookie)
	}
	// A control path on the preview host is a 404, and the guest would have
	// answered 200.
	if status, _, body := fetch(t, ts, host, "/v1/sandboxes", ""); status != http.StatusNotFound || body == "GUEST-V1" {
		t.Fatalf("GET /v1/sandboxes on the preview host answered %d %q", status, body)
	}
	// An unknown hostname is a 404 with the preview headers.
	status, header, _ = fetch(t, ts, publicHost(randomLabel(t)), "/", "")
	if status != http.StatusNotFound {
		t.Fatalf("an unknown hostname answered %d", status)
	}
	assertPreviewHeaders(t, header)
}

// ---------------------------------------------------------------------------
// Helpers

func buildTemplate(t *testing.T, ctx context.Context, tmgr *template.Manager) {
	t.Helper()
	if err := tmgr.Start(ctx, template.BuildRequest{
		Name:        templateName,
		Image:       "docker.io/library/python:3.12-slim",
		VCPUs:       2,
		MemoryMB:    512,
		DiskMB:      2048,
		Setup:       []string{},
		EgressAllow: []string{"pypi.org"},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		info, err := tmgr.TemplateInfo(ctx, templateName)
		if err != nil {
			t.Fatal(err)
		}
		if info.State == store.TemplateReady {
			return
		}
		if info.State == store.TemplateFailed {
			t.Fatalf("template build failed: %s", info.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("template stuck in %q", info.State)
		}
		time.Sleep(time.Second)
	}
}

func createSandbox(t *testing.T, ctx context.Context, sbx *sandbox.Manager, req sandbox.CreateRequest) store.Sandbox {
	t.Helper()
	row, err := sbx.Create(ctx, req)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if row.State != store.SandboxRunning {
		t.Fatalf("created sandbox state %q", row.State)
	}
	return row
}

func execGuest(t *testing.T, ctx context.Context, sbx *sandbox.Manager, sb store.Sandbox, argv ...string) runtime.ExecResult {
	t.Helper()
	out, err := sbx.Exec(ctx, sb.ID, runtime.ExecRequest{Cmd: argv, TimeoutSeconds: 300}, nil)
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	return out
}

// inGuestServer answers both preview ports, logs every request with its
// headers, and echoes a marker. MARKER is replaced with the sandbox id.
const inGuestServer = `import http.server, socketserver, threading, time, json

LOG = "/tmp/requests.log"
MARKER = "__MARKER__"

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(LOG, "a") as f:
            f.write(json.dumps({"path": self.path, "headers": dict(self.headers)}) + "\n")
        body = ("sec-" + MARKER).encode()
        if self.path == "/v1/sandboxes":
            body = b"GUEST-V1"
        self.send_response(200)
        if self.path == "/cookie":
            self.send_header("Set-Cookie", "guest=1; Domain=.sec.example; Path=/")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

for port in (7000, 7001):
    server = Server(("0.0.0.0", port), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
while True:
    time.sleep(60)
`

func startGuestServer(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string) {
	t.Helper()
	script := strings.ReplaceAll(inGuestServer, "__MARKER__", id)
	if err := sbx.WriteFile(ctx, id, "/tmp/sec_server.py", strings.NewReader(script)); err != nil {
		t.Fatalf("write the guest server: %v", err)
	}
	if err := runGuest(ctx, sbx, id, "sh", "-c", "nohup python /tmp/sec_server.py >/tmp/sec_server.log 2>&1 &"); err != nil {
		t.Fatalf("start the guest server: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if out, err := sbx.Exec(ctx, id, runtime.ExecRequest{
			Cmd:            []string{"python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:7000', timeout=2)"},
			TimeoutSeconds: 30,
		}, nil); err == nil && out.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			log := guestOutput(t, ctx, sbx, id, "cat", "/tmp/sec_server.log")
			listing := guestOutput(t, ctx, sbx, id, "sh", "-c", "ls -l /tmp; ps aux | head -20")
			t.Fatalf("the in-guest server did not start; server log:\n%s\nguest state:\n%s", log, listing)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// guestOutput runs one command and returns its output whatever its exit code.
func guestOutput(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string, argv ...string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, id, runtime.ExecRequest{Cmd: argv, TimeoutSeconds: 30}, nil)
	if err != nil {
		return fmt.Sprintf("exec %v: %v", argv, err)
	}
	return out.Stdout + out.Stderr
}

// runGuest runs one command and fails when its exit code is not zero.
func runGuest(ctx context.Context, sbx *sandbox.Manager, id string, argv ...string) error {
	out, err := sbx.Exec(ctx, id, runtime.ExecRequest{Cmd: argv, TimeoutSeconds: 300}, nil)
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", out.ExitCode, out.Stderr)
	}
	return nil
}

func guestRequestCount(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string) int {
	t.Helper()
	out, err := sbx.Exec(ctx, id, runtime.ExecRequest{
		Cmd:            []string{"sh", "-c", "wc -l < /tmp/requests.log"},
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out.Stdout))
	if err != nil {
		t.Fatalf("guest request log: %q", out.Stdout)
	}
	return n
}

func guestRequestLog(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string) string {
	t.Helper()
	out, err := sbx.Exec(ctx, id, runtime.ExecRequest{
		Cmd:            []string{"cat", "/tmp/requests.log"},
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out.Stdout
}

// newAuth opens a viewer store in a temp file, so the gate never touches the
// host's viewer accounts.
func newAuth(t *testing.T, ctx context.Context) *authall.Auth {
	t.Helper()
	auth, db, err := ingress.NewAuth(ctx, filepath.Join(t.TempDir(), "viewer.db"), zone)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return auth
}

func newPreview(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager, auth *authall.Auth) *httptest.Server {
	t.Helper()
	handler := ingress.New(ingress.Config{Zone: zone, Store: st, Sandboxes: sbx, Auth: auth}).Handler()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// fetch sends a request to the preview listener with the published hostname.
func fetch(t *testing.T, ts *httptest.Server, host, path, cookie string) (int, http.Header, string) {
	t.Helper()
	return fetchWithHeaders(t, ts, host, path, cookie, nil)
}

func fetchWithHeaders(t *testing.T, ts *httptest.Server, host, path, cookie string, extra map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for name, value := range extra {
		req.Header.Set(name, value)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s%s: %v", host, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func signIn(t *testing.T, ts *httptest.Server, host string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": viewerEmail, "password": viewerPass})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/_kiln/auth/sign-in/email", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in: status %d: %s", resp.StatusCode, data)
	}
	for _, cookie := range resp.Header.Values("Set-Cookie") {
		if strings.Contains(cookie, "=") {
			pair, _, _ := strings.Cut(cookie, ";")
			return strings.TrimSpace(pair)
		}
	}
	t.Fatalf("sign-in set no cookie: %s", data)
	return ""
}

func assertPreviewHeaders(t *testing.T, header http.Header) {
	t.Helper()
	if got := header.Get("X-Robots-Tag"); got != "noindex" {
		t.Fatalf("X-Robots-Tag = %q, want noindex", got)
	}
	if got := header.Get("Content-Security-Policy"); got != "sandbox" {
		t.Fatalf("Content-Security-Policy = %q, want sandbox", got)
	}
}

func publicHost(subdomain string) string {
	if strings.Contains(subdomain, ".") {
		return subdomain
	}
	return subdomain + "." + zone
}

func randomLabel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
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

func waitState(t *testing.T, ctx context.Context, st store.Store, id, state string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		row, err := st.GetSandbox(ctx, id)
		if err != nil {
			t.Fatal(err)
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

func grepLines(text, prefix string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func clearSandboxes(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager) {
	rows, err := st.ListSandboxes(ctx)
	if err != nil {
		t.Logf("cleanup: list sandboxes: %v", err)
		return
	}
	for _, row := range rows {
		if row.TemplateName == templateName && row.DestroyedAt == nil {
			if err := sbx.Destroy(ctx, row.ID); err != nil {
				t.Logf("cleanup: destroy %s: %v", row.ID, err)
			}
		}
	}
}

func clearSnapshots(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager, root string) {
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
		case snap.TemplateName == templateName:
			if derr := sbx.DeleteSnapshot(ctx, snap.ID); derr != nil {
				if derr2 := st.DeleteSnapshot(ctx, snap.ID); derr2 != nil {
					t.Logf("cleanup: delete snapshot %s: %v", snap.ID, derr2)
				}
			}
			_ = os.RemoveAll(dir)
		}
	}
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
