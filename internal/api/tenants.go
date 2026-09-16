package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/alternayte/kiln/internal/store"
)

// tenantJSON is one tenant and its caps. A zero cap is no limit.
type tenantJSON struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	MaxSandboxes     int       `json:"max_sandboxes"`
	MaxTemplates     int       `json:"max_templates"`
	MaxSnapshotBytes int64     `json:"max_snapshot_bytes"`
	Sandboxes        int       `json:"sandboxes"`
	Templates        int       `json:"templates"`
	SnapshotBytes    int64     `json:"snapshot_bytes"`
	CreatedAt        time.Time `json:"created_at"`
}

type tenantRequest struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	MaxSandboxes     int    `json:"max_sandboxes"`
	MaxTemplates     int    `json:"max_templates"`
	MaxSnapshotBytes int64  `json:"max_snapshot_bytes"`
}

// operatorOnly refuses a tenant-scoped call. One tenant never reads or
// changes the tenant list.
func (s *Server) operatorOnly(w http.ResponseWriter, r *http.Request) bool {
	if _, scoped := store.TenantFrom(r.Context()); scoped {
		s.writeErr(w, store.ErrNotFound)
		return false
	}
	return true
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	if !s.operatorOnly(w, r) {
		return
	}
	var req tenantRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalid, "id is required")
		return
	}
	name := req.Name
	if name == "" {
		name = req.ID
	}
	row := store.Tenant{
		ID:   req.ID,
		Name: name,
		Caps: store.Caps{
			MaxSandboxes:     req.MaxSandboxes,
			MaxTemplates:     req.MaxTemplates,
			MaxSnapshotBytes: req.MaxSnapshotBytes,
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Store.CreateTenant(r.Context(), row); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tenantJSON{
		ID: row.ID, Name: row.Name,
		MaxSandboxes: row.Caps.MaxSandboxes, MaxTemplates: row.Caps.MaxTemplates,
		MaxSnapshotBytes: row.Caps.MaxSnapshotBytes, CreatedAt: row.CreatedAt,
	})
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	if !s.operatorOnly(w, r) {
		return
	}
	rows, err := s.Store.ListTenants(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]tenantJSON, 0, len(rows))
	for _, row := range rows {
		one, err := s.tenantJSON(r, row)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		out = append(out, one)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	if !s.operatorOnly(w, r) {
		return
	}
	row, err := s.Store.GetTenant(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	one, err := s.tenantJSON(r, row)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, one)
}

// setTenantCaps replaces the caps of one tenant.
func (s *Server) setTenantCaps(w http.ResponseWriter, r *http.Request) {
	if !s.operatorOnly(w, r) {
		return
	}
	var req tenantRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	id := r.PathValue("id")
	caps := store.Caps{
		MaxSandboxes:     req.MaxSandboxes,
		MaxTemplates:     req.MaxTemplates,
		MaxSnapshotBytes: req.MaxSnapshotBytes,
	}
	if err := s.Store.SetTenantCaps(r.Context(), id, caps); err != nil {
		s.writeErr(w, err)
		return
	}
	row, err := s.Store.GetTenant(r.Context(), id)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	one, err := s.tenantJSON(r, row)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, one)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	if !s.operatorOnly(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == store.DefaultTenant {
		s.writeErr(w, errors.New("the default tenant is never deleted"))
		return
	}
	if err := s.Store.DeleteTenant(r.Context(), id); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) tenantJSON(r *http.Request, row store.Tenant) (tenantJSON, error) {
	usage, err := s.Store.TenantUsage(r.Context(), row.ID)
	if err != nil {
		return tenantJSON{}, err
	}
	return tenantJSON{
		ID:               row.ID,
		Name:             row.Name,
		MaxSandboxes:     row.Caps.MaxSandboxes,
		MaxTemplates:     row.Caps.MaxTemplates,
		MaxSnapshotBytes: row.Caps.MaxSnapshotBytes,
		Sandboxes:        usage.Sandboxes,
		Templates:        usage.Templates,
		SnapshotBytes:    usage.SnapshotBytes,
		CreatedAt:        row.CreatedAt,
	}, nil
}
