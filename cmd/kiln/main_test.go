package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alternayte/kiln/internal/apispec"
)

func invoke(t *testing.T, handler http.HandlerFunc, stdin string, args ...string) (int, string, string) {
	t.Helper()
	if handler != nil {
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		t.Setenv("KILN_URL", srv.URL)
		t.Setenv("KILN_API_KEY", "kiln_test")
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func apiError(status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"code":%q,"message":"m"}}`, code)
	}
}

// The table is a contract with every agent that branches on a code.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		args    []string
		want    int
	}{
		{"not_found", apiError(404, "not_found"), []string{"sandboxes", "get", "nope"}, 3},
		{"conflict", apiError(409, "conflict"), []string{"sandboxes", "delete", "x"}, 4},
		{"exhausted", apiError(429, "exhausted"), []string{"sandboxes", "get", "x"}, 5},
		{"invalid", apiError(400, "invalid"), []string{"sandboxes", "get", "x"}, 6},
		{"internal", apiError(500, "internal"), []string{"sandboxes", "get", "x"}, 1},
		{"usage", nil, []string{"sandboxes", "nonsense"}, 2},
		{"a missing argument", apiError(500, "internal"), []string{"sandboxes", "get"}, 2},
		{"a missing required flag", apiError(500, "internal"), []string{"templates", "create", "--name", "x"}, 2},
		{"exec on a missing sandbox", apiError(404, "not_found"), []string{"exec", "x", "--", "true"}, 125},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := invoke(t, tc.handler, "", tc.args...)
			if code != tc.want {
				t.Fatalf("exit %d, want %d", code, tc.want)
			}
		})
	}
}

func TestWaitTimeoutExits14(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
		}
		fmt.Fprint(w, `{"name":"t","state":"building"}`)
	}
	code, _, _ := invoke(t, handler, "", "templates", "create", "--name", "t", "--image", "i", "--vcpus", "1",
		"--memory-mb", "1", "--disk-mb", "1", "--egress-allow", "", "--wait", "--timeout", "1ms")
	if code != 14 {
		t.Fatalf("exit %d, want 14", code)
	}
}

func TestExecPassesTheCommandExitCode(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Errorf("exec did not ask for a stream")
		}
		var body struct {
			Cmd []string `json:"cmd"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !reflect.DeepEqual(body.Cmd, []string{"sh", "-c", "exit 7"}) {
			t.Errorf("cmd = %v", body.Cmd)
		}
		fmt.Fprint(w, "event: stdout\ndata: \"hi\\n\"\n\nevent: exit\ndata: {\"exit_code\":7,\"timed_out\":false}\n\n")
	}
	code, out, _ := invoke(t, handler, "", "exec", "x", "--", "sh", "-c", "exit 7")
	if code != 7 || out != "hi\n" {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

func TestExecOnAStoppedGatewayExits125(t *testing.T) {
	t.Setenv("KILN_URL", "http://127.0.0.1:1")
	t.Setenv("KILN_API_KEY", "k")
	if code, _, _ := invoke(t, nil, "", "exec", "x", "--", "true"); code != 125 {
		t.Fatalf("exit %d, want 125", code)
	}
}

func TestJSONFailureCarriesNext(t *testing.T) {
	code, out, _ := invoke(t, apiError(404, "not_found"), "", "sandboxes", "get", "nope", "--json")
	var got struct {
		Error struct{ Code string } `json:"error"`
		Next  string                `json:"next"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	if code != 3 || got.Error.Code != "not_found" || got.Next != "kiln sandboxes list" {
		t.Fatalf("exit %d, %+v", code, got)
	}
}

func TestEveryOperationResolvesToItsCommand(t *testing.T) {
	if err := apispec.CheckCommands(); err != nil {
		t.Fatal(err)
	}
}

func TestReadsTheHostConfigWithNoURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer op" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]string{"bearer_token": "op", "control_addr": srv.URL})
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILN_URL", "")
	t.Setenv("KILN_ROOT", root)
	if code, _, stderr := invoke(t, nil, "", "health"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestNoURLAndNoConfigIsAUsageError(t *testing.T) {
	t.Setenv("KILN_URL", "")
	t.Setenv("KILN_ROOT", t.TempDir())
	if code, _, _ := invoke(t, nil, "", "health"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

// previewAPI is the part of the host a preview touches.
type previewAPI struct {
	calls     []string
	templates map[string]map[string]any
	sandbox   map[string]any
}

func (a *previewAPI) handle(t *testing.T, buildState string, rows []map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/templates":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.templates[body["name"].(string)] = body
			w.WriteHeader(202)
			fmt.Fprint(w, `{}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/templates/"):
			fmt.Fprintf(w, `{"state":%q,"error":"boom"}`, buildState)
		case r.Method == "POST" && r.URL.Path == "/v1/sandboxes":
			_ = json.NewDecoder(r.Body).Decode(&a.sandbox)
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":"new"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/publish"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"url":"https://p%v.example.com/"}`, body["port"])
		case r.Method == "GET" && r.URL.Path == "/v1/sandboxes":
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == "GET" && r.URL.Path == "/v1/events":
			w.WriteHeader(200)
		case r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	}
}

func TestPreviewUpReplacesThePreviousPreviewOfItsGroup(t *testing.T) {
	api := &previewAPI{templates: map[string]map[string]any{}}
	rows := []map[string]any{
		{"id": "new", "template": "pr-1-new", "metadata": map[string]any{"repository": "r", "pull_request": "1"}},
		{"id": "old", "template": "pr-1-old", "metadata": map[string]any{"repository": "r", "pull_request": "1"}},
		{"id": "other", "template": "pr-2", "metadata": map[string]any{"repository": "r", "pull_request": "2"}},
	}
	code, out, stderr := invoke(t, api.handle(t, "ready", rows), "SECRET_KEY=abc=def\n\n", "preview", "up", "--json",
		"--name", "pr-1-new", "--image", "img", "--port", "8000", "--start", `/app/server --flag "two words"`,
		"--publish", "9000 9001", "--env-file", "-", "--group", "repository=r", "--group", "pull_request=1", "--metadata", "commit=abc")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
	var answer struct {
		URL       string            `json:"url"`
		URLs      map[string]string `json:"urls"`
		SandboxID string            `json:"sandbox_id"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.URL != "https://p8000.example.com/" || answer.URLs["9001"] != "https://p9001.example.com/" || answer.SandboxID != "new" {
		t.Fatalf("answer %+v", answer)
	}
	start := api.templates["pr-1-new"]["start"].([]any)
	if !reflect.DeepEqual(start, []any{"/app/server", "--flag", "two words"}) {
		t.Fatalf("start = %v", start)
	}
	if got := api.sandbox["env"].(map[string]any)["SECRET_KEY"]; got != "abc=def" {
		t.Fatalf("env value = %v", got)
	}
	if strings.Contains(stderr, "abc=def") || strings.Contains(out, "abc=def") {
		t.Fatal("an env value reached the output")
	}
	joined := strings.Join(api.calls, "\n")
	if !strings.Contains(joined, "DELETE /v1/sandboxes/old") || !strings.Contains(joined, "DELETE /v1/templates/pr-1-old") {
		t.Fatalf("the previous preview stayed:\n%s", joined)
	}
	if strings.Contains(joined, "DELETE /v1/sandboxes/new") || strings.Contains(joined, "DELETE /v1/sandboxes/other") {
		t.Fatalf("the wrong preview went:\n%s", joined)
	}
}

func TestPreviewUpRemovesTheTemplateOfAFailedBuild(t *testing.T) {
	api := &previewAPI{templates: map[string]map[string]any{}}
	code, _, _ := invoke(t, api.handle(t, "failed", nil), "", "preview", "up", "--name", "t", "--image", "i", "--port", "1")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(strings.Join(api.calls, "\n"), "DELETE /v1/templates/t") {
		t.Fatalf("the template stayed: %v", api.calls)
	}
}

func TestPreviewRefusesABadEnvLineBeforeAnyCall(t *testing.T) {
	api := &previewAPI{templates: map[string]map[string]any{}}
	code, _, _ := invoke(t, api.handle(t, "ready", nil), "NOEQUALS\n", "preview", "up", "--image", "i", "--port", "1", "--env-file", "-")
	if code != 2 || len(api.calls) != 0 {
		t.Fatalf("exit %d, calls %v", code, api.calls)
	}
}

func TestSplitWords(t *testing.T) {
	got, err := splitWords(`a 'b c' "d\"e" f\ g ""`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b c", `d"e`, "f g", ""}) {
		t.Fatalf("%q", got)
	}
	if _, err := splitWords(`a "b`); err == nil {
		t.Fatal("an open quote passed")
	}
}

func TestInitKeepsWhatItDoesNotOwn(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, "AGENTS.md")
	mcp := filepath.Join(dir, ".mcp.json")
	skill := filepath.Join(dir, ".claude", "skills", "kiln", "SKILL.md")
	os.WriteFile(agents, []byte("# Mine\nkeep this\n"), 0o644)
	os.WriteFile(mcp, []byte(`{"mcpServers":{"other":{"url":"x"}},"extra":1}`), 0o644)
	t.Setenv("KILN_API_KEY", "kiln_never_on_disk")

	if code, _, stderr := invoke(t, nil, "", "init", dir); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	first, _ := os.ReadFile(agents)
	if !strings.HasPrefix(string(first), "# Mine\nkeep this\n") || !strings.Contains(string(first), sectionBegin) {
		t.Fatalf("AGENTS.md = %q", first)
	}
	var doc map[string]any
	raw, _ := os.ReadFile(mcp)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if servers["other"] == nil || servers["kiln"] == nil || doc["extra"] == nil {
		t.Fatalf(".mcp.json = %s", raw)
	}

	// A second run changes nothing, and the section appears once.
	invoke(t, nil, "", "init", dir)
	second, _ := os.ReadFile(agents)
	if string(second) != string(first) {
		t.Fatalf("a second run changed AGENTS.md: %q", second)
	}

	// A skill a person edited stays, and --force replaces it.
	os.WriteFile(skill, []byte("my edit\n"), 0o644)
	invoke(t, nil, "", "init", dir)
	if b, _ := os.ReadFile(skill); string(b) != "my edit\n" {
		t.Fatalf("the edited skill went: %q", b)
	}
	invoke(t, nil, "", "init", dir, "--force")
	if b, _ := os.ReadFile(skill); string(b) != skillText() {
		t.Fatal("--force kept the edited skill")
	}

	// A skill from an older binary, unedited, updates with no flag.
	old := "---\nname: kiln\n---\nold\n"
	stamped := old + stampFor(old)
	os.WriteFile(skill, []byte(stamped), 0o644)
	invoke(t, nil, "", "init", dir)
	if b, _ := os.ReadFile(skill); string(b) != skillText() {
		t.Fatal("an unedited older skill did not update")
	}

	filepath.WalkDir(dir, func(path string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			if b, _ := os.ReadFile(path); strings.Contains(string(b), "kiln_never_on_disk") {
				t.Errorf("%s holds the key", path)
			}
		}
		return nil
	})
}

func TestSkillCarriesTheExitCodes(t *testing.T) {
	text := skillText()
	if strings.Contains(text, "{{") || !strings.Contains(text, apispec.ExitCodeTable()) {
		t.Fatalf("skill = %s", text)
	}
}
