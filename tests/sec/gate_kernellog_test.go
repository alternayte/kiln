//go:build kvm

package secgate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/api"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
)

// oomLine is what the guest kernel prints when it kills a process for memory.
const oomLine = "Out of memory: Killed process"

// C10: a process that allocates past the guest memory is killed, and the
// kernel log route shows the kill. A fork starts a new VM with no lines of its
// source, and a destroyed sandbox answers 404.
func checkKernelLog(t *testing.T, ctx context.Context, st store.Store, sbx *sandbox.Manager, root string) {
	t.Helper()
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template: templateName, Lifecycle: store.LifecycleEphemeral, IdleSeconds: 900,
	})
	t.Cleanup(func() { _ = sbx.Destroy(context.WithoutCancel(ctx), sb.ID) })
	control := httptest.NewServer((&api.Server{Store: st, Sandboxes: sbx, Token: "operator", Root: root, Base: ctx}).Handler())
	defer control.Close()
	read := func(id string) (int, []string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, control.URL+"/v1/sandboxes/"+id+"/console?lines=2000", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer operator")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil
		}
		var body struct {
			Lines []string `json:"lines"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("kernel log %q: %v", raw, err)
		}
		return resp.StatusCode, body.Lines
	}
	killed := func(lines []string) string {
		for _, line := range lines {
			if strings.Contains(line, oomLine) && strings.Contains(line, "(python") {
				return line
			}
		}
		return ""
	}

	// The template has 512 MiB. One allocation larger than the guest memory
	// fails at once with MemoryError, and the kernel kills nothing. Steps of
	// 64 MiB each pass the overcommit check, and the bytes are written, so
	// the guest runs out of memory and the kernel kills the process.
	grow := "b = []\nwhile True: b.append(b'x' * (64 << 20))"
	res, err := sbx.Exec(ctx, sb.ID, runtime.ExecRequest{Cmd: []string{"python", "-c", grow}, TimeoutSeconds: 120}, nil)
	if err != nil {
		t.Fatalf("exec the allocation: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("an allocation past the guest memory exited 0")
	}
	var lines []string
	deadline := time.Now().Add(10 * time.Second)
	for {
		var code int
		code, lines = read(sb.ID)
		if code != http.StatusOK {
			t.Fatalf("kernel log: status %d", code)
		}
		if killed(lines) != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if killed(lines) == "" {
		t.Fatalf("kernel log has no %q line for python; last lines:\n%s", oomLine, strings.Join(lines[max(len(lines)-30, 0):], "\n"))
	}

	copies, err := sbx.Fork(ctx, sb.ID, 1, false, nil, nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	fork := copies[0]
	if code, lines := read(fork.ID); code != http.StatusOK || killed(lines) != "" {
		t.Fatalf("fork kernel log: status %d, kill line %q, want 200 and none", code, killed(lines))
	}
	if err := sbx.Destroy(ctx, fork.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := read(fork.ID); code != http.StatusNotFound {
		t.Fatalf("destroyed fork kernel log: status %d, want 404", code)
	}
}
