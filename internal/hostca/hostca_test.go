package hostca

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The gateway listener is the only way in from another machine, so a call
// without a certificate from this CA must not reach a handler.
func TestGatewayListenerDemandsACertificateFromThisCA(t *testing.T) {
	ca := New(t.TempDir())
	if err := ca.Ensure(); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := ca.ServerTLS("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv.TLS = tlsConfig
	srv.StartTLS()
	defer srv.Close()

	caPEM, certPEM, keyPEM, serial, err := ca.Issue("gateway-1", 30)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		t.Fatal("the CA certificate does not parse")
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	client := func(certs []tls.Certificate) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: certs,
		}}}
	}

	resp, err := client([]tls.Certificate{cert}).Get(srv.URL)
	if err != nil {
		t.Fatalf("a certificate from this CA was refused: %v", err)
	}
	resp.Body.Close()

	if _, err := client(nil).Get(srv.URL); err == nil {
		t.Fatal("a call with no client certificate reached the handler")
	}

	if err := ca.Revoke(serial); err != nil {
		t.Fatal(err)
	}
	if _, err := client([]tls.Certificate{cert}).Get(srv.URL); err == nil {
		t.Fatal("a revoked certificate still connects")
	}
}
