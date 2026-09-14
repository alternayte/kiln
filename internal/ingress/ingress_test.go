package ingress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
)

func TestSubdomainOf(t *testing.T) {
	cases := []struct {
		host string
		zone string
		want string
		ok   bool
	}{
		{"abc123.example.com", "example.com", "abc123", true},
		{"abc123.example.com:443", "example.com", "abc123", true},
		{"ABC123.Example.Com", "example.com", "abc123", true},
		{"abc123.example.com.", "example.com", "abc123", true},
		{"abc123-def.example.com", "example.com", "abc123-def", true},
		{"example.com", "example.com", "", false},
		{"a.b.example.com", "example.com", "", false},
		{"abc123.example.org", "example.com", "", false},
		{"evil-example.com", "example.com", "", false},
		{"-bad.example.com", "example.com", "", false},
		{"bad-.example.com", "example.com", "", false},
		{"abc123.example.com", "", "", false},
	}
	for _, tc := range cases {
		got, ok := subdomainOf(tc.host, tc.zone)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("subdomainOf(%q, %q) = %q, %t; want %q, %t", tc.host, tc.zone, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDropDomainAttribute(t *testing.T) {
	got := dropDomainAttribute("session=abc; Domain=.example.com; Path=/; Secure")
	if got != "session=abc; Path=/; Secure" {
		t.Fatalf("dropDomainAttribute = %q", got)
	}
	if got := dropDomainAttribute("a=1; domain=example.com"); got != "a=1" {
		t.Fatalf("dropDomainAttribute = %q", got)
	}
}

// TestWakeLimits pins the two caps: ten wakes per subdomain in a rolling
// minute and four host-wide restores in flight.
func TestWakeLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := New(Config{Now: func() time.Time { return now }})

	for i := 0; i < wakesInFlight; i++ {
		if !s.allowWake("aaaa") {
			t.Fatalf("wake %d was refused below the host-wide cap", i)
		}
	}
	if s.allowWake("aaaa") {
		t.Fatal("a fifth restore in flight was allowed")
	}
	for i := 0; i < wakesInFlight; i++ {
		s.releaseWake()
	}
	// The in-flight cap is free again, but aaaа already spent four of its
	// ten rolling-minute wakes.
	for i := 0; i < wakesPerMinute-wakesInFlight; i++ {
		if !s.allowWake("aaaa") {
			t.Fatalf("wake %d was refused below the per-subdomain cap", i)
		}
		s.releaseWake()
	}
	if s.allowWake("aaaa") {
		t.Fatal("an eleventh wake in the rolling minute was allowed")
	}
	// Another subdomain has its own window, and the host-wide slot is free.
	if !s.allowWake("bbbb") {
		t.Fatal("a wake of a different subdomain was refused")
	}
	s.releaseWake()
	// The window rolls.
	now = now.Add(2 * time.Minute)
	if !s.allowWake("aaaa") {
		t.Fatal("a wake after the rolling minute was refused")
	}
}

// TestRoutingWithoutDialing pins what the ingress answers before any guest
// contact: unknown hostnames 404, control paths 404, and a team preview
// without a viewer store answers 503.
func TestRoutingWithoutDialing(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := sandbox.New(sandbox.Config{Root: t.TempDir(), Store: st, Zone: "example.com"})
	srv := New(Config{Zone: "example.com", Store: st, Sandboxes: mgr})

	ctx := context.Background()
	if err := st.CreateTemplate(ctx, store.Template{
		Name: "py", ImageRef: "docker.io/library/python:3.12-slim", VCPUs: 1, MemoryMB: 256,
		DiskMB: 1024, EgressAllow: []string{}, State: store.TemplateReady, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSandbox(ctx, store.Sandbox{
		ID: "sandbox1", TemplateName: "py", Lifecycle: store.LifecyclePersistent, State: store.SandboxRunning,
		IdleSeconds: 60, LastActiveAt: time.Unix(1_700_000_000, 0).UTC(), Metadata: "{}", CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	published, err := mgr.Publish(ctx, "sandbox1", 8000, store.VisibilityTeam)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		host string
		path string
		want int
	}{
		{"public host unknown", "nosuch.example.com", "/", http.StatusNotFound},
		{"outside the zone", "abc.example.org", "/", http.StatusNotFound},
		{"apex", "example.com", "/", http.StatusNotFound},
		{"control path", published.Subdomain + ".example.com", "/v1/sandboxes", http.StatusNotFound},
		{"auth without a viewer store", published.Subdomain + ".example.com", "/_kiln/auth/session", http.StatusNotFound},
		{"team without a viewer store", published.Subdomain + ".example.com", "/", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if tc.want != http.StatusNotFound && tc.want != http.StatusServiceUnavailable {
				return
			}
			if got := rec.Header().Get("X-Robots-Tag"); got != "noindex" {
				t.Fatalf("X-Robots-Tag = %q", got)
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != "sandbox" {
				t.Fatalf("Content-Security-Policy = %q", got)
			}
		})
	}

	// A failed sandbox serves nothing, even while its published row exists.
	if err := st.SetSandboxState(ctx, "sandbox1", store.SandboxFailed); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "sandbox1.example.com"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a failed sandbox answered %d, want 404", rec.Code)
	}
}

// TestViewerLogin pins the team-preview login path: an operator creates the
// first viewer, sign-in sets a session cookie scoped to the zone, and the
// session resolves on a later request.
func TestViewerLogin(t *testing.T) {
	ctx := context.Background()
	auth, db, err := NewAuth(ctx, filepath.Join(t.TempDir(), "viewer.db"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exists, err := ViewerExists(ctx, auth, "viewer@example.com")
	if err != nil || exists {
		t.Fatalf("viewer exists before create: %v, %v", exists, err)
	}
	if err := CreateViewer(ctx, auth, "viewer@example.com", "a-long-enough-password"); err != nil {
		t.Fatal(err)
	}
	if exists, err := ViewerExists(ctx, auth, "viewer@example.com"); err != nil || !exists {
		t.Fatalf("viewer exists after create: %v, %v", exists, err)
	}

	ts := httptest.NewServer(auth.Handler())
	t.Cleanup(ts.Close)
	body := strings.NewReader(`{"email":"viewer@example.com","password":"a-long-enough-password"}`)
	resp, err := http.Post(ts.URL+AuthPrefix+"/sign-in/email", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("sign-in status %d: %s", resp.StatusCode, data)
	}
	var session *http.Cookie
	for _, cookie := range resp.Cookies() {
		session = cookie
	}
	if session == nil {
		t.Fatal("sign-in set no cookie")
	}
	// RFC 6265 treats a leading dot as decoration, and the Go cookie writer
	// strips it. Either form covers every subdomain of the zone.
	if session.Domain != "example.com" && session.Domain != ".example.com" {
		t.Fatalf("session cookie domain %q, want the zone", session.Domain)
	}
	if !session.Secure {
		t.Fatal("session cookie is not Secure")
	}

	// The session resolves on a later request, which is the check the proxy
	// makes before it forwards.
	req := httptest.NewRequest(http.MethodGet, "https://abc123.example.com/", nil)
	req.AddCookie(session)
	got, err := auth.Session(ctx, req)
	if err != nil || got == nil {
		t.Fatalf("session lookup: %+v, %v", got, err)
	}
}
