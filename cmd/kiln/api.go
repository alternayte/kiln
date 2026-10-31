package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/alternayte/kiln/internal/apispec"
)

const defaultRoot = "/var/lib/kiln"

// connection finds where to call and with which credential. An operator on the
// host needs no setup, because config.json holds both.
func connection() (base, token string, err error) {
	base, token = os.Getenv("KILN_URL"), os.Getenv("KILN_API_KEY")
	if base != "" {
		if token == "" {
			return "", "", usagef("export KILN_API_KEY=<api key>", "KILN_URL is set and KILN_API_KEY is not")
		}
		return strings.TrimRight(base, "/"), token, nil
	}
	root := os.Getenv("KILN_ROOT")
	if root == "" {
		root = defaultRoot
	}
	b, readErr := os.ReadFile(filepath.Join(root, "config.json"))
	if readErr != nil {
		return "", "", usagef("export KILN_URL=https://<gateway> KILN_API_KEY=<api key>", "KILN_URL is not set, and %v", readErr)
	}
	var cfg struct {
		BearerToken string `json:"bearer_token"`
		ControlAddr string `json:"control_addr"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", "", usagef("", "%s: %v", filepath.Join(root, "config.json"), err)
	}
	base = cfg.ControlAddr
	if base == "" {
		base = "127.0.0.1:8080"
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return base, cfg.BearerToken, nil
}

// request sends one call and returns the open response. A status of 400 or
// above comes back as a failure.
func (c *cli) request(ctx context.Context, method, path, accept string, body io.Reader) (*http.Response, error) {
	base, token, err := connection()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, usagef("", "%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "kiln/"+apispec.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &failure{exit: apispec.ExitFailure, code: "network", message: err.Error(), next: "check KILN_URL, then run kiln health"}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, apiFailure(resp.StatusCode, b, path)
	}
	return resp, nil
}

// call sends one call with a JSON body, and decodes the answer into out when
// out is set. It returns the raw answer.
func (c *cli) call(ctx context.Context, method, path string, body, out any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, usagef("", "%v", err)
		}
		reader = bytes.NewReader(b)
	}
	resp, err := c.request(ctx, method, path, "", reader)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &failure{exit: apispec.ExitFailure, code: "network", message: err.Error()}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, &failure{exit: apispec.ExitFailure, code: "failed", message: "the answer is not JSON: " + err.Error()}
		}
	}
	return raw, nil
}

// apiFailure turns an error answer into a failure with its exit code and the
// command that recovers.
func apiFailure(status int, body []byte, path string) *failure {
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	f := &failure{exit: apispec.ExitFailure, code: "failed", message: fmt.Sprintf("HTTP %d: %s", status, strings.TrimSpace(string(body)))}
	if json.Unmarshal(body, &decoded) == nil && decoded.Error.Code != "" {
		f.code, f.message = decoded.Error.Code, decoded.Error.Message
	}
	list := listCommand(path)
	switch f.code {
	case "not_found":
		f.exit, f.next = apispec.ExitNotFound, list
	case "conflict":
		f.exit, f.next = apispec.ExitConflict, list
	case "exhausted":
		f.exit, f.next = apispec.ExitExhausted, "kiln sandboxes list, then kiln sandboxes delete <id>"
	case "invalid":
		f.exit, f.next = apispec.ExitInvalid, "kiln help"
	}
	if status == http.StatusUnauthorized {
		f.next = "check KILN_API_KEY"
	}
	return f
}

// listCommand names the command that shows the rows a path reaches.
func listCommand(path string) string {
	for _, noun := range []string{"sandboxes", "templates", "snapshots", "registries", "viewers", "tenants"} {
		if strings.HasPrefix(path, "/v1/"+noun) {
			return "kiln " + noun + " list"
		}
	}
	return "kiln help"
}
