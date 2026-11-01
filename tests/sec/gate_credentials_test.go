//go:build kvm

package secgate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	credentialTemplate    = "sec-credential"
	oldCredentialTemplate = "sec-credential-old"
	// boundName echoes each request header, and httpbin.org answers 401 at
	// /bearer with no Authorization header.
	boundName = "httpbin.org"
	// unboundName echoes too, and no binding names it.
	unboundName     = "postman-echo.com"
	credentialValue = "kiln-gate-credential-value"
)

// credentialFetch prints the status and the body of one request. It follows no redirect.
const credentialFetch = `import json,sys,urllib.request,urllib.error
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,*a,**k): return None
url=sys.argv[1]
headers=json.loads(sys.argv[2]) if len(sys.argv)>2 else {}
op=urllib.request.build_opener(NoRedirect)
try:
    r=op.open(urllib.request.Request(url,headers=headers),timeout=30)
except urllib.error.HTTPError as e:
    r=e
print(r.status if hasattr(r,'status') else r.code)
print(r.read().decode())
`

// TestSecCredentials runs the credential binding on real microVMs, against a
// public site that echoes what reached it.
func TestSecCredentials(t *testing.T) {
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
	sealKey := make([]byte, store.KeySize)
	for i := range sealKey {
		sealKey[i] = byte(i + 1)
	}
	builder := template.Builder{KilninitPath: filepath.Join(root, "bin", "kilninit")}
	tmgr := &template.Manager{Root: root, Store: st, Runtime: rt, Network: nm, Builder: builder, KernelPath: kernel, SealKey: sealKey}
	newManager := func(net *network.Manager) *sandbox.Manager {
		return sandbox.New(sandbox.Config{
			Root: root, Store: st, Runtime: rt, Network: net, SealKey: sealKey,
			Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
			KernelPath: kernel,
		})
	}
	sbx := newManager(nm)
	destroyAll := func() {
		clean := context.WithoutCancel(ctx)
		rows, _ := st.ListSandboxes(clean)
		for _, row := range rows {
			if (row.TemplateName == credentialTemplate || row.TemplateName == oldCredentialTemplate) && row.DestroyedAt == nil {
				_ = sbx.Destroy(clean, row.ID)
			}
		}
		_ = tmgr.Delete(clean, credentialTemplate)
		_ = tmgr.Delete(clean, oldCredentialTemplate)
	}
	destroyAll()
	t.Cleanup(destroyAll)

	build(t, ctx, tmgr, template.BuildRequest{
		Name: credentialTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 512, DiskMB: 2048,
		EgressAllow: []string{boundName, unboundName},
	}, store.TemplateReady)
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template: credentialTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: 900,
	})
	binding := sandbox.CredentialInput{
		Name: "echo", Host: boundName, Header: "Authorization", Value: "Bearer " + credentialValue,
	}
	get := func(t *testing.T, id, url, headers string) (string, string) {
		t.Helper()
		argv := []string{"python", "-c", credentialFetch, url}
		if headers != "" {
			argv = append(argv, headers)
		}
		res, err := sbx.Exec(ctx, id, runtime.ExecRequest{Cmd: argv, TimeoutSeconds: 60}, nil)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("fetch %s: %+v %v", url, res, err)
		}
		status, body, _ := strings.Cut(res.Stdout, "\n")
		return status, body
	}
	sentAuth := func(t *testing.T, body string) string {
		t.Helper()
		var parsed struct {
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("echo body %q: %v", body, err)
		}
		for k, v := range parsed.Headers {
			if strings.EqualFold(k, "Authorization") {
				return v
			}
		}
		return ""
	}
	if status, _ := get(t, sb.ID, "https://"+boundName+"/bearer", ""); status != "401" {
		t.Fatalf("before a binding the provider answered %s", status)
	}
	if _, err := sbx.SetCredentials(ctx, sb.ID, []sandbox.CredentialInput{binding}); err != nil {
		t.Fatal(err)
	}

	t.Run("KeyWorksAndStaysOutOfTheGuest", func(t *testing.T) {
		if status, _ := get(t, sb.ID, "https://"+boundName+"/bearer", ""); status != "200" {
			t.Fatalf("the provider answered %s", status)
		}
		found := guestOutput(t, ctx, sbx, sb.ID, "sh", "-c", "env; cat /proc/*/environ 2>/dev/null")
		if strings.Contains(found, credentialValue) {
			t.Error("the guest environment holds the value")
		}
	})
	t.Run("KeyStaysOutOfTheDatabase", func(t *testing.T) {
		for _, name := range []string{"kiln.db", "kiln.db-wal"} {
			raw, err := os.ReadFile(filepath.Join(root, name))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), credentialValue) {
				t.Errorf("%s holds the value", name)
			}
		}
	})
	t.Run("GuestHeaderIsReplaced", func(t *testing.T) {
		_, body := get(t, sb.ID, "https://"+boundName+"/anything", `{"Authorization":"Bearer guest"}`)
		if got := sentAuth(t, body); got != binding.Value {
			t.Errorf("the provider saw %q", got)
		}
	})
	t.Run("OtherHostGetsNoKey", func(t *testing.T) {
		_, body := get(t, sb.ID, "https://"+unboundName+"/headers", "")
		if strings.Contains(body, credentialValue) {
			t.Error("another allowed host received the key")
		}
	})
	t.Run("RedirectCarriesNoKey", func(t *testing.T) {
		status, _ := get(t, sb.ID, "https://"+boundName+"/redirect-to?url=https%3A%2F%2F"+unboundName+"%2Fheaders", "")
		if status != "302" {
			t.Errorf("the guard answered %s, want the provider's redirect", status)
		}
	})
	t.Run("PathNarrowsTheKey", func(t *testing.T) {
		narrow := binding
		narrow.Path = "/anything/acme/"
		if _, err := sbx.SetCredentials(ctx, sb.ID, []sandbox.CredentialInput{narrow}); err != nil {
			t.Fatal(err)
		}
		_, body := get(t, sb.ID, "https://"+boundName+"/anything/acme/x", "")
		if got := sentAuth(t, body); got != binding.Value {
			t.Errorf("the prefix sent %q", got)
		}
		for _, path := range []string{"/anything/other", "/anything/acme/../../headers"} {
			_, body := get(t, sb.ID, "https://"+boundName+path, `{"Authorization":"Bearer guest"}`)
			if got := sentAuth(t, body); got != "" {
				t.Errorf("%s sent %q", path, got)
			}
		}
		if _, err := sbx.SetCredentials(ctx, sb.ID, []sandbox.CredentialInput{binding}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("EgressCannotCloseABoundHost", func(t *testing.T) {
		if _, err := sbx.SetEgress(ctx, sb.ID, []string{unboundName}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("closing the host: %v", err)
		}
		if _, err := sbx.ObserveEgress(ctx, sb.ID); !errors.Is(err, store.ErrConflict) {
			t.Errorf("observing with a binding: %v", err)
		}
	})
	t.Run("ForksGetTheirOwnKeys", func(t *testing.T) {
		// A sandbox with only bindings needs no allow_secret_fork.
		plain, err := sbx.Fork(ctx, sb.ID, 1, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sbx.Destroy(context.WithoutCancel(ctx), plain[0].ID)
		if status, _ := get(t, plain[0].ID, "https://"+boundName+"/bearer", ""); status != "401" {
			t.Errorf("a fork with no binding answered %s", status)
		}
		if _, err := sbx.SetCredentials(ctx, plain[0].ID, []sandbox.CredentialInput{binding}); err != nil {
			t.Fatal(err)
		}
		if status, _ := get(t, plain[0].ID, "https://"+boundName+"/bearer", ""); status != "200" {
			t.Errorf("after a binding the fork answered %s", status)
		}
		other := binding
		other.Value = "Bearer other-key"
		copies, err := sbx.Fork(ctx, sb.ID, 2, false, nil, []sandbox.CredentialInput{other})
		if err != nil {
			t.Fatal(err)
		}
		defer sbx.Destroy(context.WithoutCancel(ctx), copies[0].ID)
		defer sbx.Destroy(context.WithoutCancel(ctx), copies[1].ID)
		_, body := get(t, copies[0].ID, "https://"+boundName+"/anything", "")
		if got := sentAuth(t, body); got != other.Value {
			t.Errorf("the fork sent %q", got)
		}
		if _, err := sbx.SetCredentials(ctx, copies[0].ID, nil); err != nil {
			t.Fatal(err)
		}
		if status, _ := get(t, copies[0].ID, "https://"+boundName+"/bearer", ""); status != "401" {
			t.Errorf("a removed key answered %s", status)
		}
		if status, _ := get(t, copies[1].ID, "https://"+boundName+"/bearer", ""); status != "200" {
			t.Errorf("removing one fork's key changed the other: %s", status)
		}
	})
	t.Run("SnapshotHoldsNoKey", func(t *testing.T) {
		snap, err := sbx.Snapshot(ctx, sb.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		copies, err := sbx.RestoreSnapshot(ctx, snap.ID, 1, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer sbx.Destroy(context.WithoutCancel(ctx), copies[0].ID)
		if status, _ := get(t, copies[0].ID, "https://"+boundName+"/bearer", ""); status != "401" {
			t.Errorf("a restored copy answered %s", status)
		}
	})
	t.Run("SurvivesSleepWakeAndRestart", func(t *testing.T) {
		if err := sbx.Sleep(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		// A sleeping sandbox takes a change without a wake.
		rotated := binding
		rotated.Value = "Bearer rotated-value"
		if _, err := sbx.SetCredentials(ctx, sb.ID, []sandbox.CredentialInput{rotated}); err != nil {
			t.Fatal(err)
		}
		if _, err := sbx.Wake(ctx, sb.ID); err != nil {
			t.Fatal(err)
		}
		_, body := get(t, sb.ID, "https://"+boundName+"/anything", "")
		if got := sentAuth(t, body); got != rotated.Value {
			t.Errorf("after a wake the provider saw %q", got)
		}
		restartedNet := network.New(kilnBin, root)
		restarted := newManager(restartedNet)
		sweep := &reconcile.Reconciler{Root: root, Store: st, Sandboxes: restarted, Network: restartedNet, Templates: tmgr, Runtime: rt}
		if _, err := sweep.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		sbx = restarted
		_, body = get(t, sb.ID, "https://"+boundName+"/anything", "")
		if got := sentAuth(t, body); got != rotated.Value {
			t.Errorf("after a restart the provider saw %q", got)
		}
	})
	t.Run("RequestLog", func(t *testing.T) {
		get(t, sb.ID, "https://"+boundName+"/anything/logged?token=query", "")
		deadline := time.Now().Add(20 * time.Second)
		for {
			out, err := sbx.EgressRequests(ctx, sb.ID)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "token=query") || strings.Contains(string(raw), "rotated-value") {
				t.Fatalf("the log holds a query or a value: %s", raw)
			}
			found := false
			for _, row := range out.Requests {
				found = found || (row.Path == "/anything/logged" && row.Method == "GET" && row.Status == 200 && row.Binding == "echo")
			}
			if found {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the log never showed the request: %s", raw)
			}
			time.Sleep(200 * time.Millisecond)
		}
		waitRecord(t, ctx, sbx, sb.ID, func(rows []store.EgressRecord) bool {
			for _, row := range rows {
				if row.Host == boundName && row.Decision == "injected" && row.Binding == "echo" {
					return true
				}
			}
			return false
		})
	})
	t.Run("OldTemplateAnswersConflict", func(t *testing.T) {
		old := &template.Manager{Root: root, Store: st, Runtime: rt, Network: nm, Builder: builder, KernelPath: kernel}
		build(t, ctx, old, template.BuildRequest{
			Name: oldCredentialTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 512, DiskMB: 2048,
			EgressAllow: []string{boundName},
		}, store.TemplateReady)
		oldSandbox := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
			Template: oldCredentialTemplate, Lifecycle: store.LifecycleEphemeral, IdleSeconds: 900,
		})
		defer sbx.Destroy(context.WithoutCancel(ctx), oldSandbox.ID)
		_, err := sbx.SetCredentials(ctx, oldSandbox.ID, []sandbox.CredentialInput{binding})
		if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "rebuild") {
			t.Errorf("an old template answered %v", err)
		}
	})
}
