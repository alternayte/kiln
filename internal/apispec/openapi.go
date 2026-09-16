package apispec

import (
	"encoding/json"
	"strings"
)

// Version is the semver of this contract. It rises with the binary.
const Version = "0.2.0"

// OpenAPI returns the OpenAPI 3.1 document of the tenant API. The operator
// routes stay out: a tenant never reaches them, and the SDKs never call them.
func OpenAPI(serverURL string) map[string]any {
	paths := map[string]any{}
	for _, op := range Operations() {
		if op.Operator {
			continue
		}
		path := openAPIPath(op.Path)
		item, ok := paths[path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[path] = item
		}
		item[strings.ToLower(op.Method)] = operationObject(op)
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Kiln",
			"version":     Version,
			"description": "Isolated Linux microVMs on one host, created from OCI images and forked into copies.",
		},
		"servers": []any{map[string]any{"url": serverURL}},
		"security": []any{
			map[string]any{"apiKey": []any{}},
			map[string]any{"oauth2": []any{}},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"apiKey": map[string]any{
					"type": "http", "scheme": "bearer",
					"description": "A tenant API key, or an OAuth access token.",
				},
				"oauth2": map[string]any{
					"type":        "http",
					"scheme":      "bearer",
					"description": "An OAuth 2.1 access token of this tenant.",
				},
			},
			"schemas": schemas(),
		},
		"paths": paths,
	}
}

// OpenAPIJSON is the document as indented JSON.
func OpenAPIJSON(serverURL string) ([]byte, error) {
	return json.MarshalIndent(OpenAPI(serverURL), "", "  ")
}

func operationObject(op Operation) map[string]any {
	out := map[string]any{
		"operationId": op.ID,
		"summary":     op.Summary,
		"responses":   responses(op),
	}
	if len(op.Params) > 0 {
		var params []any
		for _, p := range op.Params {
			params = append(params, map[string]any{
				"name":        p.Name,
				"in":          "path",
				"required":    true,
				"description": p.Doc,
				"schema":      fieldSchema(p),
			})
		}
		out["parameters"] = params
	}
	if len(op.Body) > 0 {
		props := map[string]any{}
		var required []any
		for _, f := range op.Body {
			props[f.Name] = fieldSchema(f)
			if f.Required {
				required = append(required, f.Name)
			}
		}
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		out["requestBody"] = map[string]any{
			"required": true,
			"content":  map[string]any{"application/json": map[string]any{"schema": schema}},
		}
	}
	return out
}

func responses(op Operation) map[string]any {
	success := map[string]any{"description": "The call succeeded."}
	if op.Returns != "" {
		success["content"] = map[string]any{
			"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/" + op.Returns}},
		}
	}
	out := map[string]any{statusKey(op.Status): success}
	for _, code := range []string{"400", "401", "404", "409", "507"} {
		out[code] = map[string]any{
			"description": errorDoc[code],
			"content": map[string]any{
				"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}},
			},
		}
	}
	return out
}

var errorDoc = map[string]string{
	"400": "The request is invalid. The message names the field.",
	"401": "The credential is missing, expired or revoked.",
	"404": "The row does not exist, or it belongs to another tenant.",
	"409": "The call conflicts with the state, for example a template that still has sandboxes.",
	"507": "A cap of this tenant is reached. The message names the cap.",
}

func statusKey(status int) string {
	if status == 0 {
		return "200"
	}
	return string(rune('0'+status/100)) + string(rune('0'+status/10%10)) + string(rune('0'+status%10))
}

func fieldSchema(f Field) map[string]any {
	switch f.Type {
	case "array":
		return map[string]any{"type": "array", "items": map[string]any{"type": f.Items}}
	case "object":
		return map[string]any{"type": "object", "additionalProperties": true}
	default:
		return map[string]any{"type": f.Type}
	}
}

// openAPIPath turns the {path...} wildcard the host uses into {path}.
func openAPIPath(p string) string { return strings.ReplaceAll(p, "...}", "}") }

func schemas() map[string]any {
	str := map[string]any{"type": "string"}
	num := map[string]any{"type": "integer"}
	object := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	list := func(ref string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/" + ref}}
	}
	sandbox := object(map[string]any{
		"id": str, "template": str, "state": str, "lifecycle": str,
		"created_at": str, "published": list("Published"),
	})
	return map[string]any{
		"Error": object(map[string]any{
			"error": object(map[string]any{"code": str, "message": str}),
		}),
		"Health":        object(map[string]any{"ok": map[string]any{"type": "boolean"}, "kvm": map[string]any{"type": "boolean"}, "firecracker": str, "sandboxes": num}),
		"Template":      object(map[string]any{"name": str, "image": str, "state": str, "error": str, "created_at": str}),
		"TemplateState": object(map[string]any{"name": str, "state": str}),
		"TemplateList":  list("Template"),
		"Sandbox":       sandbox,
		"SandboxList":   list("Sandbox"),
		"ExecResult": object(map[string]any{
			"exit_code": num, "stdout": str, "stderr": str,
			"truncated": map[string]any{"type": "boolean"}, "timed_out": map[string]any{"type": "boolean"},
		}),
		"SnapshotCreated": object(map[string]any{"snapshot_id": str}),
		"Snapshot":        object(map[string]any{"id": str, "template": str, "size_bytes": num, "created_at": str}),
		"SnapshotList":    list("Snapshot"),
		"Published":       object(map[string]any{"port": num, "url": str, "visibility": str}),
		"Event":           object(map[string]any{"id": num, "sandbox_id": str, "from": str, "to": str, "reason": str, "at": str}),
		"EventList":       list("Event"),
		"FileBody":        map[string]any{"type": "string", "format": "binary"},
		"Tenant": object(map[string]any{
			"id": str, "name": str, "max_sandboxes": num, "max_templates": num,
			"max_snapshot_bytes": num, "sandboxes": num, "templates": num, "snapshot_bytes": num,
		}),
		"Viewer":     object(map[string]any{"email": str, "tenant": str}),
		"TenantList": list("Tenant"),
	}
}
