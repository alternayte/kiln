//go:build kvm

package secgate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/alternayte/kiln/internal/api"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

const (
	streamTemplate = "sec-stream"
	streamPort     = 7421
	streamIdle     = 20
)

// streamServer serves HTTPS with the certificate the gate made, and keeps a
// connection open between requests.
const streamServer = `import http.server,ssl
class H(http.server.BaseHTTPRequestHandler):
    protocol_version='HTTP/1.1'
    def do_GET(self):
        self.send_response(200);self.send_header('Content-Length','9');self.end_headers();self.wfile.write(b'stream-ok')
    def log_message(self,*a): pass
s=http.server.ThreadingHTTPServer(('0.0.0.0',7421),H)
c=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);c.load_cert_chain('/opt/stream/cert.pem','/opt/stream/key.pem')
s.socket=c.wrap_socket(s.socket,server_side=True)
s.serve_forever()
`

// TestSecGuestPortStream proves that a client off the host reaches a guest
// port with the guest's own certificate, that traffic keeps the sandbox
// awake, and that sleep and destroy end the stream.
func TestSecGuestPortStream(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("gate sec needs root: start it with sudo")
	}
	repo := repoRoot(t)
	root := kilnRoot()
	kernel := kernelPath(t, repo, root)
	kilnBin := buildKiln(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()
	st, err := store.Open(filepath.Join(root, "kiln.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rt, err := runtime.New(runtime.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	nm := network.New(kilnBin, root)
	if err := nm.EnsureBase(ctx); err != nil {
		t.Fatal(err)
	}
	builder := template.Builder{KilninitPath: filepath.Join(root, "bin", "kilninit")}
	tmgr := &template.Manager{Root: root, Store: st, Runtime: rt, Network: nm, Builder: builder, KernelPath: kernel}
	sbx := sandbox.New(sandbox.Config{
		Root: root, Store: st, Runtime: rt, Network: nm, Zone: zone,
		Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: kilnBin} },
		KernelPath: kernel,
	})
	destroyAll := func() {
		clean := context.WithoutCancel(ctx)
		rows, _ := st.ListSandboxes(clean)
		for _, row := range rows {
			if row.TemplateName == streamTemplate && row.DestroyedAt == nil {
				_ = sbx.Destroy(clean, row.ID)
			}
		}
		_ = tmgr.Delete(clean, streamTemplate)
	}
	destroyAll()
	t.Cleanup(destroyAll)

	certPEM, keyPEM, cert := guestCertificate(t)
	build(t, ctx, tmgr, template.BuildRequest{
		Name: streamTemplate, Image: agentImage, VCPUs: 1, MemoryMB: 256, DiskMB: 2048,
		Setup: []string{
			"mkdir -p /opt/stream",
			// The files travel as base64, so no quote in them meets the shell.
			"echo " + base64.StdEncoding.EncodeToString([]byte(certPEM)) + " | base64 -d > /opt/stream/cert.pem",
			"echo " + base64.StdEncoding.EncodeToString([]byte(keyPEM)) + " | base64 -d > /opt/stream/key.pem",
			"echo " + base64.StdEncoding.EncodeToString([]byte(streamServer)) + " | base64 -d > /opt/stream/serve.py",
		},
		Start:     []string{"python", "/opt/stream/serve.py"},
		StartPort: streamPort,
	}, store.TemplateReady)
	sb := createSandbox(t, ctx, sbx, sandbox.CreateRequest{
		Template: streamTemplate, Lifecycle: store.LifecyclePersistent, IdleSeconds: streamIdle,
	})

	control := httptest.NewServer((&api.Server{Store: st, Sandboxes: sbx, Templates: tmgr, Token: "operator", Root: root, Base: ctx}).Handler())
	defer control.Close()
	dial := func(port int) (*websocket.Conn, *http.Response, error) {
		url := strings.Replace(control.URL, "http", "ws", 1) + fmt.Sprintf("/v1/sandboxes/%s/ports/%d/stream", sb.ID, port)
		return websocket.Dial(ctx, url, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer operator"}},
		})
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	// open runs TLS inside a stream and checks that the guest's own
	// certificate answered.
	open := func(t *testing.T) (*websocket.Conn, *tls.Conn) {
		t.Helper()
		ws, _, err := dial(streamPort)
		if err != nil {
			t.Fatalf("open the stream: %v", err)
		}
		conn := tls.Client(websocket.NetConn(ctx, ws, websocket.MessageBinary), &tls.Config{RootCAs: pool, ServerName: "guest"})
		if err := conn.HandshakeContext(ctx); err != nil {
			t.Fatalf("TLS through the stream: %v", err)
		}
		if got := conn.ConnectionState().PeerCertificates[0]; !bytes.Equal(got.Raw, cert.Raw) {
			t.Fatalf("the stream presented %q, not the guest certificate", got.Subject)
		}
		return ws, conn
	}
	get := func(t *testing.T, conn *tls.Conn, r *bufio.Reader) {
		t.Helper()
		if _, err := io.WriteString(conn, "GET /v1/health HTTP/1.1\r\nHost: guest\r\n\r\n"); err != nil {
			t.Fatalf("send through the stream: %v", err)
		}
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("read through the stream: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "stream-ok" {
			t.Fatalf("the guest answered %d %q", resp.StatusCode, body)
		}
	}
	// closedWith waits for the host to end a stream and returns its status.
	closedWith := func(t *testing.T, ws *websocket.Conn, within time.Duration) websocket.StatusCode {
		t.Helper()
		readCtx, cancel := context.WithTimeout(ctx, within)
		defer cancel()
		for {
			_, _, err := ws.Read(readCtx)
			if err == nil {
				continue
			}
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				return ce.Code
			}
			t.Fatalf("the stream ended with no close status: %v", err)
		}
	}
	state := func(t *testing.T) string {
		t.Helper()
		row, err := st.GetSandbox(ctx, sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		return row.State
	}

	t.Run("GuestCertificate", func(t *testing.T) {
		ws, conn := open(t)
		defer ws.CloseNow()
		get(t, conn, bufio.NewReader(conn))
	})
	t.Run("NoListener", func(t *testing.T) {
		_, resp, err := dial(streamPort + 1)
		if err == nil {
			t.Fatal("a port with no listener upgraded")
		}
		if resp == nil || resp.StatusCode != http.StatusConflict {
			t.Fatalf("a port with no listener answered %v, want 409", resp)
		}
	})
	t.Run("TextMessage", func(t *testing.T) {
		ws, _, err := dial(streamPort)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.CloseNow()
		if err := ws.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
			t.Fatal(err)
		}
		if code := closedWith(t, ws, 15*time.Second); code != websocket.StatusUnsupportedData {
			t.Fatalf("a text message closed the stream with %d, want 1003", code)
		}
	})
	t.Run("TrafficKeepsAwakeAndSilenceSleeps", func(t *testing.T) {
		ws, conn := open(t)
		defer ws.CloseNow()
		r := bufio.NewReader(conn)
		until := time.Now().Add(2 * streamIdle * time.Second)
		for time.Now().Before(until) {
			get(t, conn, r)
			if got := state(t); got != store.SandboxRunning {
				t.Fatalf("the sandbox is %q while traffic flows", got)
			}
			time.Sleep(4 * time.Second)
		}
		if code := closedWith(t, ws, 4*streamIdle*time.Second); code != websocket.StatusGoingAway {
			t.Fatalf("a silent stream ended with %d, want 1001", code)
		}
		// The host closes the stream before it saves the memory, so the row
		// reads sleeping only once the snapshot is written.
		deadline := time.Now().Add(2 * time.Minute)
		for state(t) != store.SandboxSleeping {
			if time.Now().After(deadline) {
				t.Fatalf("after a silent stream the sandbox is %q, want sleeping", state(t))
			}
			time.Sleep(time.Second)
		}
		ws2, conn2 := open(t)
		defer ws2.CloseNow()
		get(t, conn2, bufio.NewReader(conn2))
		if got := state(t); got != store.SandboxRunning {
			t.Fatalf("a new stream left the sandbox %q", got)
		}
	})
	t.Run("DestroyEndsTheStream", func(t *testing.T) {
		ws, _ := open(t)
		defer ws.CloseNow()
		done := make(chan error, 1)
		go func() { done <- sbx.Destroy(ctx, sb.ID) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("destroy waited on an open stream")
		}
		if code := closedWith(t, ws, 15*time.Second); code != websocket.StatusGoingAway {
			t.Fatalf("destroy ended the stream with %d, want 1001", code)
		}
	})
}

// guestCertificate makes the self-signed certificate the guest serves and the
// client pins.
func guestCertificate(t *testing.T) (string, string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "guest"},
		DNSNames:     []string{"guest"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return string(certPEM), string(keyPEM), cert
}
