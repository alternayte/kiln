// Package api serves the Kiln HTTP surface. The control listener is the only
// place these routes exist.
package api

import (
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
	// Base is the lifetime context for asynchronous work.
	Base context.Context
}

// Handler returns the /v1 surface behind bearer authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/templates", s.listTemplates)
	mux.HandleFunc("POST /v1/templates", s.createTemplate)
	mux.HandleFunc("GET /v1/templates/{name}", s.getTemplate)
	mux.HandleFunc("DELETE /v1/templates/{name}", s.deleteTemplate)
	mux.HandleFunc("GET /v1/sandboxes", s.listSandboxes)
	mux.HandleFunc("POST /v1/sandboxes", s.createSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.getSandbox)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.deleteSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.execSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}/files/{path...}", s.getSandboxFile)
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files/{path...}", s.putSandboxFile)
	return s.auth(mux)
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
		next.ServeHTTP(w, r)
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
