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

// listViewers returns the viewers of the tenant of this call.
func (s *Server) listViewers(w http.ResponseWriter, r *http.Request) {
	if s.Viewers == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this host serves no previews, so it holds no viewer")
		return
	}
	tenant, ok := store.TenantFrom(r.Context())
	if !ok {
		tenant = store.DefaultTenant
	}
	rows, err := s.Viewers.List(r.Context(), tenant)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if rows == nil {
		rows = []store.Viewer{}
	}
	// A list route answers a bare array here, like the sandbox and template
	// lists, so one client shape reads all three.
	writeJSON(w, http.StatusOK, rows)
}

// deleteViewer revokes one viewer of the tenant of this call. The viewer is
// disabled and its sessions end, so an open preview stops.
func (s *Server) deleteViewer(w http.ResponseWriter, r *http.Request) {
	if s.Viewers == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this host serves no previews, so it holds no viewer")
		return
	}
	tenant, ok := store.TenantFrom(r.Context())
	if !ok {
		tenant = store.DefaultTenant
	}
	if err := s.Viewers.Delete(r.Context(), tenant, r.PathValue("id")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
