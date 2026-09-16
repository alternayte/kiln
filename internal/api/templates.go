package api

import (
	"net/http"
	"time"

	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

// createTemplateRequest is the POST /v1/templates body.
type createTemplateRequest struct {
	Name        string    `json:"name"`
	Image       string    `json:"image"`
	VCPUs       int       `json:"vcpus"`
	MemoryMB    int       `json:"memory_mb"`
	DiskMB      int       `json:"disk_mb"`
	Setup       []string  `json:"setup"`
	EgressAllow *[]string `json:"egress_allow"`
	TTLSeconds  *int      `json:"ttl_seconds"`
	Start       []string  `json:"start"`
	Port        int       `json:"port"`
}

// templateResponse is one template as the API reports it.
type templateResponse struct {
	Name          string    `json:"name"`
	Image         string    `json:"image"`
	ImageDigest   string    `json:"image_digest,omitempty"`
	VCPUs         int       `json:"vcpus"`
	MemoryMB      int       `json:"memory_mb"`
	DiskMB        int       `json:"disk_mb"`
	EgressAllow   []string  `json:"egress_allow"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	SnapshotBytes int64     `json:"snapshot_bytes"`
	Sandboxes     int       `json:"sandboxes"`
	CreatedAt     time.Time `json:"created_at"`
	TTLSeconds    *int      `json:"ttl_seconds,omitempty"`
	Start         []string  `json:"start,omitempty"`
	Port          int       `json:"port,omitempty"`
}

func templateJSON(info template.Info) templateResponse {
	egress := info.EgressAllow
	if egress == nil {
		egress = []string{}
	}
	return templateResponse{
		Name:          info.Name,
		Image:         info.ImageRef,
		ImageDigest:   info.ImageDigest,
		VCPUs:         info.VCPUs,
		MemoryMB:      info.MemoryMB,
		DiskMB:        info.DiskMB,
		EgressAllow:   egress,
		State:         info.State,
		Error:         info.Error,
		SnapshotBytes: info.SnapshotBytes,
		Sandboxes:     info.Sandboxes,
		CreatedAt:     info.CreatedAt,
		TTLSeconds:    info.TTLSeconds,
		Start:         info.Start,
		Port:          info.StartPort,
	}
}

// createTemplate starts a build and returns before it runs.
func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	if err := s.admit(r.Context(), wantTemplate); err != nil {
		s.writeErr(w, err)
		return
	}
	var req createTemplateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if req.EgressAllow == nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, "egress_allow is required and may be empty")
		return
	}
	build := template.BuildRequest{
		Name:        req.Name,
		Image:       req.Image,
		VCPUs:       req.VCPUs,
		MemoryMB:    req.MemoryMB,
		DiskMB:      req.DiskMB,
		Setup:       req.Setup,
		EgressAllow: *req.EgressAllow,
		TTLSeconds:  req.TTLSeconds,
		Start:       req.Start,
		StartPort:   req.Port,
	}
	// The build outlives the request, so it runs on the daemon context. The
	// tenant travels with it, or the rows land on the wrong tenant.
	ctx := s.base()
	if tenant, ok := store.TenantFrom(r.Context()); ok {
		ctx = store.WithTenant(ctx, tenant)
	}
	if err := s.Templates.Start(ctx, build); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"name":  build.Name,
		"state": store.TemplateBuilding,
	})
}

// listTemplates returns every template with its snapshot size and children.
func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListTemplates(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]templateResponse, 0, len(rows))
	for _, row := range rows {
		info, err := s.Templates.TemplateInfo(r.Context(), row.Name)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		out = append(out, templateJSON(info))
	}
	writeJSON(w, http.StatusOK, out)
}

// getTemplate returns one template.
func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	info, err := s.Templates.TemplateInfo(r.Context(), r.PathValue("name"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, templateJSON(info))
}

// deleteTemplate removes a template and its files.
func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	if err := s.Templates.Delete(r.Context(), r.PathValue("name")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
