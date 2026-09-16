package gateway

import (
	"net/http"

	authall "github.com/alternayte/auth-all"
)

// meResponse is what the web UI needs and the auth-all client cannot give:
// which tenant the session acts on, the role it holds there, and whether it
// administers tenants. Everything else about the person comes from the
// auth-all session.
type meResponse struct {
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
	TenantID   string `json:"tenant_id,omitempty"`
	TenantName string `json:"tenant_name,omitempty"`
	Role       string `json:"role,omitempty"`
	IsOperator bool   `json:"is_operator"`
}

// me describes the caller. The UI hides a control the role cannot use; the
// server still refuses the call, so this is a convenience and not a check.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := authall.PrincipalFrom(r.Context())
	if p == nil || p.User == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "the call carries no session")
		return
	}
	out := meResponse{
		UserID:     p.User.ID,
		Email:      p.User.Email,
		Role:       p.Role,
		IsOperator: s.isOperator(p),
	}
	if p.Organization != nil {
		out.TenantID = p.Organization.ID
		out.TenantName = p.Organization.Name
	}
	writeJSON(w, http.StatusOK, out)
}
