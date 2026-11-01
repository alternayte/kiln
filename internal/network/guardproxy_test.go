package network

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/alternayte/kiln/internal/hostca"
	"github.com/alternayte/kiln/internal/store"
)

func TestBindingMatch(t *testing.T) {
	b := Binding{Path: "/repos/acme/x/", Methods: []string{"GET"}}
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/repos/acme/x/issues", true},
		{"POST", "/repos/acme/x/issues", false},
		{"GET", "/repos/other", false},
		{"GET", "/repos/acme/x/../../other", false},
		{"GET", "/repos/acme/x/./issues", false},
		{"GET", "/repos/acme/x%2f..%2f..%2fother", false},
		{"GET", "/repos/acme/x/%2e%2e/other", false},
		{"GET", "/repos/acme/x/%252e%252e/other", false},
		{"GET", "/repos/acme/x//issues", false},
		{"GET", "/repos/acme/x/..%5c..%5cother", false},
	}
	for _, c := range cases {
		if got := b.matchRequest(c.method, c.path); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.method, c.path, got, c.want)
		}
	}
	exact := Binding{Path: "/repos/acme/x"}
	if exact.matchRequest("GET", "/repos/acme/xy") {
		t.Error("a prefix with no slash matched a longer segment")
	}
	if !exact.matchRequest("GET", "/repos/acme/x/y") || !exact.matchRequest("GET", "/repos/acme/x") {
		t.Error("a prefix with no slash missed its own segment")
	}
}

// proxyHarness is a guard in this process with one bound host and an upstream
// that echoes what reached it.
type proxyHarness struct {
	guard  *guardServer
	client *http.Client
}

func newProxyHarness(t *testing.T, bindings ...Binding) *proxyHarness {
	t.Helper()
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://other.example/stolen", http.StatusFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"host": r.Host, "headers": r.Header})
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := newGuardServer(cancel)
	g.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
	}
	g.dial = func(ctx context.Context, _ []netip.Addr) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	g.upstreamTLS = &tls.Config{RootCAs: roots}
	g.startProxy(ctx)

	st, err := store.Open(filepath.Join(t.TempDir(), "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key := make([]byte, store.KeySize)
	ca, err := hostca.TenantCA(ctx, st, key, store.DefaultTenant)
	if err != nil {
		t.Fatal(err)
	}
	cert, leafKey, err := hostca.IssueLeaf(key, ca, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	g.setPolicy(guardCommand{
		Allow: []string{"example.com"}, Bindings: bindings,
		Leaves: []Leaf{{Host: "example.com", Cert: cert, Key: leafKey}},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go g.handle(ctx, conn)
		}
	}()
	trust := x509.NewCertPool()
	trust.AppendCertsFromPEM([]byte(ca.CertPEM))
	return &proxyHarness{guard: g, client: &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: trust},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", ln.Addr().String())
			},
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// get sends one request with a path the client must not clean.
func (h *proxyHarness) get(t *testing.T, path string, header http.Header) (*http.Response, map[string]any) {
	t.Helper()
	req := &http.Request{
		Method: http.MethodGet, Host: "example.com", Header: header,
		URL: &url.URL{Scheme: "https", Host: "example.com", Opaque: path},
	}
	if header == nil {
		req.Header = http.Header{}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	return resp, parsed
}

func authOf(parsed map[string]any) string {
	headers, _ := parsed["headers"].(map[string]any)
	values, _ := headers["Authorization"].([]any)
	if len(values) == 0 {
		return ""
	}
	return values[0].(string)
}

func TestProxyInjectsOnlyOnAMatch(t *testing.T) {
	const key = "Bearer real-key"
	h := newProxyHarness(t, Binding{Name: "gh", Host: "example.com", Header: "Authorization", Value: key, Path: "/repos/acme/x/"})

	if _, got := h.get(t, "/repos/acme/x/issues?token=q", nil); authOf(got) != key {
		t.Fatalf("a match sent %q", authOf(got))
	}
	guest := http.Header{"Authorization": {"Bearer guest-choice"}}
	if _, got := h.get(t, "/repos/acme/x/issues", guest); authOf(got) != key {
		t.Fatalf("a guest header survived: %q", authOf(got))
	}
	for _, path := range []string{"/repos/other", "/repos/acme/x/../../other", "/repos/acme/x%2f..%2fother"} {
		resp, got := h.get(t, path, http.Header{"Authorization": {"Bearer guest-choice"}})
		if resp.StatusCode != http.StatusOK || authOf(got) != "" {
			t.Fatalf("%s: status %d, sent %q; want the request to pass with no key", path, resp.StatusCode, authOf(got))
		}
	}
}

func TestProxyFollowsNoRedirectAndSetsTheHost(t *testing.T) {
	h := newProxyHarness(t, Binding{Name: "k", Host: "example.com", Header: "X-Api-Key", Value: "real"})
	resp, _ := h.get(t, "/redirect", nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://other.example/stolen" {
		t.Fatalf("status %d, location %q; want the redirect to reach the guest", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, got := h.get(t, "/x", http.Header{"Host": {"attacker.example"}})
	if got["host"] != "example.com" {
		t.Fatalf("upstream host %v", got["host"])
	}
}

func TestProxyLogsNoQueryAndNoValue(t *testing.T) {
	h := newProxyHarness(t, Binding{Name: "k", Host: "example.com", Header: "X-Api-Key", Value: "real-value"})
	h.get(t, "/a?token=query", nil)
	h.get(t, "/b", nil)
	h.client.CloseIdleConnections()

	g := h.guard
	g.recordsMu.Lock()
	defer g.recordsMu.Unlock()
	if len(g.requests) != 2 || g.requests[0].Path != "/a" || g.requests[0].Status != 200 || g.requests[0].Binding != "k" {
		t.Fatalf("requests = %+v", g.requests)
	}
	injected := g.records[recordKey{"example.com", originalPort, DecisionInjected, "k"}]
	if injected == nil || injected.Count != 1 {
		t.Fatalf("injected row = %+v; want one for one connection", injected)
	}
	raw, _ := json.Marshal(g.requests)
	for _, leak := range []string{"token=query", "real-value"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Fatalf("the log holds %q: %s", leak, raw)
		}
	}
}
