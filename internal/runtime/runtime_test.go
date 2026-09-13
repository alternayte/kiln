package runtime

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/alternayte/kiln/internal/guestproto"
)

func TestNormalizeExecDefaults(t *testing.T) {
	req := guestproto.Request{Op: guestproto.OpExec, Cmd: []string{"echo"}}
	if e := guestproto.NormalizeExec(&req); e != nil {
		t.Fatalf("NormalizeExec: %v", e)
	}
	if req.Cwd != "/" {
		t.Fatalf("cwd %q", req.Cwd)
	}
	if req.TimeoutSeconds != guestproto.ExecTimeoutDefault {
		t.Fatalf("timeout %d", req.TimeoutSeconds)
	}
}

func TestExecRejectsInvalid(t *testing.T) {
	vm := &VM{}
	cases := map[string]ExecRequest{
		"no cmd":   {},
		"timeout":  {Cmd: []string{"echo"}, TimeoutSeconds: guestproto.ExecTimeoutMax + 1},
		"negative": {Cmd: []string{"echo"}, TimeoutSeconds: -1},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := vm.Exec(context.Background(), req)
			if !IsExecError(err, guestproto.CodeInvalid) {
				t.Fatalf("err %v, want an invalid ExecError", err)
			}
		})
	}
}

// apiRecorder serves a fake Firecracker API over a unix socket and records
// every request.
type apiRecorder struct {
	mu      sync.Mutex
	paths   []string
	bodies  []string
	replies map[string]int
}

func (r *apiRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.paths = append(r.paths, req.Method+" "+req.URL.Path)
		r.bodies = append(r.bodies, string(b))
		code := http.StatusNoContent
		if r.replies != nil {
			if c, ok := r.replies[req.URL.Path]; ok {
				code = c
			}
		}
		r.mu.Unlock()
		w.WriteHeader(code)
	})
}

func TestConfigureAPIRequests(t *testing.T) {
	rec := &apiRecorder{}
	sock := serveAPI(t, rec)
	vm := &VM{Socket: sock}
	f := &Firecracker{}
	spec := Spec{
		ID:             "p1-api",
		KernelPath:     "/kernel",
		RootfsPath:     "/rootfs",
		RootfsReadOnly: true,
		VCPUs:          2,
		MemoryMiB:      512,
		VsockCID:       3,
		VsockPort:      52,
	}
	if err := f.configure(context.Background(), spec, vm); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /boot-source",
		"PUT /drives/rootfs",
		"PUT /machine-config",
		"PUT /vsock",
		"PUT /actions",
	}
	if len(rec.paths) != len(want) {
		t.Fatalf("requests %v, want %v", rec.paths, want)
	}
	for i := range want {
		if rec.paths[i] != want[i] {
			t.Fatalf("request %d %q, want %q", i, rec.paths[i], want[i])
		}
	}
	joined := strings.Join(rec.bodies, "\n")
	for _, want := range []string{
		`"/vmlinux"`,
		`root=/dev/vda ro`,
		`"path_on_host":"/rootfs.ext4"`,
		`"is_read_only":true`,
		`"vcpu_count":2`,
		`"guest_cid":3`,
		`"action_type":"InstanceStart"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("bodies do not contain %q:\n%s", want, joined)
		}
	}
}

func TestAPISurfacesFaultMessage(t *testing.T) {
	sock := serveAPIHandler(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"fault_message":"bad drive"}`))
	}))
	a := newAPI(sock)
	err := a.put(context.Background(), "/drives/rootfs", map[string]string{"drive_id": "rootfs"})
	if err == nil || !strings.Contains(err.Error(), "bad drive") {
		t.Fatalf("err %v, want the fault message", err)
	}
}

func TestStartRejectsUnsafeID(t *testing.T) {
	f := &Firecracker{root: t.TempDir()}
	_, err := f.Start(context.Background(), Spec{ID: "../escape", KernelPath: "/k", RootfsPath: "/r"})
	if err == nil {
		t.Fatal("Start accepted an unsafe id")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err %v, want invalid", err)
	}
}

func serveAPI(t *testing.T, rec *apiRecorder) string {
	t.Helper()
	return serveAPIHandler(t, rec.handler())
}

func serveAPIHandler(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := t.TempDir() + "/fc.sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}
