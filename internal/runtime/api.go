package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiClientTimeout bounds one Firecracker API call. A snapshot load can serve
// page faults for a while before it answers.
const apiClientTimeout = 2 * time.Minute

// api is a minimal Firecracker API client over the jailed API socket.
type api struct {
	client *http.Client
}

func newAPI(socket string) *api {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &api{client: &http.Client{Transport: tr, Timeout: apiClientTimeout}}
}

func (a *api) put(ctx context.Context, path string, body any) error {
	_, err := a.do(ctx, http.MethodPut, path, body)
	return err
}

func (a *api) patch(ctx context.Context, path string, body any) error {
	_, err := a.do(ctx, http.MethodPatch, path, body)
	return err
}

// get calls one Firecracker endpoint and returns its JSON body.
func (a *api) get(ctx context.Context, path string) ([]byte, error) {
	return a.do(ctx, http.MethodGet, path, nil)
}

func (a *api) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("firecracker api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return respBody, nil
	}
	var fault struct {
		FaultMessage string `json:"fault_message"`
	}
	if err := json.Unmarshal(respBody, &fault); err == nil && fault.FaultMessage != "" {
		return nil, fmt.Errorf("firecracker api %s %s: %s", method, path, fault.FaultMessage)
	}
	return nil, fmt.Errorf("firecracker api %s %s: HTTP %s", method, path, resp.Status)
}
