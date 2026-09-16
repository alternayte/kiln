package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/alternayte/kiln/internal/store"
)

// registryRequest stores one credential for one tenant.
type registryRequest struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// registryResponse never carries the token. A credential goes in and is used
// on the host; nothing reads it back out.
type registryResponse struct {
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// putRegistry stores the credential a template build uses to pull a private
// image for this tenant.
func (s *Server) putRegistry(w http.ResponseWriter, r *http.Request) {
	var req registryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" || req.Username == "" || req.Token == "" {
		writeError(w, http.StatusBadRequest, CodeInvalid, "host, username and token are required")
		return
	}
	// The host of a reference is the registry, so a value with a path or a
	// scheme would never match a pull.
	if strings.ContainsAny(req.Host, "/ :") {
		writeError(w, http.StatusBadRequest, CodeInvalid,
			"host is a registry hostname, such as ghcr.io, with no scheme and no path")
		return
	}
	sealed, err := store.Seal(s.SealKey, req.Token)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	row := store.Registry{
		Host:      req.Host,
		Username:  req.Username,
		Token:     sealed,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Store.PutRegistry(r.Context(), row); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, registryResponse{
		Host: row.Host, Username: row.Username, CreatedAt: row.CreatedAt,
	})
}

// listRegistries returns the credentials of this tenant without their tokens.
func (s *Server) listRegistries(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListRegistries(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]registryResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, registryResponse{
			Host: row.Host, Username: row.Username, CreatedAt: row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteRegistry removes one credential. A build that needed it falls back to
// an anonymous pull, which a public image still allows.
func (s *Server) deleteRegistry(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteRegistry(r.Context(), r.PathValue("host")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
