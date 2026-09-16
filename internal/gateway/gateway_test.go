package gateway

import (
	"context"
	"encoding/json"
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
		authall.WithPlugins(roles.New(roles.Hierarchy("viewer", "editor", "operator")), tenants, keys),
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
