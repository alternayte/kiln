//go:build kvm

package secgate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

const dnsTemplate = "sec-dns"

// dnsFromPort asks the resolver for one allowed name from one source port.
const dnsFromPort = `import socket,sys
q=b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01"
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("",int(sys.argv[1])))
s.settimeout(2)
for attempt in range(3):
    s.sendto(q,(sys.argv[2],53))
    try:
        print(s.recv(4096)[3]&15)
        sys.exit(0)
    except socket.timeout:
        pass
print("no answer")
`

// TestSecDNSSharedSourcePort asks from one source port in two sandboxes. Every
// guest has one address and one gateway, and copies of one image can pick the
// same port, so this tuple repeats on a real host.
func TestSecDNSSharedSourcePort(t *testing.T) {
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
	builder := template.Builder{KilninitPath: filepath.Join(root, "bin", "kilninit")}
	tmgr := &template.Manager{Root: root, Store: st, Runtime: rt, Network: nm, Builder: builder, KernelPath: kernel}
	sbx := sandbox.New(sandbox.Config{
		Root: root, Store: st, Runtime: rt, Network: nm,
		Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
		KernelPath: kernel,
	})
	destroyAll := func() {
		clean := context.WithoutCancel(ctx)
		rows, _ := st.ListSandboxes(clean)
		for _, row := range rows {
			if row.TemplateName == dnsTemplate && row.DestroyedAt == nil {
				_ = sbx.Destroy(clean, row.ID)
			}
		}
		_ = tmgr.Delete(clean, dnsTemplate)
	}
	destroyAll()
	t.Cleanup(destroyAll)
	build(t, ctx, tmgr, template.BuildRequest{
		Name: dnsTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 512, DiskMB: 2048,
		EgressAllow: []string{"example.com"},
	}, store.TemplateReady)
	ask := func(id string, port int) string {
		res, err := sbx.Exec(ctx, id, runtime.ExecRequest{
			Cmd: []string{"python", "-c", dnsFromPort, fmt.Sprint(port), runtime.GuestGateway}, TimeoutSeconds: 30,
		}, nil)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("ask: %+v %v", res, err)
		}
		return strings.TrimSpace(res.Stdout)
	}
	report := func(t *testing.T) {
		out, _ := exec.Command("conntrack", "-L", "-p", "udp", "--orig-port-dst", "53").CombinedOutput()
		t.Logf("conntrack:\n%s", out)
	}
	first := createSandbox(t, ctx, sbx, sandbox.CreateRequest{Template: dnsTemplate, Lifecycle: store.LifecycleEphemeral, IdleSeconds: 900})
	second := createSandbox(t, ctx, sbx, sandbox.CreateRequest{Template: dnsTemplate, Lifecycle: store.LifecycleEphemeral, IdleSeconds: 900})
	t.Run("TwoLiveSandboxes", func(t *testing.T) {
		if got := ask(first.ID, 40000); got != "0" {
			report(t)
			t.Fatalf("the first sandbox: %s", got)
		}
		if got := ask(second.ID, 40000); got != "0" {
			report(t)
			t.Errorf("the second sandbox on the same port: %s", got)
		}
		if got := ask(first.ID, 40000); got != "0" {
			report(t)
			t.Errorf("the first sandbox again: %s", got)
		}
	})
	// The log says how often copies of one image pick one port by themselves.
	t.Run("PortsOfCopies", func(t *testing.T) {
		copies, err := sbx.Fork(ctx, second.ID, 3, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range copies {
			res, err := sbx.Exec(ctx, c.ID, runtime.ExecRequest{
				Cmd: []string{"python", "-c", "import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.connect(('172.31.0.1',53));print(s.getsockname()[1])"}, TimeoutSeconds: 30,
			}, nil)
			t.Logf("copy %s port %s %v", c.ID, strings.TrimSpace(res.Stdout), err)
			if err := sbx.Destroy(ctx, c.ID); err != nil {
				t.Error(err)
			}
		}
	})
	t.Run("AfterADestroy", func(t *testing.T) {
		if got := ask(first.ID, 40001); got != "0" {
			t.Fatalf("the first sandbox: %s", got)
		}
		if err := sbx.Destroy(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		if got := ask(second.ID, 40001); got != "0" {
			report(t)
			t.Errorf("a port a destroyed sandbox used: %s", got)
		}
	})
}
