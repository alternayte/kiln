// Package client is the Go SDK for the Kiln HTTP API. It depends only on the
// standard library, so a caller outside this module can import it.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls one Kiln daemon.
type Client struct {
	// BaseURL is the control listener, for example http://127.0.0.1:8080.
	BaseURL string
	// Token is the bearer token from config.json.
	Token string
	// HTTP is the underlying client. Nil uses http.DefaultClient.
	HTTP *http.Client
}

// New returns a client for one control listener.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), Token: token}
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Error is an API error with its stable code.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("kiln: HTTP %d: %s: %s", e.Status, e.Code, e.Message)
}

// NotFound reports whether err is a 404 from the API.
func NotFound(err error) bool {
	var e *Error
	return asError(err, &e) && e.Status == http.StatusNotFound
}

// Conflict reports whether err is a 409 from the API.
func Conflict(err error) bool {
	var e *Error
	return asError(err, &e) && e.Status == http.StatusConflict
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func apiError(status int, body []byte) error {
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Error.Code != "" {
		return &Error{Status: status, Code: decoded.Error.Code, Message: decoded.Error.Message}
	}
	return &Error{Status: status, Code: "internal", Message: strings.TrimSpace(string(body))}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("kiln: %s %s: %w", method, path, err)
	}
	return nil
}

// TemplateRequest is one POST /v1/templates body. EgressAllow is required and
// may be empty.
type TemplateRequest struct {
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	VCPUs       int      `json:"vcpus"`
	MemoryMB    int      `json:"memory_mb"`
	DiskMB      int      `json:"disk_mb"`
	Setup       []string `json:"setup"`
	EgressAllow []string `json:"egress_allow"`
}

// Template is one template row.
type Template struct {
	Name          string    `json:"name"`
	Image         string    `json:"image"`
	ImageDigest   string    `json:"image_digest,omitempty"`
	VCPUs         int       `json:"vcpus"`
	MemoryMB      int       `json:"memory_mb"`
	DiskMB        int       `json:"disk_mb"`
	EgressAllow   []string  `json:"egress_allow"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	SnapshotBytes int64     `json:"snapshot_bytes"`
	Sandboxes     int       `json:"sandboxes"`
	CreatedAt     time.Time `json:"created_at"`
}

// CreateTemplate starts a build and returns before it runs.
func (c *Client) CreateTemplate(ctx context.Context, req TemplateRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/templates", req, nil)
}

// Templates lists every template.
func (c *Client) Templates(ctx context.Context) ([]Template, error) {
	var out []Template
	if err := c.do(ctx, http.MethodGet, "/v1/templates", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Template returns one template.
func (c *Client) Template(ctx context.Context, name string) (Template, error) {
	var out Template
	if err := c.do(ctx, http.MethodGet, "/v1/templates/"+url.PathEscape(name), nil, &out); err != nil {
		return Template{}, err
	}
	return out, nil
}

// DeleteTemplate removes one template.
func (c *Client) DeleteTemplate(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/templates/"+url.PathEscape(name), nil, nil)
}

// SandboxRequest is one POST /v1/sandboxes body. Lifecycle and IdleSeconds
// are required. TTLSeconds nil means no deadline.
type SandboxRequest struct {
	Template    string         `json:"template"`
	Lifecycle   string         `json:"lifecycle"`
	IdleSeconds int            `json:"idle_seconds"`
	TTLSeconds  *int           `json:"ttl_seconds,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Secrets     []string       `json:"secrets,omitempty"`
}

// Sandbox is one sandbox row.
type Sandbox struct {
	ID           string          `json:"id"`
	State        string          `json:"state"`
	Template     string          `json:"template"`
	Lifecycle    string          `json:"lifecycle"`
	IdleSeconds  int             `json:"idle_seconds"`
	TTLSeconds   *int            `json:"ttl_seconds"`
	Metadata     json.RawMessage `json:"metadata"`
	CreatedAt    time.Time       `json:"created_at"`
	LastActiveAt time.Time       `json:"last_active_at"`
	DestroyedAt  *time.Time      `json:"destroyed_at,omitempty"`
}

// CreateSandbox restores a template snapshot into a running sandbox.
func (c *Client) CreateSandbox(ctx context.Context, req SandboxRequest) (Sandbox, error) {
	var out Sandbox
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes", req, &out); err != nil {
		return Sandbox{}, err
	}
	return out, nil
}

// Sandboxes lists every sandbox.
func (c *Client) Sandboxes(ctx context.Context) ([]Sandbox, error) {
	var out []Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Sandbox returns one sandbox. A row read never wakes a sleeping sandbox.
func (c *Client) Sandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, &out); err != nil {
		return Sandbox{}, err
	}
	return out, nil
}

// DeleteSandbox destroys one sandbox and every host resource named after it.
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil)
}

// ExecRequest is one command to run inside a sandbox.
type ExecRequest struct {
	Cmd            []string          `json:"cmd"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// ExecResult is the buffered result of one command.
type ExecResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Exec runs one command and buffers its output. A sleeping sandbox wakes
// first.
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error) {
	var out ExecResult
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", req, &out); err != nil {
		return ExecResult{}, err
	}
	return out, nil
}

// ReadFile opens one guest file. The caller closes the reader. A sleeping
// sandbox wakes first.
func (c *Client) ReadFile(ctx context.Context, id, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/sandboxes/"+url.PathEscape(id)+"/files/"+escapePath(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, apiError(resp.StatusCode, data)
	}
	return resp.Body, nil
}

// WriteFile writes one guest file. Parent directories are created.
func (c *Client) WriteFile(ctx context.Context, id, path string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+"/v1/sandboxes/"+url.PathEscape(id)+"/files/"+escapePath(path), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return apiError(resp.StatusCode, data)
}

// SnapshotCreated is the answer to a snapshot call.
type SnapshotCreated struct {
	SnapshotID string `json:"snapshot_id"`
	SizeBytes  int64  `json:"size_bytes"`
}

// Snapshot writes a listed snapshot of one sandbox. With stop set the sandbox
// sleeps afterwards.
func (c *Client) Snapshot(ctx context.Context, id string, stop bool) (SnapshotCreated, error) {
	var out SnapshotCreated
	body := map[string]any{"stop": stop}
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/snapshot", body, &out); err != nil {
		return SnapshotCreated{}, err
	}
	return out, nil
}

// SandboxRef names one copy.
type SandboxRef struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// Fork snapshots a running sandbox and restores count copies. A secret-bearing
// sandbox needs allowSecretFork.
func (c *Client) Fork(ctx context.Context, id string, count int, allowSecretFork bool) ([]SandboxRef, error) {
	var out struct {
		Sandboxes []SandboxRef `json:"sandboxes"`
	}
	body := map[string]any{"count": count, "allow_secret_fork": allowSecretFork}
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/fork", body, &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
}

// Snapshot is one listed snapshot row.
type Snapshot struct {
	ID            string    `json:"id"`
	Template      string    `json:"template"`
	ParentID      string    `json:"parent_id,omitempty"`
	SizeBytes     int64     `json:"size_bytes"`
	SecretBearing bool      `json:"secret_bearing"`
	CreatedAt     time.Time `json:"created_at"`
}

// Snapshots lists every listed snapshot.
func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	var out []Snapshot
	if err := c.do(ctx, http.MethodGet, "/v1/snapshots", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SnapshotInfo returns one listed snapshot.
func (c *Client) SnapshotInfo(ctx context.Context, id string) (Snapshot, error) {
	var out Snapshot
	if err := c.do(ctx, http.MethodGet, "/v1/snapshots/"+url.PathEscape(id), nil, &out); err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

// DeleteSnapshot removes one listing once no live child reads it.
func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/snapshots/"+url.PathEscape(id), nil, nil)
}

// RestoreSnapshot copies a listed snapshot into count new sandboxes.
func (c *Client) RestoreSnapshot(ctx context.Context, snapshotID string, count int, allowSecretFork bool) ([]SandboxRef, error) {
	var out struct {
		Sandboxes []SandboxRef `json:"sandboxes"`
	}
	body := map[string]any{"count": count, "allow_secret_fork": allowSecretFork}
	if err := c.do(ctx, http.MethodPost, "/v1/snapshots/"+url.PathEscape(snapshotID)+"/restore", body, &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
}

// Health is the daemon's view of the host.
type Health struct {
	OK          bool   `json:"ok"`
	KVM         bool   `json:"kvm"`
	Firecracker string `json:"firecracker"`
	Sandboxes   int    `json:"sandboxes"`
}

// Health returns the daemon's health.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	if err := c.do(ctx, http.MethodGet, "/v1/health", nil, &out); err != nil {
		return Health{}, err
	}
	return out, nil
}

// Event is one state transition.
type Event struct {
	ID        int64     `json:"id"`
	SandboxID string    `json:"sandbox_id,omitempty"`
	FromState string    `json:"from_state,omitempty"`
	ToState   string    `json:"to_state,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

// Events calls fn for every state transition until ctx ends. A zero after
// starts at the latest event; pass the last seen id to resume without a gap.
func (c *Client) Events(ctx context.Context, after int64, sandboxID string, fn func(Event) error) error {
	query := url.Values{}
	if after > 0 {
		query.Set("after", strconv.FormatInt(after, 10))
	}
	if sandboxID != "" {
		query.Set("sandbox_id", sandboxID)
	}
	target := c.BaseURL + "/v1/events"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return apiError(resp.StatusCode, data)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		if err := fn(event); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// escapePath escapes a guest path for the URL, keeping its slashes.
func escapePath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
