package api

import (
	"net/http"

	"github.com/alternayte/kiln/internal/store"
)

// viewerRequest creates one viewer of the calling tenant.
type viewerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// createViewer adds a viewer to the tenant of this call. A viewer opens the
// team previews of that tenant and reaches nothing else.
func (s *Server) createViewer(w http.ResponseWriter, r *http.Request) {
	if s.Viewers == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this host serves no previews, so it holds no viewer")
		return
	}
	var req viewerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, CodeInvalid, "email and password are required")
		return
	}
	tenant, ok := store.TenantFrom(r.Context())
	if !ok {
		tenant = store.DefaultTenant
	}
	exists, err := s.Viewers.Exists(r.Context(), req.Email)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if exists {
		writeError(w, http.StatusConflict, CodeConflict, "a viewer with this address exists")
		return
	}
	if err := s.Viewers.Create(r.Context(), tenant, req.Email, req.Password); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"email": req.Email, "tenant": tenant})
}
