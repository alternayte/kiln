package apispec

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The OpenAPI document, the MCP tools and the agent documentation come from
// this list, so a route the host serves and this list forgets is invisible to
// every client. The check runs both ways.
func TestEveryHostRouteIsDescribed(t *testing.T) {
	body, err := os.ReadFile("../api/api.go")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ /v1[^"]*)"`)
	served := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(string(body), -1) {
		served[match[1]] = true
	}
	if len(served) == 0 {
		t.Fatal("no route was found in internal/api/api.go; the pattern is stale")
	}
	described := map[string]bool{}
	for _, op := range Operations() {
		described[op.Route()] = true
	}
	for route := range served {
		if !described[route] {
			t.Errorf("the host serves %q and apispec does not describe it", route)
		}
	}
	for route := range described {
		if !served[route] {
			t.Errorf("apispec describes %q and the host does not serve it", route)
		}
	}
}

// One id names one operation, in the SDKs and in the MCP tool list.
func TestOperationIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, op := range Operations() {
		if seen[op.ID] {
			t.Errorf("the id %q names more than one operation", op.ID)
		}
		seen[op.ID] = true
		if op.Summary == "" {
			t.Errorf("%s has no summary, so an agent reads a tool with no description", op.ID)
		}
	}
}

// The document has to parse as JSON and name every tenant route.
func TestOpenAPICoversTheTenantAPI(t *testing.T) {
	body, err := OpenAPIJSON("https://api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, op := range Operations() {
		if op.Operator || op.Terminal {
			// OpenAPI 3.1 cannot describe a WebSocket, and an operator route
			// is not a tenant's to call. A generated client that carried
			// either would fail at run time.
			if strings.Contains(text, `"operationId": "`+op.ID+`"`) {
				t.Errorf("the document carries %s, which no generated client can call", op.ID)
			}
			continue
		}
		if !strings.Contains(text, `"operationId": "`+op.ID+`"`) {
			t.Errorf("the document forgets %s", op.ID)
		}
	}
}
