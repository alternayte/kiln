package gateway

import (
	"context"
	"net/http"
	"strings"

	authall "github.com/alternayte/auth-all"
	authstore "github.com/alternayte/auth-all/store"
)

// protectedResource is /.well-known/oauth-protected-resource of RFC 9728. An
// MCP client reads it, finds the authorization server, and registers itself.
func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL()
	issuer := s.issuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 issuer,
		"authorization_servers":    []any{issuer},
		"bearer_methods_supported": []any{"header"},
		"scopes_supported":         []any{"openid", "email", "sandbox", "offline_access"},
		"resource_documentation":   base + "/llms.txt",
	})
}

// requireCaller resolves the caller. An API key, an access token and a
// session all reach the same handlers. A refused call carries the challenge,
// so an agent discovers the authorization server instead of guessing.
func (s *Server) requireCaller(next http.Handler) http.Handler {
	guarded := s.Auth.RequireAuth(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guarded.ServeHTTP(&challengeWriter{ResponseWriter: w, server: s}, r)
	})
}

// challengeWriter adds the WWW-Authenticate header to a refusal, whatever
// refused it.
type challengeWriter struct {
	http.ResponseWriter
	server *Server
}

func (w *challengeWriter) WriteHeader(status int) {
	if status == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="kiln", resource_metadata="`+w.server.baseURL()+`/.well-known/oauth-protected-resource"`)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *challengeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// tenantOfCaller returns the tenant of one authenticated call.
//
// An API key names its tenant. A session names its active tenant. An access
// token names a person, so the tenant comes from the memberships of that
// person: one membership is the answer, and several need the header, because
// the gateway never guesses which tenant a call belongs to.
func (s *Server) tenantOfCaller(r *http.Request) (string, error) {
	ctx := r.Context()
	if tenant, err := tenantOf(ctx); err == nil {
		return tenant, nil
	}
	p := authall.PrincipalFrom(ctx)
	if p == nil || p.User == nil {
		return "", errTenant("the call carries no credential")
	}
	members, ok := s.Auth.Store().(authstore.MembershipStore)
	if !ok {
		return "", errTenant("this gateway holds no tenant")
	}
	held, err := members.MembershipsOfUser(ctx, p.User.ID)
	if err != nil {
		return "", err
	}
	active := make([]string, 0, len(held))
	for _, one := range held {
		if one.Status == authstore.MembershipActive {
			active = append(active, one.OrgID)
		}
	}
	asked := strings.TrimSpace(r.Header.Get(TenantHeader))
	if asked != "" {
		for _, id := range active {
			if id == asked {
				return id, nil
			}
		}
		return "", errTenant("the caller holds no membership of " + asked)
	}
	switch len(active) {
	case 0:
		return "", errTenant("the caller belongs to no tenant")
	case 1:
		return active[0], nil
	default:
		return "", errTenant("the caller belongs to several tenants, so the call needs the " + TenantHeader + " header")
	}
}

// errTenant is a caller-facing reason the tenant could not be named.
type errTenant string

func (e errTenant) Error() string { return string(e) }

// oauthRoutes mounts the metadata documents. The plugin serves the
// authorization server metadata, and the gateway serves its own.
func (s *Server) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResource)
	if s.OAuth == nil {
		return
	}
	// RFC 8414 puts the authorization server metadata at the origin root, and
	// a plugin route cannot reach it.
	mux.Handle("/.well-known/oauth-authorization-server", s.OAuth.MetadataHandler())
	mux.Handle("/.well-known/openid-configuration", s.OAuth.MetadataHandler())
	mux.Handle("/.well-known/jwks.json", s.OAuth.MetadataHandler())
}

// Cleanup removes the spent and expired rows of the authorization server.
// Auth-All runs no background work, so the daemon calls this on a timer.
func (s *Server) Cleanup(ctx context.Context) error {
	if s.OAuth == nil {
		return nil
	}
	_, err := s.OAuth.Cleanup(ctx)
	return err
}

// issuer is the identifier a client names as the resource of its token.
func (s *Server) issuer() string {
	if s.Issuer != "" {
		return strings.TrimSuffix(s.Issuer, "/")
	}
	return s.baseURL() + "/api/auth"
}
