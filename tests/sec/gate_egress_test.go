//go:build kvm

package secgate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/reconcile"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

const (
	egressTemplate = "sec-egress"
	// allowedName sits on a shared CDN address with many other sites.
	allowedName = "pypi.org"
	otherName   = "www.python.org"
	// mirrorName is a package mirror that apt reaches over plain HTTP.
	mirrorName = "deb.debian.org"
	// metadataName is a public name that resolves to the metadata address.
	metadataName = "169.254.169.254.sslip.io"
)

// tlsProbe prints "reached" when a TLS handshake with a name finishes against an
// address. With "verify" it also proves the certificate belongs to that name.
const tlsProbe = `import socket,ssl,sys
addr,sni,mode=sys.argv[1],sys.argv[2],sys.argv[3]
ctx=ssl.create_default_context()
if mode!='verify':
    ctx.check_hostname=False
    ctx.verify_mode=ssl.CERT_NONE
s=socket.create_connection((addr,443),timeout=15)
s.settimeout(15)
ctx.wrap_socket(s,server_hostname=sni or None).close()
print('reached')
`

// udpProbe sends one packet to UDP port 443 and prints what comes back. A
// connected socket reports the port unreachable answer as a refusal.
const udpProbe = `import socket,sys,time
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
s.settimeout(10)
s.connect((sys.argv[1],443))
start=time.time()
try:
    s.send(b'\xc0'+b'\0'*1199)
    print('answered' if s.recv(64) else '')
except ConnectionRefusedError:
    print('refused %.1f' % (time.time()-start))
`

// refusedProbe connects to an address and port, and prints how long the
// refusal took.
const refusedProbe = `import socket,sys,time
start=time.time()
try:
    socket.create_connection((sys.argv[1],int(sys.argv[2])),timeout=20).close()
    print('connected')
except ConnectionRefusedError:
    print('refused %.1f' % (time.time()-start))
`

// httpProbe sends one plain HTTP request with a chosen Host header to an
// address on port 80, and prints the status and the time it took.
const httpProbe = `import http.client,sys,time
start=time.time()
c=http.client.HTTPConnection(sys.argv[1],80,timeout=20)
c.request('GET',sys.argv[3],headers={'Host':sys.argv[2]})
r=c.getresponse()
print(r.status, '%.1f' % (time.time()-start), r.read(200))
`

// fastRefusal reads the output of a probe and reports whether it names a
// refusal that took under two seconds.
func fastRefusal(out string) bool {
	var took float64
	_, err := fmt.Sscanf(strings.TrimSpace(out), "refused %f", &took)
	return err == nil && took < 2
}

// TestSecEgressGuard runs each attack the guard exists to refuse, on real
// microVMs.
func TestSecEgressGuard(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate sec needs root: start it with sudo")
	}
	repo := repoRoot(t)
	root := kilnRoot()
	kernel := kernelPath(t, repo, root)
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
		Builder:    template.Builder{KilninitPath: filepath.Join(root, "bin", "kilninit")},
		KernelPath: kernel,
	}
	newManager := func(net *network.Manager, now func() time.Time) *sandbox.Manager {
		return sandbox.New(sandbox.Config{
			Root: root, Store: st, Runtime: rt, Network: net,
			Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
			KernelPath: kernel, Zone: zone, Now: now,
		})
	}
	sbx := newManager(nm, nil)
	destroyAll := func() {
		clean := context.WithoutCancel(ctx)
		rows, _ := st.ListSandboxes(clean)
		for _, row := range rows {
			if row.TemplateName == egressTemplate && row.DestroyedAt == nil {
				_ = sbx.Destroy(clean, row.ID)
			}
		}
		_ = tmgr.Delete(clean, egressTemplate)
	}
	destroyAll()
	t.Cleanup(destroyAll)

	build(t, ctx, tmgr, template.BuildRequest{
		Name: egressTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 512, DiskMB: 2048,
		EgressAllow: []string{allowedName, metadataName, mirrorName},
	}, store.TemplateReady)
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template: egressTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: 900,
	})

	probe := func(t *testing.T, id, addr, sni, mode string) bool {
		t.Helper()
		res, err := sbx.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"python", "-c", tlsProbe, addr, sni, mode}, TimeoutSeconds: 40}, nil)
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		return res.ExitCode == 0 && strings.Contains(res.Stdout, "reached")
	}
	address := strings.TrimSpace(guestOutput(t, ctx, sbx, sb.ID, "python", "-c", "import socket; print(socket.gethostbyname('"+allowedName+"'))"))

	t.Run("AllowedNameWorks", func(t *testing.T) {
		if !probe(t, sb.ID, allowedName, allowedName, "verify") {
			t.Fatal("the allowed name did not complete a verified handshake")
		}
	})
	t.Run("OtherNameOnTheSameAddress", func(t *testing.T) {
		if probe(t, sb.ID, address, otherName, "any") {
			t.Fatal("another name on the shared address passed")
		}
	})
	t.Run("NoName", func(t *testing.T) {
		if probe(t, sb.ID, address, "", "any") {
			t.Fatal("a connection with no name passed")
		}
	})
	t.Run("DirectAddressIsRefusedAtOnce", func(t *testing.T) {
		if out := guestOutput(t, ctx, sbx, sb.ID, "python", "-c", refusedProbe, "1.1.1.1", "22"); !fastRefusal(out) {
			t.Fatalf("port 22 of a direct address: %q", out)
		}
	})
	t.Run("UDP443IsRefusedAtOnce", func(t *testing.T) {
		if out := guestOutput(t, ctx, sbx, sb.ID, "python", "-c", udpProbe, "1.1.1.1"); !fastRefusal(out) {
			t.Fatalf("UDP 443: %q", out)
		}
	})
	mirror := strings.TrimSpace(guestOutput(t, ctx, sbx, sb.ID, "python", "-c", "import socket; print(socket.gethostbyname('"+mirrorName+"'))"))
	t.Run("Port80AllowedHost", func(t *testing.T) {
		// This is the first file apt-get update reads from the mirror.
		out := guestOutput(t, ctx, sbx, sb.ID, "python", "-c", httpProbe, mirror, mirrorName, "/debian/dists/bookworm/InRelease")
		if !strings.HasPrefix(out, "200 ") {
			t.Fatalf("the allowed mirror on port 80: %q", out)
		}
	})
	t.Run("Port80OtherHostOnTheSameAddress", func(t *testing.T) {
		out := guestOutput(t, ctx, sbx, sb.ID, "python", "-c", httpProbe, mirror, otherName, "/")
		var status int
		var took float64
		if _, err := fmt.Sscanf(out, "%d %f", &status, &took); err != nil || status != 403 || took >= 2 || !strings.Contains(out, "not on the egress allow list") {
			t.Fatalf("another host on the mirror address: %q", out)
		}
	})
	t.Run("Port80WithNoRequest", func(t *testing.T) {
		guestOutput(t, ctx, sbx, sb.ID, "python", "-c", "import socket; socket.create_connection(('1.1.1.1',80),timeout=5).close()")
	})
	t.Run("AllowedNameToAGuestChosenAddress", func(t *testing.T) {
		// The certificate only verifies if the guard dialled the real site and
		// ignored the address the guest named.
		if !probe(t, sb.ID, "1.1.1.1", allowedName, "verify") {
			t.Fatal("the guard did not dial the name it resolved")
		}
	})
	t.Run("NameThatResolvesToMetadata", func(t *testing.T) {
		if probe(t, sb.ID, address, metadataName, "any") {
			t.Fatal("a name that resolves to the metadata address passed")
		}
	})

	nm.Flush()
	t.Run("Record", func(t *testing.T) {
		rows := waitRecord(t, ctx, sbx, sb.ID, func(rows []store.EgressRecord) bool {
			return hasRow(rows, otherName, 443, "denied") && hasRow(rows, "1.1.1.1", 80, "denied") && hasRow(rows, "1.1.1.1", 443, "denied") &&
				hasRow(rows, metadataName, 443, "denied") && hasRow(rows, "1.1.1.1", 22, "denied") && hasRow(rows, otherName, 80, "denied") &&
				hasRow(rows, mirrorName, 80, "allowed")
		})
		for _, want := range []struct {
			host string
			port int
		}{{allowedName, 443}, {mirrorName, 80}} {
			allowed := rowOf(rows, want.host, want.port, "allowed")
			if allowed == nil || allowed.Count < 1 || allowed.BytesOut == 0 || allowed.BytesIn == 0 {
				t.Errorf("the allowed row of %s:%d is %+v", want.host, want.port, allowed)
			}
		}
		// An address names its port in the event, so a reader tells 80 from 22.
		events, err := st.ListEventsSince(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		reasons := map[string]bool{}
		for _, e := range events {
			if e.SandboxID == sb.ID {
				reasons[e.Reason] = true
			}
		}
		for _, want := range []string{"1.1.1.1:22", "1.1.1.1:80", "1.1.1.1:443"} {
			if !reasons[sandbox.EventEgressDenied+" "+want] {
				t.Errorf("no event names %s: %v", want, reasons)
			}
		}
	})

	t.Run("OneEventPerDestination", func(t *testing.T) {
		last, err := st.LastEventID(ctx)
		if err != nil {
			t.Fatal(err)
		}
		code := "import socket\nfor i in range(100):\n    try: socket.gethostbyname('refused.example.org')\n    except OSError: pass\n"
		guestOutput(t, ctx, sbx, sb.ID, "python", "-c", code)
		waitRecord(t, ctx, sbx, sb.ID, func(rows []store.EgressRecord) bool { return hasRow(rows, "refused.example.org", 0, "denied") })
		events, err := st.ListEventsSince(ctx, last, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, e := range events {
			if e.SandboxID == sb.ID && e.Reason == sandbox.EventEgressDenied+" refused.example.org" {
				seen++
			}
		}
		if seen != 1 {
			t.Errorf("%d events for 100 refusals", seen)
		}
	})

	t.Run("ForkStartsEmpty", func(t *testing.T) {
		copies, err := sbx.Fork(ctx, sb.ID, 1, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sbx.Destroy(context.WithoutCancel(ctx), copies[0].ID)
		record, err := sbx.EgressLog(ctx, copies[0].ID)
		if err != nil || len(record.Records) != 0 {
			t.Errorf("the fork has %+v, %v", record, err)
		}
	})

	t.Run("KilledGuardFailsClosedAndReturns", func(t *testing.T) {
		last, err := st.LastEventID(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pid := guardPID(t, root, sb.ID)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for !probe(t, sb.ID, allowedName, allowedName, "verify") {
			if time.Now().After(deadline) {
				t.Fatal("egress did not return after the guard died")
			}
			time.Sleep(time.Second)
		}
		if guardPID(t, root, sb.ID) == pid {
			t.Error("the guard has the same pid")
		}
		events, err := st.ListEventsSince(ctx, last, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range events {
			found = found || (e.SandboxID == sb.ID && e.Reason == "egress guard exited")
		}
		if !found {
			t.Error("no event records the exit")
		}
	})

	t.Run("ConnectionSurvivesARestart", func(t *testing.T) {
		held, err := sbx.StartProcess(ctx, sb.ID, runtime.ProcessRequest{Cmd: []string{"python", "-c", holdConnection}, Cwd: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		waitProcessOutput(t, ctx, sbx, sb.ID, held, "connected")
		before := guardPID(t, root, sb.ID)
		restartedNet := network.New(kilnBin, root)
		restarted := newManager(restartedNet, nil)
		sweep := &reconcile.Reconciler{Root: root, Store: st, Sandboxes: restarted, Network: restartedNet, Templates: tmgr, Runtime: rt}
		if _, err := sweep.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if after := guardPID(t, root, sb.ID); after != before {
			t.Fatalf("the restart replaced the guard: %d then %d", before, after)
		}
		if err := restarted.WriteFile(ctx, sb.ID, "/tmp/go", strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		waitProcessOutput(t, ctx, restarted, sb.ID, held, "answered")
		// The rest of this test goes on with the manager that holds the link.
		sbx, nm = restarted, restartedNet
	})

	t.Run("Observe", func(t *testing.T) {
		if _, err := sbx.ObserveEgress(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		if !probe(t, sb.ID, otherName, otherName, "verify") {
			t.Fatal("observe refused a public site")
		}
		// Trusted code still must not reach the host network.
		if probe(t, sb.ID, address, metadataName, "any") {
			t.Fatal("observe reached the metadata address")
		}
		waitRecord(t, ctx, sbx, sb.ID, func(rows []store.EgressRecord) bool { return hasRow(rows, otherName, 443, "observed") })
		if _, err := sbx.Fork(ctx, sb.ID, 1, false, nil, nil); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a fork while observing: %v", err)
		}
		if _, err := sbx.Snapshot(ctx, sb.ID, false); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a snapshot while observing: %v", err)
		}
		// Sending a list ends it.
		if row, err := sbx.SetEgress(ctx, sb.ID, []string{allowedName}); err != nil || row.EgressObserve {
			t.Fatalf("ending observe: %+v %v", row, err)
		}
		if probe(t, sb.ID, address, otherName, "any") {
			t.Error("the list did not end observe")
		}
	})

	t.Run("ObserveRefusesEnvValues", func(t *testing.T) {
		bearing := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
			Template: egressTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: 900,
			Env: map[string]string{"KEY": "value"},
		})
		defer sbx.Destroy(context.WithoutCancel(ctx), bearing.ID)
		if _, err := sbx.ObserveEgress(ctx, bearing.ID); !errors.Is(err, store.ErrConflict) {
			t.Errorf("observe with env: %v", err)
		}
	})

	t.Run("RecordOutlivesTheSandbox", func(t *testing.T) {
		if err := sbx.Destroy(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		record, err := sbx.EgressLog(ctx, sb.ID)
		if err != nil || len(record.Records) == 0 {
			t.Fatalf("after the destroy: %+v %v", record, err)
		}
		later := newManager(network.New(kilnBin, root), func() time.Time { return time.Now().Add(8 * 24 * time.Hour) })
		if _, err := later.EgressLog(ctx, sb.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("after 8 days: %v", err)
		}
	})
}

func waitRecord(t *testing.T, ctx context.Context, sbx *sandbox.Manager, id string, done func([]store.EgressRecord) bool) []store.EgressRecord {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		record, err := sbx.EgressLog(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if done(record.Records) {
			return record.Records
		}
		if time.Now().After(deadline) {
			t.Fatalf("the egress record never showed the row: %+v", record.Records)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func rowOf(rows []store.EgressRecord, host string, port int, decision string) *store.EgressRecord {
	for i := range rows {
		if rows[i].Host == host && rows[i].Port == port && rows[i].Decision == decision {
			return &rows[i]
		}
	}
	return nil
}

func hasRow(rows []store.EgressRecord, host string, port int, decision string) bool {
	return rowOf(rows, host, port, decision) != nil
}

// guardPID finds the guard of one sandbox by its socket.
func guardPID(t *testing.T, root, id string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	sock := network.GuardSocket(root, network.ShortID(id))
	for {
		out, _ := exec.Command("pgrep", "-f", "egressguard --sock "+sock).Output()
		var pid int
		if _, err := fmt.Sscan(string(out), &pid); err == nil {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no guard for %s", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
