package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateSandboxSendsTokensAndDecodes pins the SDK's request shape: field
// names are the frozen API's.
func TestCreateSandboxSendsTokensAndDecodes(t *testing.T) {
	var path, auth string
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"abc","state":"running","template":"py","lifecycle":"persistent","idle_seconds":5}`)
	}))
	defer ts.Close()

	ttl := 600
	got, err := New(ts.URL, "tok").CreateSandbox(context.Background(), SandboxRequest{
		Template:    "py",
		Lifecycle:   "persistent",
		IdleSeconds: 5,
		TTLSeconds:  &ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/sandboxes" || auth != "Bearer tok" {
		t.Fatalf("path %q, auth %q", path, auth)
	}
	if body["idle_seconds"] != float64(5) || body["ttl_seconds"] != float64(600) {
		t.Fatalf("body %v", body)
	}
	if got.ID != "abc" || got.State != "running" {
		t.Fatalf("decoded %+v", got)
	}
	// The nil form must not send a ttl field at all.
	body = nil
	if _, err := New(ts.URL, "tok").CreateSandbox(context.Background(), SandboxRequest{Template: "py", Lifecycle: "ephemeral", IdleSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if _, sent := body["ttl_seconds"]; sent {
		t.Fatalf("nil ttl_seconds sent a field: %v", body)
	}
}

// TestErrorMapping pins the stable error codes and the status helpers.
func TestErrorMapping(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no such sandbox"}}`)
	}))
	defer ts.Close()

	_, err := New(ts.URL, "tok").Sandbox(context.Background(), "missing")
	if err == nil {
		t.Fatal("Sandbox returned no error for 404")
	}
	if !NotFound(err) || Conflict(err) {
		t.Fatalf("NotFound=%t Conflict=%t for %v", NotFound(err), Conflict(err), err)
	}
	var apiErr *Error
	if !asError(err, &apiErr) || apiErr.Code != "not_found" || apiErr.Status != 404 {
		t.Fatalf("error %+v", err)
	}
}

// TestExecDecodesTruncation pins the exec result fields.
func TestExecDecodesTruncation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/abc/exec" {
			t.Errorf("path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"exit_code":3,"stdout":"out","stderr":"err","timed_out":true,"truncated":true}`)
	}))
	defer ts.Close()

	got, err := New(ts.URL, "tok").Exec(context.Background(), "abc", ExecRequest{Cmd: []string{"false"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 3 || got.Stdout != "out" || got.Stderr != "err" || !got.TimedOut || !got.Truncated {
		t.Fatalf("exec result %+v", got)
	}
}

// TestEventsParsesSSE pins the stream decoder.
func TestEventsParsesSSE(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: transition
data: {"id":1,"sandbox_id":"abc","from_state":"running","to_state":"sleeping","reason":"sleep","at":"2026-09-14T00:00:00Z"}

event: transition
data: {"id":2,"sandbox_id":"abc","from_state":"sleeping","to_state":"waking","reason":"wake","at":"2026-09-14T00:00:01Z"}

`)
	}))
	defer ts.Close()

	var got []string
	err := New(ts.URL, "tok").Events(context.Background(), 0, "abc", func(e Event) error {
		got = append(got, e.FromState+">"+e.ToState)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "running>sleeping,sleeping>waking" {
		t.Fatalf("events %v", got)
	}
}

// TestEscapePathKeepsSlashes pins the guest path encoding.
func TestEscapePathKeepsSlashes(t *testing.T) {
	if got := escapePath("/work/a b/c"); got != "work/a%20b/c" {
		t.Fatalf("escapePath = %q", got)
	}
}
