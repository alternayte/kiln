package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
)

// execOutputCap is the buffered response limit for each stream.
const execOutputCap = 1 << 20

type createSandboxRequest struct {
	Template    string            `json:"template"`
	Lifecycle   string            `json:"lifecycle"`
	IdleSeconds *int              `json:"idle_seconds"`
	TTLSeconds  *int              `json:"ttl_seconds"`
	Metadata    json.RawMessage   `json:"metadata"`
	Secrets     []string          `json:"secrets"`
	Env         map[string]string `json:"env"`
}

// parseMetadata reads an optional metadata field. Nil means the field is absent,
// and an empty object is a value.
func parseMetadata(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, errors.New("metadata must be a JSON object")
	}
	return metadata, nil
}

type execRequest struct {
	Cmd            []string          `json:"cmd"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

type sandboxResponse struct {
	ID           string              `json:"id"`
	State        string              `json:"state"`
	Template     string              `json:"template"`
	Lifecycle    string              `json:"lifecycle"`
	IdleSeconds  int                 `json:"idle_seconds"`
	TTLSeconds   *int                `json:"ttl_seconds"`
	Metadata     json.RawMessage     `json:"metadata"`
	CreatedAt    time.Time           `json:"created_at"`
	LastActiveAt time.Time           `json:"last_active_at"`
	DestroyedAt  *time.Time          `json:"destroyed_at,omitempty"`
	Published    []publishedResponse `json:"published"`
	// EnvKeys names the env keys. The values are never returned.
	EnvKeys []string `json:"env_keys"`
	// EgressAllow is the policy in force on this sandbox.
	EgressAllow []string `json:"egress_allow"`
	// EgressObserve lets any TLS name through and records it.
	EgressObserve bool `json:"egress_observe"`
	// Credentials names each binding. A value is never returned.
	Credentials []credentialJSON `json:"credentials"`
}

type execResponse struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// sandboxJSON renders one sandbox with its published hostnames.
func (s *Server) sandboxJSON(ctx context.Context, sb store.Sandbox) (sandboxResponse, error) {
	metadata := json.RawMessage(sb.Metadata)
	if len(metadata) == 0 || !json.Valid(metadata) {
		metadata = json.RawMessage("{}")
	}
	out := sandboxResponse{
		ID:           sb.ID,
		State:        sb.State,
		Template:     sb.TemplateName,
		Lifecycle:    sb.Lifecycle,
		IdleSeconds:  sb.IdleSeconds,
		TTLSeconds:   sb.TTLSeconds,
		Metadata:     metadata,
		CreatedAt:    sb.CreatedAt,
		LastActiveAt: sb.LastActiveAt,
		DestroyedAt:  sb.DestroyedAt,
		Published:    []publishedResponse{},
		EnvKeys:      sb.EnvKeys,

		EgressObserve: sb.EgressObserve,
	}
	if out.EnvKeys == nil {
		out.EnvKeys = []string{}
	}
	out.EgressAllow, _ = s.Sandboxes.EgressOf(ctx, sb)
	if out.EgressAllow == nil {
		out.EgressAllow = []string{}
	}
	bindings, err := s.Sandboxes.Credentials(ctx, sb.ID)
	if err != nil {
		return sandboxResponse{}, err
	}
	out.Credentials = []credentialJSON{}
	for _, b := range bindings {
		out.Credentials = append(out.Credentials, credentialJSON{
			Name: b.Name, Host: b.Host, Header: b.Header, Path: b.Path, Methods: b.Methods,
		})
	}
	rows, err := s.Sandboxes.Published(ctx, sb.ID)
	if err != nil {
		return sandboxResponse{}, err
	}
	for _, row := range rows {
		out.Published = append(out.Published, publishedResponse{
			Port:       row.GuestPort,
			URL:        s.Sandboxes.PublishedURL(row),
			Visibility: row.Visibility,
		})
	}
	return out, nil
}

// createSandbox restores a template snapshot into a running sandbox.
func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	if err := s.admit(r.Context(), wantSandbox); err != nil {
		s.writeErr(w, err)
		return
	}
	var req createSandboxRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if req.IdleSeconds == nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, "idle_seconds is required")
		return
	}
	metadata, err := parseMetadata(req.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	sb, err := s.Sandboxes.Create(r.Context(), sandbox.CreateRequest{
		Template:    req.Template,
		Lifecycle:   req.Lifecycle,
		IdleSeconds: *req.IdleSeconds,
		TTLSeconds:  req.TTLSeconds,
		Metadata:    metadata,
		Secrets:     req.Secrets,
		Env:         req.Env,
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	view, err := s.sandboxJSON(r.Context(), sb)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// listSandboxes returns every sandbox.
func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Sandboxes.List(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]sandboxResponse, 0, len(rows))
	for _, row := range rows {
		view, err := s.sandboxJSON(r.Context(), row)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}

// getSandbox returns one sandbox.
func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := s.Sandboxes.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	view, err := s.sandboxJSON(r.Context(), sb)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// deleteSandbox destroys one sandbox.
func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	if err := s.Sandboxes.Destroy(r.Context(), r.PathValue("id")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// execSandbox runs one command, buffered or streamed.
func (s *Server) execSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req execRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	run := runtime.ExecRequest{
		Cmd:            req.Cmd,
		Cwd:            req.Cwd,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamExec(w, r, id, run)
		return
	}
	var stdout, stderr bytes.Buffer
	truncated := false
	collect := func(buf *bytes.Buffer, data []byte) {
		if buf.Len() >= execOutputCap {
			truncated = true
			return
		}
		room := execOutputCap - buf.Len()
		if len(data) > room {
			truncated = true
			data = data[:room]
		}
		buf.Write(data)
	}
	res, err := s.Sandboxes.Exec(r.Context(), id, run, func(typ byte, data []byte) error {
		if typ == guestproto.FrameStderr {
			collect(&stderr, data)
		} else {
			collect(&stdout, data)
		}
		return nil
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, execResponse{
		ExitCode:  res.ExitCode,
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		TimedOut:  res.TimedOut,
		Truncated: truncated,
	})
}

// streamExec runs one command with server-sent events. A sleeping sandbox
// wakes inside Exec, so there is no state check here.
func (s *Server) streamExec(w http.ResponseWriter, r *http.Request, id string, run runtime.ExecRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	res, err := s.Sandboxes.Exec(r.Context(), id, run, func(typ byte, data []byte) error {
		name := "stdout"
		if typ == guestproto.FrameStderr {
			name = "stderr"
		}
		encoded, _ := json.Marshal(string(data))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if err != nil {
		encoded, _ := json.Marshal(err.Error())
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", encoded)
		flusher.Flush()
		return
	}
	_, _ = fmt.Fprintf(w, "event: exit\ndata: {\"exit_code\":%d,\"timed_out\":%t}\n\n", res.ExitCode, res.TimedOut)
	flusher.Flush()
}

// sandboxLogsResponse is the answer to GET /v1/sandboxes/{id}/logs.
type sandboxLogsResponse struct {
	Build logsBuild  `json:"build"`
	Start *logsStart `json:"start,omitempty"`
	// Services reports each service with its output and how it ended.
	Services []guestproto.ServiceState `json:"services,omitempty"`
	Note     string                    `json:"note,omitempty"`
}

type logsBuild struct {
	Template string `json:"template"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
}

type logsStart struct {
	Command  []string `json:"command"`
	Port     int      `json:"port"`
	Running  bool     `json:"running"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Output   string   `json:"output"`
}

// sandboxLogs answers with the output the guest kept and the build result.
func (s *Server) sandboxLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := s.Sandboxes.Logs(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := sandboxLogsResponse{Build: logsBuild{
		Template: logs.Template.Name,
		State:    logs.Template.State,
		Error:    logs.Template.Error,
	}}
	if logs.GuestTooOld {
		out.Note = "this sandbox came from a template built before this route; rebuild the template to read its output"
	}
	if logs.Start != nil {
		out.Services = logs.Start.Services
	}
	if logs.Start != nil && logs.Template.StartPort > 0 {
		start := &logsStart{
			Command: logs.Template.Start,
			Port:    logs.Template.StartPort,
			Running: !logs.Start.Exited,
			Output:  logs.Start.Output,
		}
		if logs.Start.Exited {
			code := logs.Start.ExitCode
			start.ExitCode = &code
		}
		out.Start = start
	}
	writeJSON(w, http.StatusOK, out)
}

// getSandboxFile streams one guest file.
func (s *Server) getSandboxFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	guestPath, err := guestFilePath(r.PathValue("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	file, err := s.Sandboxes.OpenFile(r.Context(), id, guestPath)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

// putSandboxFile writes one guest file.
func (s *Server) putSandboxFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	guestPath, err := guestFilePath(r.PathValue("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if err := s.Sandboxes.WriteFile(r.Context(), id, guestPath, r.Body); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guestFilePath turns the path after /files/ into a guest path.
func guestFilePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errors.New("path must not contain ..")
		}
	}
	clean := path.Clean("/" + p)
	if clean == "/" {
		return "", errors.New("path must name a file")
	}
	return clean, nil
}
