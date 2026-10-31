//go:build kvm

package upgradegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alternayte/kiln/client"
)

const templateName = "gate-upgrade"

// TestGateUpgrade follows the steps in infra/README.md from the latest
// release: a daemon from that release runs a sandbox, the binary is replaced
// by kilnd, and the sandbox keeps running with its snapfault process.
func TestGateUpgrade(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate upgrade needs root: start it with sudo")
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
	cli := client.New("http://"+cfg.ControlAddr, cfg.BearerToken)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	previous := downloadPreviousRelease(t)
	current := buildCurrent(t)

	old := serve(t, previous, root)
	waitHealth(t, ctx, cli)
	if err := cli.CreateTemplate(ctx, client.TemplateRequest{
		Name: templateName, Image: "docker.io/library/alpine:3.20",
		VCPUs: 1, MemoryMB: 256, DiskMB: 1024, EgressAllow: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, ctx, cli)
	idle := 3600
	sandbox, err := cli.CreateSandbox(ctx, client.SandboxRequest{
		Template: templateName, Lifecycle: "persistent", IdleSeconds: idle,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := sandbox.ID
	// A value held in guest memory proves the guest kept running.
	if _, err := cli.StartProcess(ctx, id, client.ProcessRequest{Cmd: []string{"sh", "-c", "echo $$ > /tmp/pid; exec sleep 3600"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	before := guest(t, ctx, cli, id, "cat", "/tmp/pid")
	pagesBefore := snapfaultCount(t)
	if pagesBefore == 0 {
		t.Fatal("no snapfault process runs under the previous release")
	}

	// The steps of the upgrade: stop only the daemon, then remove its binary.
	old.Process.Signal(syscall.SIGTERM)
	old.Wait()
	if err := os.Remove(previous); err != nil {
		t.Fatal(err)
	}

	next := serve(t, current, root)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = cli.DeleteSandbox(ctx, id)
		_ = cli.DeleteTemplate(ctx, templateName)
		next.Process.Signal(syscall.SIGTERM)
		next.Wait()
	})
	waitHealth(t, ctx, cli)

	if after := guest(t, ctx, cli, id, "cat", "/tmp/pid"); after != before {
		t.Fatalf("the guest process changed across the upgrade: %q then %q", before, after)
	}
	if got := guest(t, ctx, cli, id, "sh", "-c", "kill -0 "+before+" && echo alive"); got != "alive" {
		t.Fatalf("the guest process did not survive the upgrade: %q", got)
	}
	if after := snapfaultCount(t); after < pagesBefore {
		t.Fatalf("snapfault processes: %d before the upgrade, %d after", pagesBefore, after)
	}
	// Reading untouched pages proves the old snapfault still serves them.
	if _, err := cli.Fork(ctx, id, 1, false); err != nil {
		t.Fatalf("a fork after the upgrade: %v", err)
	}
	rows, err := cli.Sandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Template == templateName && row.ID != id {
			if err := cli.DeleteSandbox(ctx, row.ID); err != nil {
				t.Errorf("delete the fork: %v", err)
			}
		}
	}
}

// downloadPreviousRelease fetches the host binary of the latest release and
// checks its checksum. A release before the rename calls it kiln.
func downloadPreviousRelease(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	const base = "https://github.com/alternayte/kiln/releases/latest/download/"
	sums := fetch(t, base+"SHA256SUMS", true)
	for _, name := range []string{"kilnd", "kiln"} {
		body := fetch(t, base+name, false)
		if body == nil {
			continue
		}
		sum := sha256.Sum256(body)
		want := hex.EncodeToString(sum[:]) + "  " + name
		if !strings.Contains(string(sums), want) {
			t.Fatalf("%s does not match SHA256SUMS", name)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Fatal("the latest release has no host binary")
	return ""
}

func fetch(t *testing.T, url string, required bool) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if required {
			t.Fatalf("%s: %s", url, resp.Status)
		}
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func buildCurrent(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "kilnd")
	repo, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/kilnd")
	cmd.Dir = strings.TrimSpace(string(repo))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build kilnd: %v: %s", err, b)
	}
	return out
}

func serve(t *testing.T, binary, root string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binary, "serve")
	cmd.Env = append(os.Environ(), "KILN_ROOT="+root)
	log, err := os.Create(filepath.Join(t.TempDir(), "serve.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", binary, err)
	}
	return cmd
}

func waitHealth(t *testing.T, ctx context.Context, cli *client.Client) {
	t.Helper()
	for {
		health, err := cli.Health(ctx)
		if err == nil && health.OK {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the daemon did not become healthy: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func waitReady(t *testing.T, ctx context.Context, cli *client.Client) {
	t.Helper()
	for {
		row, err := cli.Template(ctx, templateName)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == "ready" {
			return
		}
		if row.State != "building" {
			t.Fatalf("template state %s: %s", row.State, row.Error)
		}
		select {
		case <-ctx.Done():
			t.Fatal("the template did not build")
		case <-time.After(2 * time.Second):
		}
	}
}

func guest(t *testing.T, ctx context.Context, cli *client.Client, id string, argv ...string) string {
	t.Helper()
	res, err := cli.Exec(ctx, id, client.ExecRequest{Cmd: argv, TimeoutSeconds: 30})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec %v: exit %d: %s", argv, res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// snapfaultCount counts snapfault processes. It reads the argument, so a
// process from the old binary name still counts.
func snapfaultCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", " snapfault ").Output()
	if err != nil && len(out) == 0 {
		return 0
	}
	return len(strings.Fields(fmt.Sprintf("%s", out)))
}
