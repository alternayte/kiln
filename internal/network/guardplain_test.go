package network

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// plainHarness is a guard in this process whose port 80 listener reaches one
// upstream that echoes the Host header it saw.
func plainHarness(t *testing.T) (*guardServer, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "host="+r.Host+" path="+r.URL.Path)
	}))
	t.Cleanup(upstream.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := newGuardServer(cancel)
	g.resolve = func(_ context.Context, name string) ([]netip.Addr, error) {
		if name == "metadata.test" {
			return nil, errNonPublic
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
	}
	g.dial = func(ctx context.Context, _ []netip.Addr, port int) (net.Conn, error) {
		if port != plainPort {
			t.Errorf("dialled port %d", port)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}
	g.startPlain(ctx)
	g.setPolicy(guardCommand{Allow: []string{"allowed.test", "metadata.test"}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = accept(ctx, ln, g.handlePlain) }()
	return g, ln.Addr().String()
}

func plainRequest(t *testing.T, conn net.Conn, r *bufio.Reader, host string) (int, string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "GET /dists/x HTTP/1.1\r\nHost: "+host+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

func plainRow(g *guardServer, host, decision string) *Record {
	g.recordsMu.Lock()
	defer g.recordsMu.Unlock()
	if rec := g.records[recordKey{host, plainPort, decision, ""}]; rec != nil {
		copied := *rec
		return &copied
	}
	return nil
}

// TestPlainDecidesEachRequestByHost pins that port 80 passes an allowed Host
// unchanged and refuses another Host on the same connection, which an allowed
// CDN address would otherwise route to any site it serves.
func TestPlainDecidesEachRequestByHost(t *testing.T) {
	g, addr := plainHarness(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	if status, body := plainRequest(t, conn, r, "Allowed.test:80"); status != http.StatusOK || body != "host=Allowed.test:80 path=/dists/x" {
		t.Fatalf("allowed host: %d %q", status, body)
	}
	if status, body := plainRequest(t, conn, r, "other.test"); status != http.StatusForbidden || !strings.Contains(body, "other.test is not on the egress allow list") {
		t.Fatalf("other host on the same connection: %d %q", status, body)
	}
	if status, _ := plainRequest(t, conn, r, "metadata.test"); status != http.StatusForbidden {
		t.Fatalf("a name that resolves to a private address: %d", status)
	}
	if status, _ := plainRequest(t, conn, r, "allowed.test"); status != http.StatusOK {
		t.Fatalf("allowed host after a refusal: %d", status)
	}

	allowed := plainRow(g, "allowed.test", DecisionAllowed)
	if allowed == nil || allowed.Count != 2 || allowed.BytesIn == 0 || allowed.BytesOut == 0 {
		t.Errorf("allowed row %+v", allowed)
	}
	for _, host := range []string{"other.test", "metadata.test"} {
		if row := plainRow(g, host, DecisionDenied); row == nil || row.Count != 1 {
			t.Errorf("denied row of %s: %+v", host, row)
		}
	}
}

// TestPlainRefusesAConnectionWithNoRequest pins that a port 80 connection with
// no request is recorded by the address the guest chose.
func TestPlainRefusesAConnectionWithNoRequest(t *testing.T) {
	g, addr := plainHarness(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "\x16\x03\x01 not http\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.ReadAll(conn)
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for plainRow(g, "127.0.0.1", DecisionDenied) == nil {
		if time.Now().After(deadline) {
			t.Fatal("no denied row for the address")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
