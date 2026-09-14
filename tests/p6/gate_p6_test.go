//go:build kvm

package p6gate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alternayte/kiln/client"
	"github.com/alternayte/kiln/internal/ingress"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/store"
)

// gateTimeout bounds the whole gate. The gate recipe also passes a go test
// timeout.
const gateTimeout = 18 * time.Minute

const templateName = "p6-python"

// viewerEmail and viewerPassword are the gate's own viewer account. Its
// sessions live in the Auth-All tables, not in a sandbox row.
const (
	viewerEmail    = "gate-p6@example.invalid"
	viewerPassword = "gate-p6-password"
)

// TestGateP6 drives a real `kiln serve` process and reaches its public
// listener over HTTPS: a public preview answers with no credential, a team
// preview challenges and never touches the guest, a retired hostname 404s, a
// control path is never routed, the control listener is local, the guest
// cannot reach it, and the wake cap answers 503. It needs config.json with a
// zone and an ACME account, and wildcard DNS that points at this host. The
// gate recipe runs it under scripts/leak.sh.
func TestGateP6(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate P6 needs root: start it with sudo")
	}
	repo := repoRoot(t)
	runPreflight(t, repo)
	root := kilnRoot()
	kernelPath(t, repo, root)
	if _, err := os.Stat(filepath.Join(root, "bin", "kilninit")); err != nil {
		t.Fatalf("kilninit: %v (run kiln init)", err)
	}
	cfg := readConfig(t, root)
	if cfg.Zone == "" || cfg.ACME.Email == "" || len(cfg.ACME.Credentials) == 0 {
		t.Fatalf("config.json needs a zone and an ACME account; run kiln init --zone <zone> --acme-email <address> with %s set",
			ingress.CloudflareTokenKey)
	}
	kilnBin := buildKiln(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	cli := client.New("http://"+cfg.ControlAddr, cfg.BearerToken)

	var daemon *daemon
	t.Cleanup(func() {
		if daemon == nil || daemon.exited() {
			daemon = startServe(t, kilnBin, root)
			waitHealth(t, cli, 90*time.Second)
		}
		clearAll(t, cli)
		daemon.stop(t)
	})
	daemon = startServe(t, kilnBin, root)
	waitHealth(t, cli, 90*time.Second)
	clearAll(t, cli)

	buildTemplate(t, cli, ctx)

	sb, err := cli.CreateSandbox(ctx, client.SandboxRequest{
		Template:    templateName,
		Lifecycle:   store.LifecyclePersistent,
		IdleSeconds: 5,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() {
		if err := cli.DeleteSandbox(context.Background(), sb.ID); err != nil && !client.NotFound(err) {
			t.Logf("cleanup: destroy %s: %v", sb.ID, err)
		}
	})

	server := strings.ReplaceAll(inGuestServer, "__MARKER__", sb.ID)
	if err := cli.WriteFile(ctx, sb.ID, "/tmp/server.py", strings.NewReader(server)); err != nil {
		t.Fatalf("write the guest server: %v", err)
	}
	if out := execIn(t, cli, sb.ID, "sh", "-c", "nohup python /tmp/server.py >/tmp/server.log 2>&1 &"); out.ExitCode != 0 {
		t.Fatalf("start the guest server: exit %d stderr %q", out.ExitCode, out.Stderr)
	}
	waitGuest(t, cli, sb.ID, "python", "-c", guestGet("http://127.0.0.1:8000", 2))

	var publicURL, teamURL, sessionCookie string

	t.Run("Publish", func(t *testing.T) {
		public, err := cli.Publish(ctx, sb.ID, client.PublishRequest{Port: 8000, Visibility: store.VisibilityPublic})
		if err != nil {
			t.Fatalf("publish public: %v", err)
		}
		again, err := cli.Publish(ctx, sb.ID, client.PublishRequest{Port: 8000, Visibility: store.VisibilityPublic})
		if err != nil {
			t.Fatalf("republish: %v", err)
		}
		if again.URL != public.URL {
			t.Fatalf("republish moved the hostname: %s then %s", public.URL, again.URL)
		}
		team, err := cli.Publish(ctx, sb.ID, client.PublishRequest{Port: 8001, Visibility: store.VisibilityTeam})
		if err != nil {
			t.Fatalf("publish team: %v", err)
		}
		publicURL, teamURL = public.URL, team.URL
		publicLabel := labelOf(t, public.URL, cfg.Zone)
		teamLabel := labelOf(t, team.URL, cfg.Zone)
		if len(publicLabel) != 32 {
			t.Fatalf("public label %q, want 128 bits as 32 hex characters", publicLabel)
		}
		if _, err := hex.DecodeString(publicLabel); err != nil {
			t.Fatalf("public label %q is not hex", publicLabel)
		}
		if teamLabel != publicLabel+"-8001" {
			t.Fatalf("team label %q, want %s-8001", teamLabel, publicLabel)
		}
		if strings.Contains(publicLabel, sb.ID) {
			t.Fatalf("the hostname %q is derived from the sandbox id", publicLabel)
		}
		row, err := cli.Sandbox(ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(row.Published) != 2 {
			t.Fatalf("GET sandbox shows %d published ports, want 2: %+v", len(row.Published), row.Published)
		}
		seen := map[int]bool{}
		for _, p := range row.Published {
			seen[p.Port] = true
		}
		if !seen[8000] || !seen[8001] {
			t.Fatalf("published ports %+v, want 8000 and 8001", row.Published)
		}
	})

	// The wildcard certificate is issued through ACME DNS-01 in the
	// background. Wait for it before the HTTPS assertions.
	waitPublic(t, publicURL, 8*time.Minute)

	t.Run("Public", func(t *testing.T) {
		status, header, body := fetch(t, publicURL, "")
		if status != http.StatusOK {
			t.Fatalf("public preview status %d", status)
		}
		if body != "p6-"+sb.ID {
			t.Fatalf("public preview body %q, want p6-%s", body, sb.ID)
		}
		assertPreviewHeaders(t, header)
	})

	t.Run("ControlPathNotRouted", func(t *testing.T) {
		status, header, body := fetch(t, publicURL+"v1/sandboxes", "")
		if status != http.StatusNotFound {
			t.Fatalf("GET /v1/sandboxes on the preview host: status %d body %q, want 404", status, body)
		}
		assertPreviewHeaders(t, header)
	})

	t.Run("UnknownHost", func(t *testing.T) {
		label := randomLabel(t)
		status, header, _ := fetch(t, "https://"+label+"."+cfg.Zone+"/", "")
		if status != http.StatusNotFound {
			t.Fatalf("unknown hostname status %d, want 404", status)
		}
		assertPreviewHeaders(t, header)
	})

	t.Run("Team", func(t *testing.T) {
		before := guestLineCount(t, cli, sb.ID)
		status, header, _ := fetch(t, teamURL, "")
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("team preview without a session: status %d, want a challenge", status)
		}
		assertPreviewHeaders(t, header)
		if after := guestLineCount(t, cli, sb.ID); after != before {
			t.Fatalf("the unauthenticated request reached the guest: %d lines then %d", before, after)
		}
		// There is no self-signup.
		if code := rawStatus(t, http.MethodPost, teamURL+"_kiln/auth/sign-up/email"); code != http.StatusNotFound {
			t.Fatalf("the sign-up path answered %d, want 404", code)
		}
		// A viewer signs in, and the session is scoped to the zone.
		if err := ensureViewer(ctx, root, cfg.Zone); err != nil {
			t.Fatalf("create viewer: %v", err)
		}
		setCookie := signIn(t, teamURL)
		if !cookieCoversZone(setCookie, cfg.Zone) {
			t.Fatalf("session cookie %q is not scoped to every preview under %s", setCookie, cfg.Zone)
		}
		sessionCookie = cookiePair(setCookie)
		status, header, body := fetch(t, teamURL, sessionCookie)
		if status != http.StatusOK {
			t.Fatalf("team preview with a session: status %d body %q", status, body)
		}
		if body != "p6-"+sb.ID {
			t.Fatalf("team preview body %q, want p6-%s", body, sb.ID)
		}
		assertPreviewHeaders(t, header)
		if after := guestLineCount(t, cli, sb.ID); after <= before {
			t.Fatalf("the authenticated request did not reach the guest")
		}
		// A retired hostname 404s, even with a valid session.
		if err := cli.Retire(ctx, sb.ID, 8001); err != nil {
			t.Fatalf("retire: %v", err)
		}
		status, header, _ = fetch(t, teamURL, sessionCookie)
		if status != http.StatusNotFound {
			t.Fatalf("retired hostname status %d, want 404", status)
		}
		assertPreviewHeaders(t, header)
	})

	t.Run("GuestCannotReachControl", func(t *testing.T) {
		hostIP := hostUplinkIP(t)
		code := fmt.Sprintf("import socket; socket.create_connection((%q, 8080), timeout=5)", hostIP)
		if out := execIn(t, cli, sb.ID, "python", "-c", code); out.ExitCode == 0 {
			t.Fatalf("the guest reached the control API at %s:8080", hostIP)
		}
	})

	t.Run("ControlListenerIsLocal", func(t *testing.T) {
		hostIP := hostUplinkIP(t)
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(hostIP, "8080"), 5*time.Second)
		if err == nil {
			conn.Close()
			t.Fatalf("the control listener answers on the public address %s:8080", hostIP)
		}
	})

	t.Run("WakeOnRequest", func(t *testing.T) {
		stop(t, cli, sb.ID)
		waitState(t, cli, sb.ID, store.SandboxSleeping, 60*time.Second)
		status, _, body := fetch(t, publicURL, "")
		if status != http.StatusOK || body != "p6-"+sb.ID {
			t.Fatalf("a request to a sleeping preview: status %d body %q", status, body)
		}
	})

	t.Run("WakeCap", func(t *testing.T) {
		stop(t, cli, sb.ID)
		waitState(t, cli, sb.ID, store.SandboxSleeping, 60*time.Second)
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
				hc := &http.Client{Timeout: 3 * time.Minute}
				resp, err := hc.Get(publicURL)
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
			t.Fatalf("the concurrent wake blast was not capped: %d served, 0 refused", served)
		}
		if served == 0 {
			t.Fatalf("every request in the wake blast was refused")
		}
	})

	t.Run("NoSurvivors", func(t *testing.T) {
		if err := cli.DeleteSandbox(ctx, sb.ID); err != nil && !client.NotFound(err) {
			t.Fatalf("destroy: %v", err)
		}
		status, _, _ := fetch(t, publicURL, "")
		if status != http.StatusNotFound {
			t.Fatalf("a hostname of a destroyed sandbox answered %d, want 404", status)
		}
	})
}

// inGuestServer serves both preview ports. Every request is logged, so the
// gate can prove that an unauthenticated request never reached the guest.
// MARKER is replaced with the sandbox id.
const inGuestServer = `import http.server, socketserver, threading, time

LOG = "/tmp/requests.log"
MARKER = "__MARKER__"

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(LOG, "a") as f:
            f.write(self.path + "\n")
        body = ("p6-" + MARKER).encode()
        if self.path == "/v1/sandboxes":
            body = b"GUEST-V1"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass

class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

for port in (8000, 8001):
    server = Server(("0.0.0.0", port), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
while True:
    time.sleep(60)
`

func buildTemplate(t *testing.T, cli *client.Client, ctx context.Context) {
	t.Helper()
	build := client.TemplateRequest{
		Name:        templateName,
		Image:       "docker.io/library/python:3.12-slim",
		VCPUs:       2,
		MemoryMB:    512,
		DiskMB:      2048,
		Setup:       []string{},
		EgressAllow: []string{"pypi.org", "files.pythonhosted.org"},
	}
	if err := cli.CreateTemplate(ctx, build); err != nil && !client.Conflict(err) {
		t.Fatalf("create template: %v", err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		v, err := cli.Template(ctx, templateName)
		if err != nil {
			t.Fatalf("get template: %v", err)
		}
		if v.State == store.TemplateReady {
			return
		}
		if v.State == store.TemplateFailed {
			t.Fatalf("template build failed: %s", v.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("template stuck in %q", v.State)
		}
		time.Sleep(time.Second)
	}
}

// fetch calls a preview and returns its status, headers and body. An empty
// cookie sends no credential.
func fetch(t *testing.T, url, cookie string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	hc := &http.Client{Timeout: 3 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// waitPublic waits until the wildcard certificate is issued and the preview
// answers. A TLS handshake failure or a 502 is not an error yet.
func waitPublic(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	hc := &http.Client{Timeout: 30 * time.Second}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		resp, err := hc.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the preview never answered over HTTPS: %s", last)
		}
		time.Sleep(3 * time.Second)
	}
}

// assertPreviewHeaders proves the two headers every preview response carries.
func assertPreviewHeaders(t *testing.T, header http.Header) {
	t.Helper()
	if got := header.Get("X-Robots-Tag"); got != "noindex" {
		t.Fatalf("X-Robots-Tag = %q, want noindex", got)
	}
	if got := header.Get("Content-Security-Policy"); got != "sandbox" {
		t.Fatalf("Content-Security-Policy = %q, want sandbox", got)
	}
}

// labelOf returns the single hostname label below the zone.
func labelOf(t *testing.T, url, zone string) string {
	t.Helper()
	host := strings.TrimSuffix(strings.TrimPrefix(url, "https://"), "/")
	label := strings.TrimSuffix(host, "."+zone)
	if label == host || label == "" {
		t.Fatalf("URL %q does not end in .%s", url, zone)
	}
	return label
}

// randomLabel draws 128 bits for a hostname that no row serves.
func randomLabel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// guestLineCount counts the guest server's request log.
func guestLineCount(t *testing.T, cli *client.Client, id string) int {
	t.Helper()
	out := execIn(t, cli, id, "sh", "-c", "wc -l < /tmp/requests.log")
	n, err := strconv.Atoi(strings.TrimSpace(out.Stdout))
	if err != nil {
		t.Fatalf("guest request log: %q", out.Stdout)
	}
	return n
}

// guestGet is a small python one-liner that fetches a guest URL.
func guestGet(url string, timeout int) string {
	return fmt.Sprintf("import urllib.request; urllib.request.urlopen(%q, timeout=%d)", url, timeout)
}

// waitGuest runs one command until it exits zero.
func waitGuest(t *testing.T, cli *client.Client, id string, argv ...string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if out := execIn(t, cli, id, argv...); out.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("guest command %v never succeeded", argv)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func execIn(t *testing.T, cli *client.Client, id string, argv ...string) client.ExecResult {
	t.Helper()
	out, err := cli.Exec(context.Background(), id, client.ExecRequest{Cmd: argv, TimeoutSeconds: 300})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	return out
}

// stop snapshots the sandbox and leaves it sleeping.
func stop(t *testing.T, cli *client.Client, id string) {
	t.Helper()
	if _, err := cli.Snapshot(context.Background(), id, true); err != nil {
		t.Fatalf("snapshot stop: %v", err)
	}
}

func waitState(t *testing.T, cli *client.Client, id, state string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		row, err := cli.Sandbox(context.Background(), id)
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
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

// ensureViewer creates the gate's viewer account once.
func ensureViewer(ctx context.Context, root, zone string) error {
	auth, db, err := ingress.NewAuth(ctx, filepath.Join(root, "kiln.db"), zone)
	if err != nil {
		return err
	}
	defer db.Close()
	exists, err := ingress.ViewerExists(ctx, auth, viewerEmail)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return ingress.CreateViewer(ctx, auth, viewerEmail, viewerPassword)
}

// signIn posts the viewer credentials to the published hostname and returns
// the Set-Cookie header.
func signIn(t *testing.T, base string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": viewerEmail, "password": viewerPassword})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"_kiln/auth/sign-in/email", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in: status %d: %s", resp.StatusCode, data)
	}
	for _, cookie := range resp.Header.Values("Set-Cookie") {
		if strings.Contains(cookie, "=") {
			return cookie
		}
	}
	t.Fatalf("sign-in set no cookie: %s", data)
	return ""
}

// cookiePair turns a Set-Cookie header into the name=value pair a request
// carries.
func cookiePair(setCookie string) string {
	pair, _, _ := strings.Cut(setCookie, ";")
	return strings.TrimSpace(pair)
}

// cookieCoversZone reports whether a Set-Cookie header scopes the cookie to
// the whole zone. RFC 6265 makes the leading dot decoration, and the Go
// cookie writer drops it, so both forms count.
func cookieCoversZone(setCookie, zone string) bool {
	lower := strings.ToLower(setCookie)
	return strings.Contains(lower, "domain="+zone) || strings.Contains(lower, "domain=."+zone)
}

// rawStatus sends a request without following redirects and returns the
// status.
func rawStatus(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode
}

// hostUplinkIP is the host's own address, as the guest would try to reach it.
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

func clearAll(t *testing.T, cli *client.Client) {
	t.Helper()
	ctx := context.Background()
	rows, err := cli.Sandboxes(ctx)
	if err != nil {
		t.Logf("cleanup: list sandboxes: %v", err)
	}
	for _, row := range rows {
		if row.Template == templateName {
			if err := cli.DeleteSandbox(ctx, row.ID); err != nil && !client.NotFound(err) {
				t.Logf("cleanup: destroy %s: %v", row.ID, err)
			}
		}
	}
	snaps, err := cli.Snapshots(ctx)
	if err != nil {
		t.Logf("cleanup: list snapshots: %v", err)
	}
	for _, snap := range snaps {
		if snap.Template == templateName {
			if err := cli.DeleteSnapshot(ctx, snap.ID); err != nil && !client.NotFound(err) {
				t.Logf("cleanup: delete snapshot %s: %v", snap.ID, err)
			}
		}
	}
	if err := cli.DeleteTemplate(ctx, templateName); err != nil && !client.NotFound(err) && !client.Conflict(err) {
		t.Logf("cleanup: delete template %s: %v", templateName, err)
	}
}

// daemon is one `kiln serve` child process.
type daemon struct {
	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
}

func startServe(t *testing.T, kilnBin, root string) *daemon {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "serve.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(kilnBin, "serve")
	cmd.Env = append(os.Environ(), "KILN_ROOT="+root)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Fatalf("start kiln serve: %v", err)
	}
	d := &daemon{cmd: cmd, log: f, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(d.done)
	}()
	return d
}

func (d *daemon) exited() bool {
	if d == nil {
		return true
	}
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

func (d *daemon) stop(t *testing.T) {
	t.Helper()
	if d.exited() {
		d.log.Close()
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(30 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.done
	}
	if b, err := os.ReadFile(d.log.Name()); err == nil && len(b) > 0 {
		t.Logf("kiln serve log:\n%s", b)
	}
	d.log.Close()
}

func waitHealth(t *testing.T, cli *client.Client, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		health, err := cli.Health(ctx)
		cancel()
		if err == nil && health.OK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("kiln serve did not become healthy: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
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

type gateConfig struct {
	BearerToken string `json:"bearer_token"`
	ControlAddr string `json:"control_addr"`
	Zone        string `json:"zone"`
	ACME        struct {
		Email       string            `json:"email"`
		DNSProvider string            `json:"dns_provider"`
		Credentials map[string]string `json:"credentials"`
	} `json:"acme"`
}

func readConfig(t *testing.T, root string) gateConfig {
	t.Helper()
	var cfg gateConfig
	b, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatalf("config.json: %v (run kiln init)", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	if cfg.ControlAddr == "" {
		cfg.ControlAddr = "127.0.0.1:8080"
	}
	return cfg
}

// kernelPath returns the pinned kernel under the Kiln root.
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
