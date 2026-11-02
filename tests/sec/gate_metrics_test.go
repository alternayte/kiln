//go:build kvm

package secgate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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

const metricsTemplate = "sec-metrics"

func TestSecMetrics(t *testing.T) {
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
		Root: root, Store: st, Runtime: rt, Network: nm, Zone: "metrics.test",
		Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
		KernelPath: kernel,
	})
	destroyAll := func() {
		clean := context.WithoutCancel(ctx)
		rows, _ := st.ListSandboxes(clean)
		for _, row := range rows {
			if row.TemplateName == metricsTemplate && row.DestroyedAt == nil {
				_ = sbx.Destroy(clean, row.ID)
			}
		}
		_ = tmgr.Delete(clean, metricsTemplate)
	}
	destroyAll()
	t.Cleanup(destroyAll)
	build(t, ctx, tmgr, template.BuildRequest{
		Name: metricsTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 256, DiskMB: 2048,
		EgressAllow: []string{"example.com"},
	}, store.TemplateReady)

	host := httptest.NewServer((&api.Server{Store: st, Sandboxes: sbx, Templates: tmgr, Token: "operator", Root: root, Base: ctx}).Handler())
	defer host.Close()
	scrape := func(t *testing.T) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, host.URL+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer operator")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("scrape: %d %s", resp.StatusCode, body)
		}
		return string(body)
	}
	value := func(t *testing.T, text, series string) int {
		t.Helper()
		found := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\d+)$`).FindStringSubmatch(text)
		if found == nil {
			t.Fatalf("no %s in:\n%s", series, text)
		}
		n, _ := strconv.Atoi(found[1])
		return n
	}
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{Template: metricsTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: 900})

	t.Run("NoToken", func(t *testing.T) {
		resp, err := http.Get(host.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status %d", resp.StatusCode)
		}
	})

	t.Run("RunningCountEqualsTheList", func(t *testing.T) {
		rows, err := sbx.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		running := 0
		for _, row := range rows {
			if row.DestroyedAt == nil && row.State == store.SandboxRunning {
				running++
			}
		}
		if got := value(t, scrape(t), `kiln_sandboxes{state="running"}`); got != running {
			t.Errorf("metrics %d, list %d", got, running)
		}
	})

	t.Run("ForkAddsOneSample", func(t *testing.T) {
		before := value(t, scrape(t), "kiln_fork_duration_seconds_count")
		copies, err := sbx.Fork(ctx, sb.ID, 1, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sbx.Destroy(context.WithoutCancel(ctx), copies[0].ID)
		if got := value(t, scrape(t), "kiln_fork_duration_seconds_count"); got != before+1 {
			t.Errorf("fork count %d, want %d", got, before+1)
		}
	})

	t.Run("KilledGuardAddsOneRestart", func(t *testing.T) {
		before := value(t, scrape(t), "kiln_egress_guard_restarts_total")
		pid := guardPID(t, root, sb.ID)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for value(t, scrape(t), "kiln_egress_guard_restarts_total") != before+1 {
			if time.Now().After(deadline) {
				t.Fatal("the restart count did not move")
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	t.Run("SeriesCountStays", func(t *testing.T) {
		series := func(text string) int {
			n := 0
			for _, line := range strings.Split(text, "\n") {
				if line != "" && !strings.HasPrefix(line, "#") {
					n++
				}
			}
			return n
		}
		before := series(scrape(t))
		for i := 0; i < 100; i++ {
			row, err := sbx.Create(ctx, sandbox.CreateRequest{Template: metricsTemplate, Lifecycle: store.LifecycleEphemeral, IdleSeconds: 60})
			if err != nil {
				t.Fatalf("create %d: %v", i, err)
			}
			if err := sbx.Destroy(ctx, row.ID); err != nil {
				t.Fatalf("destroy %d: %v", i, err)
			}
		}
		text := scrape(t)
		if got := series(text); got != before {
			t.Errorf("series %d, was %d", got, before)
		}
		if strings.Contains(text, sb.ID) || strings.Contains(text, metricsTemplate) {
			t.Error("the output names a sandbox or a template")
		}
	})
}
