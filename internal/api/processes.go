package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/runtime"
)

// egressRequest is the PUT /v1/sandboxes/{id}/egress body.
type egressRequest struct {
	EgressAllow *[]string `json:"egress_allow"`
}

// setSandboxEgress replaces the egress policy of one sandbox.
func (s *Server) setSandboxEgress(w http.ResponseWriter, r *http.Request) {
	var req egressRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if req.EgressAllow == nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, "egress_allow is required and may be empty")
		return
	}
	row, err := s.Sandboxes.SetEgress(r.Context(), r.PathValue("id"), *req.EgressAllow)
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

// processRequest is the POST /v1/sandboxes/{id}/processes body.
type processRequest struct {
	Cmd []string          `json:"cmd"`
	Cwd string            `json:"cwd"`
	Env map[string]string `json:"env"`
}

// processResponse is one read of a detached process. ExitCode is null while
// the process runs.
type processResponse struct {
	ID           string `json:"id"`
	Running      bool   `json:"running"`
	ExitCode     *int   `json:"exit_code"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	StdoutOffset int64  `json:"stdout_offset"`
	StderrOffset int64  `json:"stderr_offset"`
	StdoutLost   int64  `json:"stdout_lost"`
	StderrLost   int64  `json:"stderr_lost"`
	More         bool   `json:"more"`
}

// startProcess starts a detached process.
func (s *Server) startProcess(w http.ResponseWriter, r *http.Request) {
	var req processRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if len(req.Cmd) == 0 {
		writeError(w, http.StatusBadRequest, CodeInvalid, "cmd is required")
		return
	}
	id, err := s.Sandboxes.StartProcess(r.Context(), r.PathValue("id"), runtime.ProcessRequest{
		Cmd: req.Cmd, Cwd: req.Cwd, Env: req.Env,
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// offsetQuery reads one optional output offset.
func offsetQuery(r *http.Request, name string) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a number that is not negative", name)
	}
	return n, nil
}

// readProcess returns the state of a detached process and its output after
// each offset.
func (s *Server) readProcess(w http.ResponseWriter, r *http.Request) {
	stdout, err := offsetQuery(r, "stdout_offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	stderr, err := offsetQuery(r, "stderr_offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	out, err := s.Sandboxes.ReadProcess(r.Context(), r.PathValue("id"), r.PathValue("pid"), stdout, stderr)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	res := processResponse{
		ID:           out.ID,
		Running:      out.Running,
		Stdout:       out.Stdout,
		Stderr:       out.Stderr,
		StdoutOffset: out.StdoutOffset,
		StderrOffset: out.StderrOffset,
		StdoutLost:   out.StdoutLost,
		StderrLost:   out.StderrLost,
		More:         out.More,
	}
	if !out.Running {
		code := out.ExitCode
		res.ExitCode = &code
	}
	writeJSON(w, http.StatusOK, res)
}

// signalProcess signals the process group of a detached process.
func (s *Server) signalProcess(w http.ResponseWriter, r *http.Request) {
	signal := r.URL.Query().Get("signal")
	if signal == "" {
		signal = "TERM"
	}
	if !guestproto.ValidProcessSignal(signal) {
		writeError(w, http.StatusBadRequest, CodeInvalid, fmt.Sprintf("signal must be one of %v", guestproto.ProcessSignals))
		return
	}
	if err := s.Sandboxes.SignalProcess(r.Context(), r.PathValue("id"), r.PathValue("pid"), signal); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
