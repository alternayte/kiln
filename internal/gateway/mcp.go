package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/kiln/internal/apispec"
)

// eventsWindow bounds the events tool. The host writes the backlog on its
// first tick, one second in, and the stream never ends, so the tool reads
// for this long and answers with what arrived.
const eventsWindow = 3 * time.Second

// The raw operations the MCP server handles by hand: their bodies are not
// JSON objects, so the generic path cannot carry them.
const (
	toolWriteFile = "writeFile"
	toolEvents    = "events"
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
	tenant, err := s.tenantOfCaller(r)
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

// tools is the tool list, built from the one API description. A terminal
// is a WebSocket, which no tool call can carry, so it stays out.
func tools() []any {
	var out []any
	for _, op := range apispec.Operations() {
		if op.Operator || op.Terminal {
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
		switch op.ID {
		case toolWriteFile:
			props["content"] = map[string]any{"type": "string", "description": "The file's content, as text."}
			required = append(required, "content")
		case toolEvents:
			props["after"] = map[string]any{"type": "integer", "description": "Return the events after this id. 0 replays every event of this tenant. Omitted, the newest events only."}
			props["sandbox_id"] = map[string]any{"type": "string", "description": "Keep the events of one sandbox."}
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
		if candidate.ID == params.Name && !candidate.Operator && !candidate.Terminal {
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
	var out []byte
	var err error
	switch op.ID {
	case toolWriteFile:
		content, _ := params.Arguments["content"].(string)
		out, err = s.hostRaw(r.Context(), op.Method, path, tenant, "application/octet-stream", strings.NewReader(content))
	case toolEvents:
		out, err = s.hostEvents(r.Context(), path, tenant, params.Arguments)
	default:
		var body any
		if len(op.Body) > 0 && len(params.Arguments) > 0 {
			body = params.Arguments
		}
		out, err = s.hostCall(r.Context(), op.Method, path, tenant, body)
	}
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

// hostRaw sends one call whose body is not JSON, and reads the whole answer.
func (s *Server) hostRaw(ctx context.Context, method, path, tenant, contentType string, body io.Reader) ([]byte, error) {
	resp, err := s.hostOpen(ctx, method, path, tenant, contentType, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("host %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(out))
	}
	return out, nil
}

// hostEvents reads the event stream for one window and answers with the
// events that arrived, as a JSON array. The stream never ends, so a tool
// call that waited for its end would never answer.
func (s *Server) hostEvents(ctx context.Context, path, tenant string, args map[string]any) ([]byte, error) {
	query := url.Values{}
	if after, ok := args["after"]; ok {
		n, err := integerArg(after)
		if err != nil {
			return nil, fmt.Errorf("after: %w", err)
		}
		query.Set("after", strconv.FormatInt(n, 10))
	}
	if id, ok := args["sandbox_id"].(string); ok && id != "" {
		query.Set("sandbox_id", id)
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, eventsWindow)
	defer cancel()
	resp, err := s.hostOpen(ctx, http.MethodGet, path, tenant, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		out, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("host GET %s: %s: %s", path, resp.Status, bytes.TrimSpace(out))
	}
	events := []json.RawMessage{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if json.Valid([]byte(data)) {
			events = append(events, json.RawMessage(data))
		}
	}
	// The window closing is the normal end of the read.
	if err := scanner.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	return json.Marshal(events)
}

// hostOpen sends one call to the host and returns the open response.
func (s *Server) hostOpen(ctx context.Context, method, path, tenant, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.Host.URL.String()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Host.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if tenant != "" {
		req.Header.Set(TenantHeader, tenant)
	}
	return (&http.Client{Transport: s.Host.Transport}).Do(req)
}

// integerArg reads a JSON number an MCP client sent as an integer.
func integerArg(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, fmt.Errorf("%v is not an integer", v)
		}
		return int64(n), nil
	case string:
		return strconv.ParseInt(n, 10, 64)
	}
	return 0, fmt.Errorf("%v is not an integer", v)
}

func (s *Server) rpcResult(w http.ResponseWriter, req rpcRequest, result any) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) rpcError(w http.ResponseWriter, req rpcRequest, code int, message string) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: message}})
}
