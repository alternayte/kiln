package api

import (
	"net/http"
	"strconv"
)

// publishRequest is one POST /v1/sandboxes/{id}/publish body. Visibility is
// required and has no default.
type publishRequest struct {
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
}

// publishCreatedResponse is the answer to a publish call.
type publishCreatedResponse struct {
	URL        string `json:"url"`
	Visibility string `json:"visibility"`
}

// publishedResponse is one preview in a sandbox view.
type publishedResponse struct {
	Port       int    `json:"port"`
	URL        string `json:"url"`
	Visibility string `json:"visibility"`
}

// publishSandbox serves one guest port on a hostname. Republishing the same
// port returns the same hostname.
func (s *Server) publishSandbox(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if req.Visibility == "" {
		writeError(w, http.StatusBadRequest, CodeInvalid, "visibility is required")
		return
	}
	row, err := s.Sandboxes.Publish(r.Context(), r.PathValue("id"), req.Port, req.Visibility)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, publishCreatedResponse{
		URL:        s.Sandboxes.PublishedURL(row),
		Visibility: row.Visibility,
	})
}

// unpublishSandbox retires one hostname. It 404s from then on and is never
// handed out again.
func (s *Server) unpublishSandbox(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, CodeInvalid, "port must be a number between 1 and 65535")
		return
	}
	if err := s.Sandboxes.Retire(r.Context(), r.PathValue("id"), port); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
