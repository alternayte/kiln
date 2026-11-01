package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/alternayte/kiln/internal/sandbox"
)

// credentialJSON is one binding. Value comes in and never goes out.
type credentialJSON struct {
	Name    string   `json:"name"`
	Host    string   `json:"host"`
	Header  string   `json:"header"`
	Value   string   `json:"value,omitempty"`
	Path    string   `json:"path,omitempty"`
	Methods []string `json:"methods,omitempty"`
}

func credentialInputs(list []credentialJSON) []sandbox.CredentialInput {
	out := make([]sandbox.CredentialInput, 0, len(list))
	for _, c := range list {
		out = append(out, sandbox.CredentialInput{
			Name: c.Name, Host: c.Host, Header: c.Header, Value: c.Value, Path: c.Path, Methods: c.Methods,
		})
	}
	return out
}

// setSandboxCredentials replaces the credential bindings of one sandbox.
func (s *Server) setSandboxCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Credentials *[]credentialJSON `json:"credentials"`
	}
	if err := decodeJSON(r, &req); err != nil {
		// A decoder error can quote the body, and this body holds values.
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			writeError(w, http.StatusBadRequest, CodeInvalid, "the body is not JSON")
			return
		}
		writeError(w, http.StatusBadRequest, CodeInvalid, "the body must hold a credentials list")
		return
	}
	if req.Credentials == nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, "credentials is required and may be empty")
		return
	}
	row, err := s.Sandboxes.SetCredentials(r.Context(), r.PathValue("id"), credentialInputs(*req.Credentials))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out, err := s.sandboxJSON(r.Context(), row)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type egressRequestJSON struct {
	Method  string    `json:"method"`
	Host    string    `json:"host"`
	Path    string    `json:"path"`
	Status  int       `json:"status"`
	Binding string    `json:"binding"`
	At      time.Time `json:"at"`
}

// getEgressRequests returns the request log of the bound hosts of one sandbox.
func (s *Server) getEgressRequests(w http.ResponseWriter, r *http.Request) {
	log, err := s.Sandboxes.EgressRequests(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	rows := make([]egressRequestJSON, 0, len(log.Requests))
	for _, row := range log.Requests {
		rows = append(rows, egressRequestJSON{
			Method: row.Method, Host: row.Host, Path: row.Path, Status: row.Status, Binding: row.Binding, At: row.At,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": rows, "dropped": log.Dropped})
}
