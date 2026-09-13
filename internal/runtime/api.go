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
	return &api{client: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

func (a *api) put(ctx context.Context, path string, body any) error {
	return a.do(ctx, http.MethodPut, path, body)
}

func (a *api) patch(ctx context.Context, path string, body any) error {
	return a.do(ctx, http.MethodPatch, path, body)
}

func (a *api) do(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var fault struct {
		FaultMessage string `json:"fault_message"`
	}
	if err := json.Unmarshal(respBody, &fault); err == nil && fault.FaultMessage != "" {
		return fmt.Errorf("firecracker api %s %s: %s", method, path, fault.FaultMessage)
	}
	return fmt.Errorf("firecracker api %s %s: HTTP %s", method, path, resp.Status)
}
