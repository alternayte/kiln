package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	authall "github.com/alternayte/auth-all"
	"github.com/alternayte/auth-all/plugins/organizations"
)

// tenantRequest creates a tenant and sets its caps. A zero cap is no limit.
type tenantRequest struct {
	Name             string `json:"name"`
	Slug             string `json:"slug"`
	MaxSandboxes     int    `json:"max_sandboxes"`
	MaxTemplates     int    `json:"max_templates"`
	MaxSnapshotBytes int64  `json:"max_snapshot_bytes"`
}

// createTenant creates the tenant here and on the host in one call, so the
// two never drift.
func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	p := authall.PrincipalFrom(r.Context())
	if !s.isOperator(p) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	var req tenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	row, err := s.Tenants.Create(r.Context(), p.User, organizations.CreateInput{
		Name: req.Name,
		Slug: req.Slug,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	body := map[string]any{
		"id":                 row.ID,
		"name":               row.Name,
		"max_sandboxes":      req.MaxSandboxes,
		"max_templates":      req.MaxTemplates,
		"max_snapshot_bytes": req.MaxSnapshotBytes,
	}
	if _, err := s.hostCall(r.Context(), http.MethodPost, "/v1/tenants", "", body); err != nil {
		// The tenant exists in two stores, so a refusal here undoes the row
		// this call already made. A half-made tenant would take the name and
		// reach nothing.
		if undo := s.Tenants.Delete(r.Context(), p.User, row.ID); undo != nil {
			log.Printf("gateway: the host refused tenant %s and it stays here: %v", row.ID, undo)
		}
		writeError(w, http.StatusBadGateway, "internal", fmt.Sprintf("the host refused the tenant: %v", err))
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

// listTenants returns what the host holds for every tenant.
func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	if !s.isOperator(authall.PrincipalFrom(r.Context())) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	out, err := s.hostCall(r.Context(), http.MethodGet, "/v1/tenants", "", nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// setTenantCaps replaces the caps of one tenant on the host.
func (s *Server) setTenantCaps(w http.ResponseWriter, r *http.Request) {
	if !s.isOperator(authall.PrincipalFrom(r.Context())) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	var req tenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	body := map[string]any{
		"max_sandboxes":      req.MaxSandboxes,
		"max_templates":      req.MaxTemplates,
		"max_snapshot_bytes": req.MaxSnapshotBytes,
	}
	out, err := s.hostCall(r.Context(), http.MethodPut, "/v1/tenants/"+r.PathValue("id")+"/caps", "", body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// hostCall makes one call to the host with the gateway's credential. An empty
// tenant is an operator call, which the host allows on its own routes.
func (s *Server) hostCall(ctx context.Context, method, path, tenant string, body any) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.Host.URL.String()+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Host.Token)
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set(TenantHeader, tenant)
	}
	resp, err := (&http.Client{Transport: s.Host.Transport}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("host %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(out))
	}
	return out, nil
}
