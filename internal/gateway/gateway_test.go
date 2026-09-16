package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	authall "github.com/alternayte/auth-all"
	"github.com/alternayte/auth-all/plugins/apikeys"
	"github.com/alternayte/auth-all/plugins/organizations"
	"github.com/alternayte/auth-all/plugins/roles"
	"github.com/alternayte/auth-all/store/sqlite"
)

// seenRequest is what the fake host received.
type seenRequest struct {
	tenant string
	auth   string
	cookie string
	path   string
}

func TestProxyNamesTheTenantAndHidesTheCallerCredential(t *testing.T) {
	seen := make(chan seenRequest, 4)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- seenRequest{
			tenant: r.Header.Get(TenantHeader),
			auth:   r.Header.Get("Authorization"),
			cookie: r.Header.Get("Cookie"),
			path:   r.URL.Path,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer host.Close()

	srv, key, tenantID := newTestGateway(t, host.URL)
	public := httptest.NewServer(srv.Handler())
	defer public.Close()

	// A call with the tenant's API key reaches the host, and the host sees
	// the gateway's own credential, never the caller's.
	req, _ := http.NewRequest(http.MethodGet, public.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Cookie", "session=stolen")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	got := <-seen
	if got.tenant != tenantID {
		t.Fatalf("tenant %q, want %q", got.tenant, tenantID)
	}
	if got.auth != "Bearer host-token" {
		t.Fatalf("the host saw %q, want the gateway's host token", got.auth)
	}
	if got.cookie != "" {
		t.Fatalf("the caller's cookie reached the host: %q", got.cookie)
	}

	// A call with no credential never reaches the host.
	resp, err = http.Get(public.URL + "/v1/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d for a call with no credential, want 401", resp.StatusCode)
	}
	select {
	case extra := <-seen:
		t.Fatalf("a call with no credential reached the host: %+v", extra)
	default:
	}

	// The audit log holds the call that did reach the host.
	var count int
	row := srv.Audit.DB.QueryRow(`SELECT count(*) FROM kiln_audit WHERE tenant_id = ? AND path = '/v1/sandboxes' AND status = 200`, tenantID)
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("audit rows %d, want 1", count)
	}
}

// newTestGateway builds a gateway on a temporary store, with one tenant and
// one key of that tenant.
func newTestGateway(t *testing.T, hostURL string) (*Server, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open("file:" + filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tenants := organizations.New(
		organizations.Roles(
			organizations.Role("operator", "*"),
			organizations.Role("editor", "sandbox:*", "organization:read"),
		),
		organizations.DefaultRole("editor"),
		organizations.OwnerRole("operator"),
	)
	keys := apikeys.New(apikeys.Prefix("kiln_"), apikeys.Organizations(tenants))
	auth, err := authall.New(
		authall.WithStore(sqlite.New(db)),
		authall.WithBaseURL("http://gateway.test"),
		authall.WithEmailPassword(),
		authall.WithPlugins(roles.New(roles.Hierarchy("reader", "editor", "operator")), tenants, keys),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// The sign-up route is the only way to make a user, so the test uses it.
	signUp := httptest.NewRecorder()
	body := strings.NewReader(`{"email":"operator@example.com","password":"correct horse battery staple"}`)
	req := httptest.NewRequest(http.MethodPost, "/sign-up/email", body)
	req.Header.Set("Content-Type", "application/json")
	auth.Handler().ServeHTTP(signUp, req)
	if signUp.Code >= 400 {
		t.Fatalf("sign up: %d %s", signUp.Code, signUp.Body.String())
	}
	var created struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(signUp.Body.Bytes(), &created); err != nil || created.User.ID == "" {
		t.Fatalf("sign up response %s: %v", signUp.Body.String(), err)
	}
	user, err := auth.Store().Users().GetByID(ctx, created.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := tenants.Create(ctx, user, organizations.CreateInput{Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	_, plaintext, err := keys.Create(ctx, user, apikeys.CreateInput{
		UserID: user.ID, Name: "ci", OrgID: row.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(hostURL)
	if err != nil {
		t.Fatal(err)
	}
	audit := &Audit{DB: db, Driver: "sqlite"}
	if err := audit.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return &Server{
		Auth:    auth,
		Tenants: tenants,
		Keys:    keys,
		Host:    &Host{URL: parsed, Token: "host-token", Transport: http.DefaultTransport},
		Audit:   audit,
	}, plaintext, row.ID
}

// An agent reads the contract without a credential, and drives the same
// operations as tools with one.
func TestAgentSurface(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(TenantHeader) == "" {
			t.Errorf("the host call names no tenant")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"sbx-1","state":"running"}`))
	}))
	defer host.Close()
	srv, key, _ := newTestGateway(t, host.URL)
	srv.BaseURL = "https://api.example.com"
	public := httptest.NewServer(srv.Handler())
	defer public.Close()

	for _, path := range []string{"/openapi.json", "/llms.txt", "/llms-full.txt", "/.well-known/ai-catalog.json"} {
		resp, err := http.Get(public.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status %d", path, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Fatalf("%s is empty", path)
		}
	}

	rpc := func(t *testing.T, payload string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, public.URL+"/mcp", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	initialized := rpc(t, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	result, _ := initialized["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("initialize %+v", initialized)
	}
	listed := rpc(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, _ = listed["result"].(map[string]any)
	toolList, _ := result["tools"].([]any)
	if len(toolList) < 10 {
		t.Fatalf("the tool list holds %d tools", len(toolList))
	}
	for _, tool := range toolList {
		if tool.(map[string]any)["name"] == "createTenant" {
			t.Fatal("an operator route reached the tool list")
		}
	}
	called := rpc(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"getSandbox","arguments":{"id":"sbx-1"}}}`)
	result, _ = called["result"].(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	if structured["id"] != "sbx-1" {
		t.Fatalf("tools/call %+v", called)
	}

	// A tool call with no credential never reaches the host.
	resp, err := http.Post(public.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d for an MCP call with no credential, want 401", resp.StatusCode)
	}
}

// An MCP client finds the authorization server from a refused call, and the
// documents it needs are served without a credential.
func TestOAuthDiscovery(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer host.Close()
	srv, _, _ := newTestGateway(t, host.URL)
	srv.BaseURL = "https://api.example.com"
	public := httptest.NewServer(srv.Handler())
	defer public.Close()

	for _, path := range []string{"/v1/sandboxes", "/mcp"} {
		resp, err := http.Post(public.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s status %d, want 401", path, resp.StatusCode)
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		if !strings.Contains(challenge, "resource_metadata=") {
			t.Fatalf("%s challenge %q names no metadata document", path, challenge)
		}
	}

	resp, err := http.Get(public.URL + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var metadata struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	// The resource identifier is the issuer, because the authorization
	// server accepts its own token only when the audience names it.
	want := srv.BaseURL + "/api/auth"
	if metadata.Resource != want || len(metadata.Servers) != 1 || metadata.Servers[0] != want {
		t.Fatalf("metadata %+v, want resource and server %q", metadata, want)
	}
}

// A credential that names a person and not a tenant resolves through the
// memberships of that person. One membership is the answer.
func TestTenantFromMembership(t *testing.T) {
	seen := make(chan string, 2)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(TenantHeader)
		w.Write([]byte(`[]`))
	}))
	defer host.Close()
	srv, _, tenantID := newTestGateway(t, host.URL)
	public := httptest.NewServer(srv.Handler())
	defer public.Close()

	// Sign in as the person who owns the tenant. The session names no active
	// tenant, so the gateway reads the membership.
	body := strings.NewReader(`{"email":"operator@example.com","password":"correct horse battery staple"}`)
	req, _ := http.NewRequest(http.MethodPost, public.URL+"/api/auth/sign-in/email", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://gateway.test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in status %d", resp.StatusCode)
	}
	var session *http.Cookie
	for _, cookie := range resp.Cookies() {
		session = cookie
	}
	if session == nil {
		t.Fatal("sign in set no cookie")
	}
	call, _ := http.NewRequest(http.MethodGet, public.URL+"/v1/sandboxes", nil)
	call.AddCookie(session)
	call.Header.Set("Origin", "http://gateway.test")
	resp, err = http.DefaultClient.Do(call)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d for a session call, want 200", resp.StatusCode)
	}
	if got := <-seen; got != tenantID {
		t.Fatalf("the host saw tenant %q, want %q", got, tenantID)
	}
}

// The web UI answers the origin, and it must never shadow the API. A
// client-side route that swallowed /v1 would turn every API error into an
// HTML page.
func TestUIDoesNotShadowTheAPI(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer host.Close()
	srv, _, _ := newTestGateway(t, host.URL)
	srv.AuthPrefix = "/api/auth/"
	public := httptest.NewServer(srv.Handler())
	defer public.Close()

	// An API path with no handler is the API's 404, not the UI's index.
	for _, path := range []string{"/v1/nothing", "/api/auth/nothing", "/.well-known/nothing"} {
		resp, err := http.Get(public.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "<!doctype html") {
			t.Fatalf("%s answered with the UI index", path)
		}
	}
	// The sign-in and consent screens belong to the UI now, so the UI
	// handler answers them. This binary carries no built bundle, so it says
	// so rather than serving a blank page.
	for _, path := range []string{"/sign-in", "/consent", "/sandboxes/abc"} {
		resp, err := http.Get(public.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Fatalf("%s reached no handler", path)
		}
	}
}
