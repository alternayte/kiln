package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	authall "github.com/alternayte/auth-all"
	"github.com/alternayte/auth-all/plugins/apikeys"
	"github.com/alternayte/auth-all/plugins/organizations"
)

// TenantHeader names the tenant of one call to the host. It repeats the
// constant in internal/api, because the gateway is the client of that API.
const TenantHeader = "X-Kiln-Tenant"

// Server is the public HTTP surface.
type Server struct {
	// Auth holds users, organizations, API keys, roles and rate limits.
	Auth *authall.Auth
	// Tenants is the tenant plugin. One auth-all tenant row is one tenant.
	Tenants *organizations.Plugin
	// Keys issues and revokes the credentials a machine caller holds.
	Keys *apikeys.Plugin
	// Host is the Kiln host this gateway serves.
	Host *Host
	// Audit records one row per proxied call.
	Audit *Audit
	// AuthPrefix is where the login routes are mounted.
	AuthPrefix string
	// OperatorRole may create tenants and read every tenant's usage.
	OperatorRole string
}

// Handler returns the public routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	prefix := s.AuthPrefix
	if prefix == "" {
		prefix = "/auth/"
	}
	mux.Handle(prefix, http.StripPrefix(strings.TrimSuffix(prefix, "/"), s.Auth.Handler()))
	mux.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	// Tenant administration is the operator's, and it creates the tenant on
	// the host in the same call.
	mux.Handle("POST /operator/tenants", s.Auth.RequireAuth(http.HandlerFunc(s.createTenant)))
	mux.Handle("GET /operator/tenants", s.Auth.RequireAuth(http.HandlerFunc(s.listTenants)))
	mux.Handle("PUT /operator/tenants/{id}/caps", s.Auth.RequireAuth(http.HandlerFunc(s.setTenantCaps)))
	// Everything under /v1 is the host API, with the same paths and bodies.
	mux.Handle("/v1/", s.Auth.RequireAuth(http.HandlerFunc(s.forward)))
	return mux
}

// tenantOf returns the tenant of one authenticated call. An API key names
// its tenant. A session names its active tenant.
func tenantOf(ctx context.Context) (string, error) {
	p := authall.PrincipalFrom(ctx)
	if p == nil {
		return "", errors.New("the call carries no credential")
	}
	if p.APIKey != nil {
		if p.APIKey.OrgID == nil || *p.APIKey.OrgID == "" {
			return "", errors.New("the key names no tenant")
		}
		return *p.APIKey.OrgID, nil
	}
	if p.Organization == nil {
		return "", errors.New("the session names no active tenant")
	}
	return p.Organization.ID, nil
}

// isOperator reports whether the caller may administer tenants.
func (s *Server) isOperator(p *authall.Principal) bool {
	role := s.OperatorRole
	if role == "" {
		role = "operator"
	}
	return p != nil && p.Role == role
}

// forward sends one call to the host. It adds the host token and the tenant,
// and it removes the caller's own credential, which the host must never see.
func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusForbidden, "invalid", err.Error())
		return
	}
	start := time.Now()
	recorder := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = s.Host.URL.Scheme
			pr.Out.URL.Host = s.Host.URL.Host
			pr.Out.Host = s.Host.URL.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("Authorization", "Bearer "+s.Host.Token)
			pr.Out.Header.Set(TenantHeader, tenant)
		},
		Transport: s.Host.Transport,
		// An exec stream and the events stream reach the caller as the host
		// writes them.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("gateway: %s %s: %v", r.Method, r.URL.Path, err)
			writeError(w, http.StatusBadGateway, "internal", "the host is not reachable")
		},
	}
	rp.ServeHTTP(recorder, r)
	if s.Audit != nil {
		p := authall.PrincipalFrom(r.Context())
		s.Audit.Record(r.Context(), Entry{
			TenantID: tenant,
			UserID:   userID(p),
			KeyID:    keyID(p),
			Method:   r.Method,
			Path:     r.URL.Path,
			Status:   recorder.status,
			Duration: time.Since(start),
			At:       time.Now().UTC(),
		})
	}
}

func userID(p *authall.Principal) string {
	if p == nil || p.User == nil {
		return ""
	}
	return p.User.ID
}

func keyID(p *authall.Principal) string {
	if p == nil || p.APIKey == nil {
		return ""
	}
	return p.APIKey.ID
}

// statusWriter remembers the status for the audit row and passes writes
// through unbuffered, so a stream stays a stream.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError uses the error shape of the host API, so one client handles both.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
