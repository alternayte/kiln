package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/alternayte/kiln/internal/apispec"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2026-07-28"

// rpcRequest is one JSON-RPC message. A notification carries no id.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcp serves the tools an agent calls. Every tool is one API operation, and
// the call travels the same path as a REST call: the caller's credential
// names the tenant, and the host enforces it.
func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r.Context())
	if err != nil {
		writeError(w, http.StatusForbidden, "invalid", err.Error())
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()},
		})
		return
	}
	switch req.Method {
	case "initialize":
		s.rpcResult(w, req, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kiln", "version": apispec.Version},
		})
	case "notifications/initialized":
		// A notification carries no id and takes no answer.
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		s.rpcResult(w, req, map[string]any{"tools": tools()})
	case "tools/call":
		s.callTool(r, w, req, tenant)
	case "ping":
		s.rpcResult(w, req, map[string]any{})
	default:
		s.rpcError(w, req, -32601, fmt.Sprintf("the method %q does not exist", req.Method))
	}
}

// tools is the tool list, built from the one API description.
func tools() []any {
	var out []any
	for _, op := range apispec.Operations() {
		if op.Operator {
			continue
		}
		props := map[string]any{}
		var required []string
		for _, p := range op.Params {
			props[p.Name] = map[string]any{"type": jsonType(p.Type), "description": p.Doc}
			required = append(required, p.Name)
		}
		for _, f := range op.Body {
			props[f.Name] = fieldJSON(f)
			if f.Required {
				required = append(required, f.Name)
			}
		}
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		out = append(out, map[string]any{
			"name":        op.ID,
			"description": op.Summary,
			"inputSchema": schema,
		})
	}
	return out
}

func fieldJSON(f apispec.Field) map[string]any {
	switch f.Type {
	case "array":
		return map[string]any{"type": "array", "items": map[string]any{"type": jsonType(f.Items)}, "description": f.Doc}
	case "object":
		return map[string]any{"type": "object", "additionalProperties": true, "description": f.Doc}
	default:
		return map[string]any{"type": jsonType(f.Type), "description": f.Doc}
	}
}

func jsonType(t string) string {
	if t == "" {
		return "string"
	}
	return t
}

// callTool turns one tool call into one host call.
func (s *Server) callTool(r *http.Request, w http.ResponseWriter, req rpcRequest, tenant string) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.rpcError(w, req, -32602, err.Error())
		return
	}
	var op apispec.Operation
	for _, candidate := range apispec.Operations() {
		if candidate.ID == params.Name && !candidate.Operator {
			op = candidate
			break
		}
	}
	if op.ID == "" {
		s.rpcError(w, req, -32602, fmt.Sprintf("the tool %q does not exist", params.Name))
		return
	}
	path := op.Path
	for _, p := range op.Params {
		value, ok := params.Arguments[p.Name]
		if !ok {
			s.rpcError(w, req, -32602, fmt.Sprintf("%s is required", p.Name))
			return
		}
		path = strings.ReplaceAll(path, "{"+p.Name+"...}", fmt.Sprint(value))
		path = strings.ReplaceAll(path, "{"+p.Name+"}", fmt.Sprint(value))
		delete(params.Arguments, p.Name)
	}
	var body any
	if len(op.Body) > 0 && len(params.Arguments) > 0 {
		body = params.Arguments
	}
	out, err := s.hostCall(r.Context(), op.Method, path, tenant, body)
	if err != nil {
		// The agent needs the host's own message, which names the next call.
		s.rpcResult(w, req, map[string]any{
			"isError": true,
			"content": []any{map[string]any{"type": "text", "text": err.Error()}},
		})
		return
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		text = fmt.Sprintf("%s answered %d with no body.", op.ID, op.Status)
	}
	result := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
	var structured any
	if json.Unmarshal(bytes.TrimSpace(out), &structured) == nil && structured != nil {
		result["structuredContent"] = structured
	}
	s.rpcResult(w, req, result)
}

func (s *Server) rpcResult(w http.ResponseWriter, req rpcRequest, result any) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) rpcError(w http.ResponseWriter, req rpcRequest, code int, message string) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: message}})
}
