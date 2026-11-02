//go:build kvm

package secgate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/api"
	"github.com/alternayte/kiln/internal/ingress"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

const (
	viewerForkTemplate = "sec-viewer-fork"
	viewerForkPassword = "gate-viewer-fork-pass-1"
	viewerForkIdle     = 30
)

func TestSecViewerFork(t *testing.T) {
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
		Root: root, Store: st, Runtime: rt, Network: nm, Zone: zone,
		Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
		KernelPath: kernel,
	})
	destroyAll := func() {
		clean := context.WithoutCancel(ctx)
		rows, _ := st.ListSandboxes(clean)
		for _, row := range rows {
			if row.TemplateName == viewerForkTemplate && row.DestroyedAt == nil {
				_ = sbx.Destroy(clean, row.ID)
			}
		}
		_ = tmgr.Delete(clean, viewerForkTemplate)
	}
	destroyAll()
	t.Cleanup(destroyAll)
	build(t, ctx, tmgr, template.BuildRequest{
		Name: viewerForkTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 256, DiskMB: 2048,
	}, store.TemplateReady)

	auth := newAuth(t, ctx)
	preview := newPreview(t, ctx, st, sbx, auth)
	control := httptest.NewServer((&api.Server{Store: st, Sandboxes: sbx, Templates: tmgr, Token: "operator", Root: root, Base: ctx}).Handler())
	defer control.Close()
	call := func(t *testing.T, method, path string, body any) (int, string) {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, control.URL+path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer operator")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	viewer := func(t *testing.T, host, email string) string {
		t.Helper()
		if err := ingress.CreateViewer(ctx, auth, email, viewerForkPassword); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(map[string]string{"email": email, "password": viewerForkPassword})
		req, _ := http.NewRequest(http.MethodPost, preview.URL+"/_kiln/auth/sign-in/email", bytes.NewReader(data))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		resp, err := preview.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("sign-in of %s: %d", email, resp.StatusCode)
		}
		pair, _, _ := strings.Cut(resp.Header.Get("Set-Cookie"), ";")
		return pair
	}

	origin := createSandbox(t, ctx, sbx, sandbox.CreateRequest{Template: viewerForkTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: viewerForkIdle})
	startGuestServer(t, ctx, sbx, origin.ID)

	t.Run("Validation", func(t *testing.T) {
		path := "/v1/sandboxes/" + origin.ID + "/publish"
		if status, body := call(t, http.MethodPost, path, map[string]any{"port": 7000, "visibility": "public", "viewer_fork": true, "viewer_cap": 2}); status != http.StatusBadRequest {
			t.Errorf("public: %d %s", status, body)
		}
		if status, body := call(t, http.MethodPost, path, map[string]any{"port": 7000, "visibility": "team", "viewer_fork": true}); status != http.StatusBadRequest {
			t.Errorf("no cap: %d %s", status, body)
		}
		bearing := createSandbox(t, ctx, sbx, sandbox.CreateRequest{Template: viewerForkTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: 300, Env: map[string]string{"HELD": "value"}})
		defer sbx.Destroy(context.WithoutCancel(ctx), bearing.ID)
		if status, body := call(t, http.MethodPost, "/v1/sandboxes/"+bearing.ID+"/publish", map[string]any{"port": 7000, "visibility": "team", "viewer_fork": true, "viewer_cap": 2}); status != http.StatusConflict {
			t.Errorf("secret-bearing origin: %d %s", status, body)
		}
	})

	status, body := call(t, http.MethodPost, "/v1/sandboxes/"+origin.ID+"/publish", map[string]any{"port": 7000, "visibility": "team", "viewer_fork": true, "viewer_cap": 2})
	if status != http.StatusCreated {
		t.Fatalf("publish: %d %s", status, body)
	}
	row, err := st.GetPublished(ctx, origin.ID, 7000)
	if err != nil {
		t.Fatal(err)
	}
	host := publicHost(row.Subdomain)
	if status, body := call(t, http.MethodPost, "/v1/sandboxes/"+origin.ID+"/publish", map[string]any{"port": 7001, "visibility": "team"}); status != http.StatusConflict {
		t.Errorf("a plain publish of the origin: %d %s", status, body)
	}
	if status, body := call(t, http.MethodPost, "/v1/sandboxes/"+origin.ID+"/publish", map[string]any{"port": 7001, "visibility": "team", "viewer_fork": true, "viewer_cap": 2}); status != http.StatusCreated {
		t.Fatalf("second port: %d %s", status, body)
	}
	second, err := st.GetPublished(ctx, origin.ID, 7001)
	if err != nil {
		t.Fatal(err)
	}
	countBefore := guestRequestCount(t, ctx, sbx, origin.ID)

	cookieA := viewer(t, host, "viewer-a@example.invalid")
	cookieB := viewer(t, host, "viewer-b@example.invalid")
	cookieC := viewer(t, host, "viewer-c@example.invalid")
	forkOf := func(t *testing.T, cookieIndex int) string {
		t.Helper()
		forks, err := sbx.ViewerForks(ctx, origin.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cookieIndex >= len(forks) {
			t.Fatalf("forks: %+v", forks)
		}
		return forks[cookieIndex].ForkID
	}
	visit := func(t *testing.T, h, cookie string) {
		t.Helper()
		if status, _, body := fetch(t, preview, h, "/", cookie); status != http.StatusOK {
			t.Fatalf("visit of %s: %d %s", h, status, body)
		}
	}

	t.Run("NoLogin", func(t *testing.T) {
		if status, _, _ := fetch(t, preview, host, "/", ""); status != http.StatusUnauthorized {
			t.Errorf("status %d", status)
		}
	})

	visit(t, host, cookieA)
	visit(t, host, cookieB)
	forks, err := sbx.ViewerForks(ctx, origin.ID)
	if err != nil || len(forks) != 2 {
		t.Fatalf("forks %+v, %v", forks, err)
	}
	marked := forks[0]
	first := cookieA
	if strings.Contains(viewerIDOf(t, preview, host, cookieB), marked.ViewerID) {
		first = cookieB
	}

	t.Run("OwnCopy", func(t *testing.T) {
		if err := sbx.WriteFile(ctx, marked.ForkID, "/tmp/viewer-row", strings.NewReader("row")); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{forkOf(t, 1), origin.ID} {
			res, err := sbx.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"test", "-e", "/tmp/viewer-row"}, TimeoutSeconds: 20}, nil)
			if err != nil || res.ExitCode == 0 {
				t.Errorf("%s holds the row: %+v %v", id, res, err)
			}
		}
		if got := forkOf(t, 0); got != marked.ForkID {
			t.Errorf("fork of the viewer moved from %s to %s", marked.ForkID, got)
		}
	})

	t.Run("OneForkAcrossPorts", func(t *testing.T) {
		visit(t, publicHost(second.Subdomain), first)
		if got, _ := sbx.ViewerForks(ctx, origin.ID); len(got) != 2 {
			t.Errorf("forks: %+v", got)
		}
	})

	t.Run("CapPage", func(t *testing.T) {
		status, header, body := fetch(t, preview, host, "/", cookieC)
		if status != http.StatusServiceUnavailable || !strings.Contains(body, "limit of 2") {
			t.Errorf("status %d, %v: %s", status, header, body)
		}
	})

	t.Run("OriginNotTouched", func(t *testing.T) {
		if got := guestRequestCount(t, ctx, sbx, origin.ID); got != countBefore {
			t.Errorf("origin saw %d requests, had %d", got, countBefore)
		}
	})

	t.Run("Reset", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, preview.URL+"/_kiln/fork/reset", nil)
		req.Host = host
		req.Header.Set("Cookie", first)
		req.Header.Set("Origin", "null")
		resp, err := preview.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("a guest page's reset: %d", resp.StatusCode)
		}
		req, _ = http.NewRequest(http.MethodPost, preview.URL+"/_kiln/fork/reset", nil)
		req.Host = host
		req.Header.Set("Cookie", first)
		req.Header.Set("Origin", "https://"+host)
		client := *preview.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if resp, err = client.Do(req); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("reset: %d", resp.StatusCode)
		}
		visit(t, host, first)
		for _, fork := range mustForks(t, ctx, sbx, origin.ID) {
			if fork.ForkID == marked.ForkID {
				t.Fatal("the reset kept the fork")
			}
			if fork.ViewerID == marked.ViewerID {
				res, err := sbx.Exec(ctx, fork.ForkID, runtime.ExecRequest{Cmd: []string{"test", "-e", "/tmp/viewer-row"}, TimeoutSeconds: 20}, nil)
				if err != nil || res.ExitCode == 0 {
					t.Errorf("the reset fork holds the row: %+v %v", res, err)
				}
			}
		}
	})

	t.Run("IdleForkEndsAndOriginSleeps", func(t *testing.T) {
		deadline := time.Now().Add(3 * viewerForkIdle * time.Second)
		for len(mustForks(t, ctx, sbx, origin.ID)) > 0 {
			if time.Now().After(deadline) {
				t.Fatalf("forks remain: %+v", mustForks(t, ctx, sbx, origin.ID))
			}
			time.Sleep(time.Second)
		}
		waitState(t, ctx, st, origin.ID, store.SandboxSleeping, 3*viewerForkIdle*time.Second)
		visit(t, host, cookieA)
		now, err := st.GetSandbox(ctx, origin.ID)
		if err != nil || now.State != store.SandboxSleeping {
			t.Errorf("a visit moved the origin: %+v %v", now.State, err)
		}
	})

	t.Run("DestroyLeavesNothing", func(t *testing.T) {
		forks := mustForks(t, ctx, sbx, origin.ID)
		held, err := st.GetViewerOrigin(ctx, origin.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status, body := call(t, http.MethodDelete, "/v1/sandboxes/"+origin.ID, nil); status/100 != 2 {
			t.Fatalf("delete: %d %s", status, body)
		}
		for _, fork := range forks {
			row, err := st.GetSandbox(ctx, fork.ForkID)
			if err != nil || row.DestroyedAt == nil {
				t.Errorf("fork %s: %+v %v", fork.ForkID, row.State, err)
			}
		}
		if _, err := st.GetSnapshot(ctx, held.SnapshotID); err == nil {
			t.Error("the snapshot remains")
		}
		if _, err := os.Stat(filepath.Join(root, "snapshots", held.SnapshotID)); err == nil {
			t.Error("the snapshot directory remains")
		}
	})
}

func mustForks(t *testing.T, ctx context.Context, sbx *sandbox.Manager, origin string) []store.ViewerFork {
	t.Helper()
	forks, err := sbx.ViewerForks(ctx, origin)
	if err != nil {
		t.Fatal(err)
	}
	return forks
}

// viewerIDOf reads the viewer login behind one cookie from the session route.
func viewerIDOf(t *testing.T, ts *httptest.Server, host, cookie string) string {
	t.Helper()
	status, _, body := fetch(t, ts, host, "/_kiln/auth/session", cookie)
	if status != http.StatusOK {
		t.Fatalf("session: %d %s", status, body)
	}
	return body
}
