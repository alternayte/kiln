package apispec

import (
	"fmt"
	"strings"
)

// LLMsTXT is the short index an agent reads first. It names what Kiln is,
// where the machine-readable files are, and the shortest path to a running
// sandbox.
func LLMsTXT(baseURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Kiln\n\n")
	fmt.Fprintf(&b, "> Isolated Linux microVMs on one host. A sandbox boots from a template snapshot in under a second, forks into copies, sleeps, and serves a port on a public hostname.\n\n")
	fmt.Fprintf(&b, "Authenticate every call with `Authorization: Bearer <api key>`. A key names one tenant, and a call only ever reaches that tenant's rows.\n\n")
	fmt.Fprintf(&b, "## Machine readable\n\n")
	fmt.Fprintf(&b, "- [OpenAPI](%s/openapi.json): every route, body and error code.\n", baseURL)
	fmt.Fprintf(&b, "- [MCP](%s/mcp): the same operations as tools, over Streamable HTTP.\n", baseURL)
	fmt.Fprintf(&b, "- [Catalog](%s/.well-known/ai-catalog.json): this index, in one file.\n", baseURL)
	fmt.Fprintf(&b, "- [Full documentation](%s/llms-full.txt): every operation with its fields.\n\n", baseURL)
	fmt.Fprintf(&b, "## The shortest path to a running sandbox\n\n")
	fmt.Fprintf(&b, "1. `POST /v1/templates` builds a template from an OCI image. It answers 202 and builds after the answer.\n")
	fmt.Fprintf(&b, "2. `GET /v1/templates/{name}` until `state` is `ready`.\n")
	fmt.Fprintf(&b, "3. `POST /v1/sandboxes` restores that template into a sandbox.\n")
	fmt.Fprintf(&b, "4. `POST /v1/sandboxes/{id}/exec` runs a command. Send `Accept: text/event-stream` to read output as it runs.\n")
	fmt.Fprintf(&b, "5. `POST /v1/sandboxes/{id}/publish` serves a guest port on a public hostname.\n")
	fmt.Fprintf(&b, "6. `DELETE /v1/sandboxes/{id}` destroys it. Nothing is left behind.\n\n")
	fmt.Fprintf(&b, "## Errors\n\n")
	fmt.Fprintf(&b, "Every error is `{\"error\":{\"code\":\"...\",\"message\":\"...\"}}`. The code is stable: `invalid`, `not_found`, `conflict`, `exhausted`, `internal`. `exhausted` means a cap of this tenant is reached; the message names the cap. `not_found` also covers a row of another tenant.\n")
	return b.String()
}

// LLMsFullTXT is every operation with its fields, for an agent that reads
// documentation instead of a schema.
func LLMsFullTXT(baseURL string) string {
	var b strings.Builder
	b.WriteString(LLMsTXT(baseURL))
	b.WriteString("\n## Operations\n")
	for _, op := range Operations() {
		if op.Operator {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n`%s %s`\n\n%s\n", op.ID, op.Method, op.Path, op.Summary)
		if op.Terminal {
			b.WriteString("\nThis call is a WebSocket upgrade, not a request and an answer. The OpenAPI document and the generated clients leave it out.\n")
		}
		if op.Streams {
			b.WriteString("\nThis call can stream. Read the body as it arrives.\n")
		}
		if len(op.Params) > 0 {
			b.WriteString("\nPath:\n\n")
			for _, p := range op.Params {
				fmt.Fprintf(&b, "- `%s` (%s): %s\n", p.Name, p.Type, p.Doc)
			}
		}
		if len(op.Body) > 0 {
			b.WriteString("\nBody:\n\n")
			for _, f := range op.Body {
				required := ""
				if f.Required {
					required = ", required"
				}
				fmt.Fprintf(&b, "- `%s` (%s%s): %s\n", f.Name, fieldType(f), required, f.Doc)
			}
		}
		if op.Returns != "" {
			fmt.Fprintf(&b, "\nAnswers %d with `%s`.\n", op.Status, op.Returns)
		} else {
			fmt.Fprintf(&b, "\nAnswers %d with no body.\n", op.Status)
		}
	}
	return b.String()
}

// Catalog is /.well-known/ai-catalog.json: one file that names the MCP
// endpoint, the OpenAPI document and the documentation.
func Catalog(baseURL string) map[string]any {
	return map[string]any{
		"name":        "Kiln",
		"description": "Isolated Linux microVMs: create, exec, fork, sleep and publish sandboxes.",
		"version":     Version,
		"documentation": []any{
			map[string]any{"type": "llms.txt", "url": baseURL + "/llms.txt"},
			map[string]any{"type": "llms-full.txt", "url": baseURL + "/llms-full.txt"},
		},
		"interfaces": []any{
			map[string]any{
				"type":      "mcp",
				"url":       baseURL + "/mcp",
				"transport": "streamable-http",
				"auth":      []any{"bearer", "oauth2"},
			},
			map[string]any{
				"type": "openapi",
				"url":  baseURL + "/openapi.json",
				"auth": []any{"bearer", "oauth2"},
			},
		},
	}
}

func fieldType(f Field) string {
	if f.Type == "array" {
		return "array of " + f.Items
	}
	return f.Type
}
