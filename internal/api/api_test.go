package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

func testServer(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := &template.Manager{
		Root:  t.TempDir(),
		Store: st,
		Now:   func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	ts := httptest.NewServer((&Server{Store: st, Templates: mgr, Token: "secret", Base: context.Background()}).Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func call(t *testing.T, method, url, token string, body string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return parsed.Error.Code
}

func TestAuth(t *testing.T) {
	ts, _ := testServer(t)
	for name, token := range map[string]string{"missing": "", "wrong": "nope"} {
		resp, body := call(t, http.MethodGet, ts.URL+"/v1/templates", token, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s token: status %d, want 401", name, resp.StatusCode)
		}
		if code := errorCode(t, body); code != CodeInvalid {
			t.Fatalf("%s token: code %q, want %q", name, code, CodeInvalid)
		}
	}
	resp, _ := call(t, http.MethodGet, ts.URL+"/v1/templates", "secret", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: status %d, want 200", resp.StatusCode)
	}
}

func TestCreateTemplateRequiresEgressAllow(t *testing.T) {
	ts, _ := testServer(t)
	body := `{"name":"py312","image":"python:3.12-slim","vcpus":2,"memory_mb":512,"disk_mb":4096}`
	resp, out := call(t, http.MethodPost, ts.URL+"/v1/templates", "secret", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, out)
	}
	if code := errorCode(t, out); code != CodeInvalid {
		t.Fatalf("code %q, want %q", code, CodeInvalid)
	}
}

func TestCreateTemplateRejectsUnknownFields(t *testing.T) {
	ts, _ := testServer(t)
	body := `{"name":"py312","image":"i","vcpus":1,"memory_mb":1,"disk_mb":1,"egress_allow":[],"extra":true}`
	resp, _ := call(t, http.MethodPost, ts.URL+"/v1/templates", "secret", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestCreateTemplateConflictOnReady(t *testing.T) {
	ts, st := testServer(t)
	row := store.Template{
		Name: "py312", ImageRef: "i", VCPUs: 1, MemoryMB: 1, DiskMB: 1,
		EgressAllow: []string{}, State: store.TemplateReady,
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := st.CreateTemplate(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"py312","image":"i","vcpus":1,"memory_mb":1,"disk_mb":1,"egress_allow":[]}`
	resp, out := call(t, http.MethodPost, ts.URL+"/v1/templates", "secret", body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", resp.StatusCode, out)
	}
	if code := errorCode(t, out); code != CodeConflict {
		t.Fatalf("code %q, want %q", code, CodeConflict)
	}
}

func TestGetTemplateNotFound(t *testing.T) {
	ts, _ := testServer(t)
	resp, out := call(t, http.MethodGet, ts.URL+"/v1/templates/absent", "secret", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	if code := errorCode(t, out); code != CodeNotFound {
		t.Fatalf("code %q, want %q", code, CodeNotFound)
	}
}

func TestDeleteTemplate(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	for _, state := range []string{store.TemplateBuilding, store.TemplateReady} {
		row := store.Template{
			Name: state, ImageRef: "i", VCPUs: 1, MemoryMB: 1, DiskMB: 1,
			EgressAllow: []string{}, State: state,
			CreatedAt: time.Unix(1700000000, 0).UTC(),
		}
		if err := st.CreateTemplate(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	resp, out := call(t, http.MethodDelete, ts.URL+"/v1/templates/"+store.TemplateBuilding, "secret", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete building: status %d, want 409: %s", resp.StatusCode, out)
	}
	resp, _ = call(t, http.MethodDelete, ts.URL+"/v1/templates/"+store.TemplateReady, "secret", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete ready: status %d, want 204", resp.StatusCode)
	}
	resp, _ = call(t, http.MethodGet, ts.URL+"/v1/templates/"+store.TemplateReady, "secret", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, want 404", resp.StatusCode)
	}
}

func TestListTemplates(t *testing.T) {
	ts, _ := testServer(t)
	resp, out := call(t, http.MethodGet, ts.URL+"/v1/templates", "secret", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var list []map[string]any
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("list %q: %v", out, err)
	}
	if len(list) != 0 {
		t.Fatalf("list %v, want empty", list)
	}
}
