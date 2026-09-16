// Package gateway is the public face of Kiln. It authenticates a caller,
// names the tenant, and proxies the call to the host over mTLS. The host
// holds the sandboxes and decides what the named tenant reaches.
package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Host is the link to one Kiln host. The certificate proves which service
// calls, and the token is rotatable without a new certificate.
type Host struct {
	URL       *url.URL
	Token     string
	Transport http.RoundTripper
}

// HostCredentials are the values kiln gateway-env prints.
type HostCredentials struct {
	URL        string
	Token      string
	CACert     []byte
	ClientCert []byte
	ClientKey  []byte
}

// NewHost builds the link. It fails when a credential is missing, so a
// gateway never starts in a state where it cannot reach the host.
func NewHost(c HostCredentials) (*Host, error) {
	if c.URL == "" {
		return nil, errors.New("gateway: the host URL is required")
	}
	if c.Token == "" {
		return nil, errors.New("gateway: the host token is required")
	}
	parsed, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("gateway: the host URL: %w", err)
	}
	host := &Host{URL: parsed, Token: c.Token}
	if parsed.Scheme == "http" {
		// A host on the same box answers on the loopback control listener,
		// which needs no certificate.
		host.Transport = http.DefaultTransport
		return host, nil
	}
	if len(c.CACert) == 0 || len(c.ClientCert) == 0 || len(c.ClientKey) == 0 {
		return nil, errors.New("gateway: an https host needs the CA certificate, the client certificate and the client key")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(c.CACert) {
		return nil, errors.New("gateway: the CA certificate does not parse")
	}
	cert, err := tls.X509KeyPair(c.ClientCert, c.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("gateway: the client certificate: %w", err)
	}
	host.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
		},
		// A preview and an exec stream hold a connection, so the pool stays
		// generous and idle connections do not linger.
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	return host, nil
}
