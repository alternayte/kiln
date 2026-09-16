package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

// serverToken is the operator token every test here sends.
const serverToken = "operator-token"

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
	sbx := sandbox.New(sandbox.Config{
		Root:    t.TempDir(),
		Store:   st,
		Secrets: map[string]string{"S": "v"},
		Zone:    "example.com",
	})
	ts := httptest.NewServer((&Server{Store: st, Templates: mgr, Sandboxes: sbx, Token: serverToken, Base: context.Background()}).Handler())
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
	resp, _ := call(t, http.MethodGet, ts.URL+"/v1/templates", serverToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: status %d, want 200", resp.StatusCode)
	}
}

func TestCreateTemplateRequiresEgressAllow(t *testing.T) {
	ts, _ := testServer(t)
	body := `{"name":"py312","image":"python:3.12-slim","vcpus":2,"memory_mb":512,"disk_mb":4096}`
	resp, out := call(t, http.MethodPost, ts.URL+"/v1/templates", serverToken, body)
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
	resp, _ := call(t, http.MethodPost, ts.URL+"/v1/templates", serverToken, body)
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
	resp, out := call(t, http.MethodPost, ts.URL+"/v1/templates", serverToken, body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", resp.StatusCode, out)
	}
	if code := errorCode(t, out); code != CodeConflict {
		t.Fatalf("code %q, want %q", code, CodeConflict)
	}
}

func TestGetTemplateNotFound(t *testing.T) {
	ts, _ := testServer(t)
	resp, out := call(t, http.MethodGet, ts.URL+"/v1/templates/absent", serverToken, "")
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
	resp, out := call(t, http.MethodDelete, ts.URL+"/v1/templates/"+store.TemplateBuilding, serverToken, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete building: status %d, want 409: %s", resp.StatusCode, out)
	}
	resp, _ = call(t, http.MethodDelete, ts.URL+"/v1/templates/"+store.TemplateReady, serverToken, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete ready: status %d, want 204", resp.StatusCode)
	}
	resp, _ = call(t, http.MethodGet, ts.URL+"/v1/templates/"+store.TemplateReady, serverToken, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, want 404", resp.StatusCode)
	}
}

func TestListTemplates(t *testing.T) {
	ts, _ := testServer(t)
	resp, out := call(t, http.MethodGet, ts.URL+"/v1/templates", serverToken, "")
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

func TestCreateSandboxValidation(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	row := store.Template{
		Name: "py312", ImageRef: "i", VCPUs: 1, MemoryMB: 1, DiskMB: 1,
		EgressAllow: []string{}, State: store.TemplateReady,
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := st.CreateTemplate(ctx, row); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body string
		code string
	}{
		{
			name: "missing idle_seconds",
			body: `{"template":"py312","lifecycle":"ephemeral"}`,
			code: CodeInvalid,
		},
		{
			name: "unknown lifecycle",
			body: `{"template":"py312","lifecycle":"forever","idle_seconds":60}`,
			code: CodeInvalid,
		},
		{
			name: "unknown secret",
			body: `{"template":"py312","lifecycle":"ephemeral","idle_seconds":60,"secrets":["NOPE"]}`,
			code: CodeInvalid,
		},
		{
			name: "unknown template",
			body: `{"template":"absent","lifecycle":"ephemeral","idle_seconds":60}`,
			code: CodeNotFound,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, out := call(t, http.MethodPost, ts.URL+"/v1/sandboxes", serverToken, c.body)
			if code := errorCode(t, out); code != c.code {
				t.Fatalf("status %d code %q, want %q: %s", resp.StatusCode, code, c.code, out)
			}
		})
	}
}

func TestSandboxNotFound(t *testing.T) {
	ts, _ := testServer(t)
	resp, out := call(t, http.MethodGet, ts.URL+"/v1/sandboxes/absent", serverToken, "")
	if resp.StatusCode != http.StatusNotFound || errorCode(t, out) != CodeNotFound {
		t.Fatalf("get: status %d body %s", resp.StatusCode, out)
	}
	resp, _ = call(t, http.MethodDelete, ts.URL+"/v1/sandboxes/absent", serverToken, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete: status %d, want 404", resp.StatusCode)
	}
}

func TestSnapshotListing(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	if err := st.CreateTemplate(ctx, store.Template{
		Name: "py312", ImageRef: "i", VCPUs: 1, MemoryMB: 1, DiskMB: 1,
		EgressAllow: []string{}, State: store.TemplateReady,
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	created := time.Unix(1700000000, 0).UTC()
	for _, snap := range []store.Snapshot{
		{ID: "listed", TemplateName: "py312", SizeBytes: 5, CreatedAt: created, Listed: true, OriginSandboxID: "one"},
		{ID: "hidden", TemplateName: "py312", SizeBytes: 6, CreatedAt: created, Listed: false, OriginSandboxID: "one"},
	} {
		if err := st.CreateSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	resp, out := call(t, http.MethodGet, ts.URL+"/v1/snapshots", serverToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d: %s", resp.StatusCode, out)
	}
	var list []snapshotView
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("list %q: %v", out, err)
	}
	if len(list) != 1 || list[0].ID != "listed" {
		t.Fatalf("list %+v, want only the listed image", list)
	}
	if resp, _ := call(t, http.MethodGet, ts.URL+"/v1/snapshots/hidden", serverToken, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get hidden: status %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, http.MethodDelete, ts.URL+"/v1/snapshots/hidden", serverToken, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete hidden: status %d, want 404", resp.StatusCode)
	}
}

func TestForkAndRestoreValidation(t *testing.T) {
	ts, _ := testServer(t)
	if resp, out := call(t, http.MethodPost, ts.URL+"/v1/sandboxes/absent/fork", serverToken, `{"count":1}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("fork absent: status %d, want 404: %s", resp.StatusCode, out)
	}
	if resp, out := call(t, http.MethodPost, ts.URL+"/v1/sandboxes/absent/fork", serverToken, `{"count":0}`); resp.StatusCode != http.StatusBadRequest || errorCode(t, out) != CodeInvalid {
		t.Fatalf("fork with count 0: status %d body %s", resp.StatusCode, out)
	}
	if resp, _ := call(t, http.MethodPost, ts.URL+"/v1/snapshots/absent/restore", serverToken, `{"count":1}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore absent: status %d, want 404", resp.StatusCode)
	}
	if resp, out := call(t, http.MethodPost, ts.URL+"/v1/snapshots/absent/restore", serverToken, `{"count":1,"extra":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("restore unknown field: status %d, want 400: %s", resp.StatusCode, out)
	}
}

func TestSnapshotBodyForms(t *testing.T) {
	ts, _ := testServer(t)
	// An empty body and an empty object both parse; the absent sandbox is the
	// not-found answer, which proves the request was read.
	for _, body := range []string{"", "{}", `{"stop":true}`} {
		if resp, out := call(t, http.MethodPost, ts.URL+"/v1/sandboxes/absent/snapshot", serverToken, body); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("snapshot with body %q: status %d, want 404: %s", body, resp.StatusCode, out)
		}
	}
	for _, body := range []string{`{"stop":"sometimes"}`, `{"extra":1}`} {
		if resp, out := call(t, http.MethodPost, ts.URL+"/v1/sandboxes/absent/snapshot", serverToken, body); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("snapshot with body %q: status %d, want 400: %s", body, resp.StatusCode, out)
		}
	}
}

func TestGuestFilePath(t *testing.T) {
	for _, bad := range []string{"", "..", "a/../b", "."} {
		if _, err := guestFilePath(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	got, err := guestFilePath("work/marker")
	if err != nil || got != "/work/marker" {
		t.Fatalf("got %q err %v", got, err)
	}
}

// TestStatusPageIsLocalAndReadOnly pins the control page: no token, HTML
// only, and no form or script.
func TestStatusPageIsLocalAndReadOnly(t *testing.T) {
	ts, _ := testServer(t)
	resp, body := call(t, http.MethodGet, ts.URL+"/", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	page := string(body)
	if !strings.Contains(page, "<!doctype html>") || !strings.Contains(page, "Kiln") {
		t.Fatalf("the status page is not HTML: %q", page)
	}
	for _, banned := range []string{"<form", "<script", "<button", "<input"} {
		if strings.Contains(page, banned) {
			t.Fatalf("the status page carries %s", banned)
		}
	}
}

// TestPublishValidation pins the publish contract: visibility is required,
// and an unknown sandbox is not found.
func TestPublishValidation(t *testing.T) {
	ts, _ := testServer(t)
	resp, body := call(t, http.MethodPost, ts.URL+"/v1/sandboxes/missing/publish", serverToken, `{"port":8000}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing visibility: status %d: %s", resp.StatusCode, body)
	}
	resp, body = call(t, http.MethodPost, ts.URL+"/v1/sandboxes/missing/publish", serverToken, `{"port":8000,"visibility":"public"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown sandbox: status %d: %s", resp.StatusCode, body)
	}
	resp, body = call(t, http.MethodDelete, ts.URL+"/v1/sandboxes/missing/publish/8000", serverToken, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("retire unknown: status %d: %s", resp.StatusCode, body)
	}
}

// A build outlives its request, so the tenant has to travel with it. The
// first version ran the build on the daemon context and wrote every template
// to the default tenant, which also made the cap unreachable.
func TestTemplateBuildKeepsTheTenantAndItsCap(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	if err := st.CreateTenant(ctx, store.Tenant{
		ID: "acme", Name: "Acme",
		Caps:      store.Caps{MaxTemplates: 1},
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	create := func(name string) int {
		body := `{"name":"` + name + `","image":"docker.io/library/python:3.12-slim",` +
			`"vcpus":1,"memory_mb":256,"disk_mb":2048,"setup":[],"egress_allow":[]}`
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/templates", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+serverToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(TenantHeader, "acme")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if status := create("one"); status != http.StatusAccepted {
		t.Fatalf("first build status %d, want 202", status)
	}
	// The build runs in the background, so wait for the row it writes.
	var rows []store.Template
	for i := 0; i < 100; i++ {
		var err error
		rows, err = st.ListTemplates(store.WithTenant(ctx, "acme"))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 1 || rows[0].Name != "one" {
		t.Fatalf("the tenant holds %+v, want the template it created", rows)
	}
	if status := create("two"); status != http.StatusInsufficientStorage {
		t.Fatalf("second build status %d, want 507 over the cap", status)
	}
}
