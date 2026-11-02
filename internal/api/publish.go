package api

import (
	"net/http"
	"strconv"

	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
)

// publishRequest is one POST /v1/sandboxes/{id}/publish body. Visibility is
// required and has no default.
type publishRequest struct {
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
	// The rest make one fork for each viewer login.
	ViewerFork        bool             `json:"viewer_fork"`
	ViewerCap         int              `json:"viewer_cap"`
	AllowSecretFork   bool             `json:"allow_secret_fork"`
	ViewerCredentials []credentialJSON `json:"viewer_credentials"`
}

// publishCreatedResponse is the answer to a publish call.
type publishCreatedResponse struct {
	URL        string `json:"url"`
	Visibility string `json:"visibility"`
	// ViewerCap and ViewerResetURL are set on a viewer fork origin.
	ViewerCap      int    `json:"viewer_cap,omitempty"`
	ViewerResetURL string `json:"viewer_reset_url,omitempty"`
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
	if req.ViewerFork {
		s.publishViewerFork(w, r, req)
		return
	}
	if req.ViewerCap != 0 || req.AllowSecretFork || len(req.ViewerCredentials) > 0 {
		writeError(w, http.StatusBadRequest, CodeInvalid, "viewer_cap, allow_secret_fork and viewer_credentials need viewer_fork")
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

// publishViewerFork publishes a port whose hostname gives each viewer login a
// fork. An anonymous request must not start a microVM, so it needs team.
func (s *Server) publishViewerFork(w http.ResponseWriter, r *http.Request, req publishRequest) {
	if req.Visibility != store.VisibilityTeam {
		writeError(w, http.StatusBadRequest, CodeInvalid, "viewer_fork needs visibility team")
		return
	}
	if req.ViewerCap < 1 {
		writeError(w, http.StatusBadRequest, CodeInvalid, "viewer_cap is required with viewer_fork, and must be greater than zero")
		return
	}
	if err := s.admit(r.Context(), wantSnapshot); err != nil {
		s.writeErr(w, err)
		return
	}
	row, err := s.Sandboxes.PublishViewerFork(r.Context(), r.PathValue("id"), req.Port, sandbox.ViewerForkOptions{
		Cap: req.ViewerCap, AllowSecretFork: req.AllowSecretFork, Credentials: credentialInputs(req.ViewerCredentials),
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	url := s.Sandboxes.PublishedURL(row)
	writeJSON(w, http.StatusCreated, publishCreatedResponse{
		URL: url, Visibility: row.Visibility, ViewerCap: req.ViewerCap, ViewerResetURL: url + "_kiln/fork",
	})
}

// viewerForkResponse is one viewer and its live fork.
type viewerForkResponse struct {
	ViewerID string `json:"viewer_id"`
	ForkID   string `json:"fork_id"`
}

// listViewerForks names the live fork of each viewer of one origin.
func (s *Server) listViewerForks(w http.ResponseWriter, r *http.Request) {
	forks, err := s.Sandboxes.ViewerForks(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]viewerForkResponse, 0, len(forks))
	for _, fork := range forks {
		out = append(out, viewerForkResponse{ViewerID: fork.ViewerID, ForkID: fork.ForkID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewer_forks": out})
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
