// Package api serves the Kiln HTTP surface. The control listener is the only
// place these routes exist.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

// Error codes are stable and part of the API.
const (
	CodeNotFound  = "not_found"
	CodeInvalid   = "invalid"
	CodeConflict  = "conflict"
	CodeExhausted = "exhausted"
	CodeInternal  = "internal"
)

// Server holds the API dependencies.
type Server struct {
	Store     store.Store
	Templates *template.Manager
	Sandboxes *sandbox.Manager
	Token     string
	// FirecrackerVersion is the pinned version GET /v1/health reports.
	FirecrackerVersion string
	// Root is the Kiln root, for the status page's disk use.
	Root string
	// Base is the lifetime context for asynchronous work.
	Base context.Context
	// Viewers holds the people who open team previews. Nil on a host that
	// serves no preview.
	Viewers ViewerStore
}

// ViewerStore is the part of the viewer login the API touches. The ingress
// implements it; the interface keeps this package free of that import.
type ViewerStore interface {
	// Create adds one viewer of one tenant.
	Create(ctx context.Context, tenant, address, password string) error
	// Exists reports whether an address already holds a viewer.
	Exists(ctx context.Context, address string) (bool, error)
}

// Handler returns the control surface: the /v1 API behind bearer
// authentication and the read-only status page at /.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/events", s.events)
	mux.HandleFunc("GET /v1/templates", s.listTemplates)
	mux.HandleFunc("POST /v1/templates", s.createTemplate)
	mux.HandleFunc("GET /v1/templates/{name}", s.getTemplate)
	mux.HandleFunc("DELETE /v1/templates/{name}", s.deleteTemplate)
	mux.HandleFunc("GET /v1/sandboxes", s.listSandboxes)
	mux.HandleFunc("POST /v1/sandboxes", s.createSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.getSandbox)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.deleteSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.execSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot", s.createSnapshot)
	mux.HandleFunc("POST /v1/sandboxes/{id}/fork", s.forkSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{id}/publish", s.publishSandbox)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/publish/{port}", s.unpublishSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}/files/{path...}", s.getSandboxFile)
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files/{path...}", s.putSandboxFile)
	mux.HandleFunc("POST /v1/viewers", s.createViewer)
	mux.HandleFunc("POST /v1/tenants", s.createTenant)
	mux.HandleFunc("GET /v1/tenants", s.listTenants)
	mux.HandleFunc("GET /v1/tenants/{id}", s.getTenant)
	mux.HandleFunc("PUT /v1/tenants/{id}/caps", s.setTenantCaps)
	mux.HandleFunc("DELETE /v1/tenants/{id}", s.deleteTenant)
	mux.HandleFunc("GET /v1/snapshots", s.listSnapshots)
	mux.HandleFunc("GET /v1/snapshots/{id}", s.getSnapshot)
	mux.HandleFunc("DELETE /v1/snapshots/{id}", s.deleteSnapshot)
	mux.HandleFunc("POST /v1/snapshots/{id}/restore", s.restoreSnapshot)
	// The status page is local to the control listener, so it needs no
	// token. Everything under /v1 keeps the bearer check.
	root := http.NewServeMux()
	root.HandleFunc("GET /{$}", s.statusPage)
	root.Handle("/v1/", s.auth(mux))
	return root
}

func (s *Server) base() context.Context {
	if s.Base != nil {
		return s.Base
	}
	return context.Background()
}

// auth compares the bearer token as a digest, in constant time.
func (s *Server) auth(next http.Handler) http.Handler {
	want := sha256.Sum256([]byte("Bearer " + s.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeError(w, http.StatusUnauthorized, CodeInvalid, "missing or invalid bearer token")
			return
		}
		// Every handler below reads the request context, so the scope is
		// set once here and no query can forget it.
		ctx, err := s.tenant(r)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr maps a domain error to its status and stable code.
func (s *Server) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
		return
	case errors.Is(err, sandbox.ErrExhausted):
		writeError(w, http.StatusInsufficientStorage, CodeExhausted, err.Error())
		return
	case template.IsInvalid(err), sandbox.IsInvalid(err):
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	var ee *runtime.ExecError
	if errors.As(err, &ee) {
		switch ee.Code {
		case guestproto.CodeNotFound:
			writeError(w, http.StatusNotFound, CodeNotFound, ee.Message)
		case guestproto.CodeInvalid:
			writeError(w, http.StatusBadRequest, CodeInvalid, ee.Message)
		default:
			writeError(w, http.StatusInternalServerError, CodeInternal, ee.Message)
		}
		return
	}
	writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
}

// decodeJSON accepts one JSON object of at most 1 MiB with no unknown fields.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// decodeOptionalJSON is decodeJSON for a body every field of which is
// optional, so an empty body takes the zero value.
func decodeOptionalJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}
