//go:build kvm

package servicesgate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alternayte/kiln/client"
	"github.com/alternayte/kiln/internal/runtime"
)

const (
	stackTemplate = "gate-services"
	psql          = "psql -h postgres -U postgres -tA -c "
)

// TestGateServices builds a real Postgres beside an application image and
// follows docs/specs/template-services.md. The application image is Postgres
// too, because it carries the psql client.
func TestGateServices(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate services needs root: start it with sudo")
	}
	root := os.Getenv("KILN_ROOT")
	if root == "" {
		root = "/var/lib/kiln"
	}
	var cfg struct {
		BearerToken string `json:"bearer_token"`
		ControlAddr string `json:"control_addr"`
	}
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatalf("config.json: %v (run kilnd init)", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	repo, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "kilnd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/kilnd")
	build.Dir = strings.TrimSpace(string(repo))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build kilnd: %v: %s", err, out)
	}
	daemon := exec.Command(binary, "serve")
	daemon.Env = append(os.Environ(), "KILN_ROOT="+root)
	logPath := filepath.Join(t.TempDir(), "serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	daemon.Stdout, daemon.Stderr = logFile, logFile
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.Process.Signal(syscall.SIGTERM)
		daemon.Wait()
		if t.Failed() {
			if out, err := os.ReadFile(logPath); err == nil {
				t.Logf("kilnd log:\n%s", out)
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	cli := client.New("http://"+cfg.ControlAddr, cfg.BearerToken)
	for {
		if health, err := cli.Health(ctx); err == nil && health.OK {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the daemon did not become healthy")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() { clearAll(t, cli) })
	clearAll(t, cli)

	postgres := client.TemplateService{
		Name: "postgres", Image: "docker.io/library/postgres:16-alpine", Port: 5432,
		Ready: []string{"pg_isready"},
		Env:   map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"},
	}
	if err := cli.CreateTemplate(ctx, client.TemplateRequest{
		Name: stackTemplate, Image: "docker.io/library/postgres:16-alpine",
		VCPUs: 2, MemoryMB: 1024, DiskMB: 4096, EgressAllow: []string{},
		Services: []client.TemplateService{postgres},
		Seed: []string{
			psql + `"create table rows (v text)"`,
			psql + `"insert into rows values ('seeded')"`,
		},
	}); err != nil {
		t.Fatal(err)
	}
	waitTemplate(t, ctx, cli, stackTemplate, "ready")

	idle := 3600
	var origin string
	t.Run("RestoresWithTheStackRunning", func(t *testing.T) {
		began := time.Now()
		sandbox, err := cli.CreateSandbox(ctx, client.SandboxRequest{Template: stackTemplate, Lifecycle: "persistent", IdleSeconds: idle})
		if err != nil {
			t.Fatal(err)
		}
		origin = sandbox.ID
		t.Logf("a create took %s", time.Since(began))
		if got := guest(t, ctx, cli, origin, psql+`"select v from rows"`); got != "seeded" {
			t.Fatalf("rows = %q", got)
		}
		if got := guest(t, ctx, cli, origin, "getent hosts postgres"); !strings.HasPrefix(got, "127.0.0.1") {
			t.Fatalf("getent hosts postgres = %q", got)
		}
		// One postmaster, and it started before this sandbox existed.
		if got := guest(t, ctx, cli, origin, "pgrep -x postgres -o | wc -l"); got != "1" {
			t.Fatalf("oldest postgres count = %q", got)
		}
	})

	t.Run("ForksKeepTheirOwnData", func(t *testing.T) {
		guest(t, ctx, cli, origin, psql+`"insert into rows values ('pre-fork')"`)
		before := memoryEvents(t, origin)
		copies, err := cli.Fork(ctx, origin, 3, false)
		if err != nil {
			t.Fatal(err)
		}
		assertSnapshotUnderLimit(t, origin, 1024, before)
		// Dropping the page cache makes the guest read its drive. A fork on a
		// blank drive passes without this, because the cache holds the file.
		for _, fork := range copies {
			guest(t, ctx, cli, fork.ID, "sync; echo 3 > /proc/sys/vm/drop_caches")
			if got := guest(t, ctx, cli, fork.ID, psql+`"select count(*) from rows where v = 'pre-fork'"`); got != "1" {
				t.Fatalf("fork %s holds %q pre-fork rows", fork.ID, got)
			}
		}
		guest(t, ctx, cli, copies[0].ID, psql+`"insert into rows values ('only-one')"`)
		for _, fork := range copies[1:] {
			if got := guest(t, ctx, cli, fork.ID, psql+`"select count(*) from rows where v = 'only-one'"`); got != "0" {
				t.Fatalf("a row crossed to fork %s: %q", fork.ID, got)
			}
		}
		if got := guest(t, ctx, cli, origin, psql+`"select count(*) from rows where v = 'only-one'"`); got != "0" {
			t.Fatalf("a row crossed to the origin: %q", got)
		}
		for _, fork := range copies {
			if err := cli.DeleteSandbox(ctx, fork.ID); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("SleepKeepsTheRows", func(t *testing.T) {
		guest(t, ctx, cli, origin, psql+`"insert into rows values ('before-sleep')"`)
		if _, err := cli.Snapshot(ctx, origin, true); err != nil {
			t.Fatal(err)
		}
		if got := guest(t, ctx, cli, origin, psql+`"select count(*) from rows where v = 'before-sleep'"`); got != "1" {
			t.Fatalf("rows after a wake = %q", got)
		}
	})

	t.Run("AServiceExitIsRecordedAndNotRestarted", func(t *testing.T) {
		guest(t, ctx, cli, origin, "kill -9 $(pgrep -x postgres -o)")
		deadline := time.Now().Add(time.Minute)
		for {
			logs, err := cli.Logs(ctx, origin)
			if err != nil {
				t.Fatal(err)
			}
			if len(logs.Services) == 1 && !logs.Services[0].Running {
				if logs.Services[0].Name != "postgres" || logs.Services[0].Output == "" {
					t.Fatalf("logs = %+v", logs.Services[0])
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("postgres still runs: %+v", logs)
			}
			time.Sleep(time.Second)
		}
		found := false
		eventCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		// The client sends no after for zero, and the stream then starts at the
		// latest event. One replays the history that matters here.
		_ = cli.Events(eventCtx, 1, origin, func(e client.Event) error {
			found = found || strings.Contains(e.Reason, "service postgres exited with")
			return nil
		})
		if !found {
			t.Fatal("no event names the service")
		}
		res, err := cli.Exec(ctx, origin, client.ExecRequest{Cmd: []string{"pg_isready", "-h", "postgres"}, TimeoutSeconds: 30})
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode == 0 {
			t.Fatal("something restarted postgres")
		}
	})

	t.Run("ImageCommandWithNoStart", func(t *testing.T) {
		if err := cli.CreateTemplate(ctx, client.TemplateRequest{
			Name: "gate-services-nginx", Image: "docker.io/library/nginx:alpine",
			VCPUs: 1, MemoryMB: 256, DiskMB: 1024, EgressAllow: []string{}, Port: 80,
		}); err != nil {
			t.Fatal(err)
		}
		waitTemplate(t, ctx, cli, "gate-services-nginx", "ready")
		sandbox, err := cli.CreateSandbox(ctx, client.SandboxRequest{Template: "gate-services-nginx", Lifecycle: "ephemeral", IdleSeconds: idle})
		if err != nil {
			t.Fatal(err)
		}
		if got := guest(t, ctx, cli, sandbox.ID, "wget -qO- 127.0.0.1:80 | grep -c nginx"); got == "0" {
			t.Fatalf("nginx answered %q", got)
		}
	})

	t.Run("NoPortRunsNoImageCommand", func(t *testing.T) {
		if err := cli.CreateTemplate(ctx, client.TemplateRequest{
			Name: "gate-services-ubuntu", Image: "docker.io/library/ubuntu:24.04",
			VCPUs: 1, MemoryMB: 256, DiskMB: 2048, EgressAllow: []string{},
		}); err != nil {
			t.Fatal(err)
		}
		waitTemplate(t, ctx, cli, "gate-services-ubuntu", "ready")
		sandbox, err := cli.CreateSandbox(ctx, client.SandboxRequest{Template: "gate-services-ubuntu", Lifecycle: "ephemeral", IdleSeconds: idle})
		if err != nil {
			t.Fatal(err)
		}
		guest(t, ctx, cli, sandbox.ID, "true")
	})

	t.Run("BuildFailuresNameTheService", func(t *testing.T) {
		for _, tc := range []struct {
			name, want string
			service    client.TemplateService
		}{
			{"gate-services-noport", "service quiet", client.TemplateService{Name: "quiet", Image: "docker.io/library/alpine:3.20", Start: []string{"sleep", "600"}, Port: 5999}},
			{"gate-services-notready", "service never", client.TemplateService{Name: "never", Image: "docker.io/library/alpine:3.20", Start: []string{"sleep", "600"}, Ready: []string{"false"}}},
			{"gate-services-nopull", "service private", client.TemplateService{Name: "private", Image: "ghcr.io/alternayte/kiln-no-such-image:none"}},
		} {
			if err := cli.CreateTemplate(ctx, client.TemplateRequest{
				Name: tc.name, Image: "docker.io/library/alpine:3.20",
				VCPUs: 1, MemoryMB: 256, DiskMB: 1024, EgressAllow: []string{},
				Services: []client.TemplateService{tc.service},
			}); err != nil {
				t.Fatal(err)
			}
			row := waitTemplate(t, ctx, cli, tc.name, "failed")
			if !strings.Contains(row.Error, tc.want) {
				t.Errorf("%s: error %q does not name %q", tc.name, row.Error, tc.want)
			}
		}
	})
}

func waitTemplate(t *testing.T, ctx context.Context, cli *client.Client, name, want string) client.Template {
	t.Helper()
	for {
		row, err := cli.Template(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != "building" {
			if row.State != want {
				t.Fatalf("template %s is %s: %s", name, row.State, row.Error)
			}
			return row
		}
		select {
		case <-ctx.Done():
			t.Fatalf("template %s did not end its build", name)
		case <-time.After(2 * time.Second):
		}
	}
}

func guest(t *testing.T, ctx context.Context, cli *client.Client, id, command string) string {
	t.Helper()
	res, err := cli.Exec(ctx, id, client.ExecRequest{Cmd: []string{"sh", "-c", command}, TimeoutSeconds: 60})
	if err != nil {
		t.Fatalf("%s: %v", command, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("%s: exit %d: %s %s", command, res.ExitCode, res.Stdout, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

func clearAll(t *testing.T, cli *client.Client) {
	t.Helper()
	ctx := context.Background()
	rows, _ := cli.Sandboxes(ctx)
	for _, row := range rows {
		if strings.HasPrefix(row.Template, stackTemplate) {
			_ = cli.DeleteSandbox(ctx, row.ID)
		}
	}
	templates, _ := cli.Templates(ctx)
	for _, row := range templates {
		if strings.HasPrefix(row.Name, stackTemplate) {
			_ = cli.DeleteTemplate(ctx, row.Name)
		}
	}
}

// assertSnapshotUnderLimit checks the cgroup of a sandbox after a fork. The
// fill loads all of guest memory before the snapshot, and the snapshot file's
// page cache was charged to the same cgroup. The cgroup then sat at its limit
// for the whole write, and in CI run 41941372942 the kernel killed
// Firecracker. A snapshot now lifts the limit while the guest is paused and
// puts it back after the write.
//
// The max counter also counts ordinary page cache reclaim after the resume,
// so it is not zero by rule. One fork added 1157 and 1477 before the fix, and
// 0 and 8 after it.
func assertSnapshotUnderLimit(t *testing.T, id string, memoryMB int, before map[string]int) {
	t.Helper()
	after := memoryEvents(t, id)
	if after["oom_kill"] != before["oom_kill"] {
		t.Errorf("the kernel killed in the sandbox cgroup during the fork: oom_kill %d, then %d", before["oom_kill"], after["oom_kill"])
	}
	if hits := after["max"] - before["max"]; hits >= 100 {
		t.Errorf("the fork ran the sandbox cgroup into its limit %d times", hits)
	}
	limit, err := os.ReadFile(filepath.Join(runtime.CgroupDir(id), "memory.max"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(limit)), strconv.FormatInt(runtime.SandboxMemoryLimit(memoryMB), 10); got != want {
		t.Errorf("memory.max after the snapshot = %s, want %s", got, want)
	}
}

// memoryEvents reads the memory.events counters of a sandbox cgroup.
func memoryEvents(t *testing.T, id string) map[string]int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(runtime.CgroupDir(id), "memory.events"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	fields := strings.Fields(string(b))
	for i := 0; i+1 < len(fields); i += 2 {
		n, err := strconv.Atoi(fields[i+1])
		if err != nil {
			t.Fatalf("memory.events %q: %v", b, err)
		}
		out[fields[i]] = n
	}
	return out
}
