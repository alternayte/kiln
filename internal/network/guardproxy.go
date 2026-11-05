package network

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	// maxGuardRequests bounds the requests a guard holds while no daemon reads
	// them.
	maxGuardRequests = 4096
	// maxRequestPath bounds one path in the request log. A guest chooses it.
	maxRequestPath = 2048
)

// Binding ties one credential to one exact host. Value travels only from the
// daemon to the guard of this sandbox, and only the guard holds it opened.
type Binding struct {
	Name    string   `json:"name"`
	Host    string   `json:"host"`
	Header  string   `json:"header"`
	Value   string   `json:"value"`
	Path    string   `json:"path,omitempty"`
	Methods []string `json:"methods,omitempty"`
}

// Leaf is the certificate of one bound host. The guard holds no key that signs
// a certificate for any other name.
type Leaf struct {
	Host string `json:"host"`
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// RequestRecord is one request a guard read on a bound host. It holds no query
// string, no header value and no body.
type RequestRecord struct {
	Method  string `json:"method"`
	Host    string `json:"host"`
	Path    string `json:"path"`
	Status  int    `json:"status"`
	Binding string `json:"binding,omitempty"`
	At      int64  `json:"at"`
}

// boundHost is what the guard holds for one host it terminates TLS on.
type boundHost struct {
	cert     tls.Certificate
	bindings []Binding
}

// matchRequest reports whether a binding sends its key on one request. A path
// the guard cannot read the one way a provider reads it sends no key.
func (b Binding) matchRequest(method, escapedPath string) bool {
	if len(b.Methods) > 0 {
		found := false
		for _, m := range b.Methods {
			found = found || m == method
		}
		if !found {
			return false
		}
	}
	if b.Path == "" {
		return true
	}
	clean, ok := canonicalPath(escapedPath)
	if !ok || !strings.HasPrefix(clean, b.Path) {
		return false
	}
	return strings.HasSuffix(b.Path, "/") || len(clean) == len(b.Path) || clean[len(b.Path)] == '/'
}

// canonicalPath decodes a request path, and refuses each form that a server can
// read as another path: a dot segment, an empty segment, an encoded slash,
// backslash or dot, and a double encoding.
func canonicalPath(escaped string) (string, bool) {
	lower := strings.ToLower(escaped)
	for _, bad := range []string{"%2f", "%5c", "%2e", "%25", "\\"} {
		if strings.Contains(lower, bad) {
			return "", false
		}
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil || !strings.HasPrefix(decoded, "/") {
		return "", false
	}
	want := path.Clean(decoded)
	if strings.HasSuffix(decoded, "/") && want != "/" {
		want += "/"
	}
	if want != decoded {
		return "", false
	}
	return decoded, true
}

// applyBindings removes the header of each binding of a host from one request,
// and sets the header of those that match. A guest never chooses the credential.
// It returns the names of the bindings it sent.
func applyBindings(h http.Header, bindings []Binding, method, escapedPath string) []string {
	for _, b := range bindings {
		h.Del(b.Header)
	}
	var names []string
	for _, b := range bindings {
		if b.matchRequest(method, escapedPath) {
			h.Set(b.Header, b.Value)
			names = append(names, b.Name)
		}
	}
	return names
}

// parseBound reads what the daemon sent for bound hosts. A leaf the guard cannot
// read leaves its host undecrypted and with no key.
func parseBound(bindings []Binding, leaves []Leaf) map[string]*boundHost {
	out := map[string]*boundHost{}
	for _, leaf := range leaves {
		cert, err := tls.X509KeyPair([]byte(leaf.Cert), []byte(leaf.Key))
		if err != nil {
			log.Printf("egress guard: leaf of %s: %v", leaf.Host, err)
			continue
		}
		out[leaf.Host] = &boundHost{cert: cert}
	}
	for _, b := range bindings {
		if bound := out[b.Host]; bound != nil {
			bound.bindings = append(bound.bindings, b)
		}
	}
	return out
}

func (g *guardServer) boundFor(host string) *boundHost {
	g.policyMu.RLock()
	defer g.policyMu.RUnlock()
	return g.bound[host]
}

// connListener hands the terminated connections of a guard to one HTTP server.
type connListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *connListener) Addr() net.Addr { return &net.TCPAddr{} }

// guestConn replays the bytes readHello took, counts what crosses it, and says
// when the HTTP server closes it.
type guestConn struct {
	net.Conn
	reader io.Reader
	count  func(in, out int64)
	once   sync.Once
	closed chan struct{}
}

func (c *guestConn) Read(b []byte) (int, error) {
	n, err := c.reader.Read(b)
	if n > 0 {
		c.count(0, int64(n))
	}
	return n, err
}

func (c *guestConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.count(int64(n), 0)
	}
	return n, err
}

func (c *guestConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// connInjected remembers which bindings one guest connection used, so each adds
// to the injected row once.
type connInjected struct {
	mu    sync.Mutex
	names map[string]bool
}

type connInjectedKey struct{}

// startProxy starts the HTTP server of the terminated connections. It lives as
// long as the guard.
func (g *guardServer) startProxy(ctx context.Context) {
	g.proxyListener = &connListener{conns: make(chan net.Conn), done: make(chan struct{})}
	g.transport = &http.Transport{
		DialContext:         g.dialUpstream,
		TLSClientConfig:     g.upstreamTLS,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: dialTimeout,
		// What the guest asked for and what the provider sent pass unchanged.
		DisableCompression: true,
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(g.serveProxy),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		// A pinned client fails each handshake, and a guest must not fill the
		// journal with that.
		ErrorLog: log.New(io.Discard, "", 0),
		// The server only sees conns that crypto/tls already wraps. Naming the
		// protocols here turns on HTTP/2 for them.
		TLSConfig: &tls.Config{NextProtos: []string{"h2", "http/1.1"}},
		ConnContext: func(base context.Context, _ net.Conn) context.Context {
			return context.WithValue(base, connInjectedKey{}, &connInjected{names: map[string]bool{}})
		},
	}
	go func() {
		<-ctx.Done()
		g.proxyListener.Close()
	}()
	go func() { _ = server.Serve(g.proxyListener) }()
}

// dialUpstream resolves a bound host here, like every other connection.
func (g *guardServer) dialUpstream(ctx context.Context, _, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dial, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	addrs, err := g.resolve(dial, host)
	if err != nil {
		return nil, err
	}
	return g.dial(dial, addrs, originalPort)
}

// serveBound terminates TLS on one guest connection and holds it until the HTTP server
// ends it. The caller already holds its decision and record.
func (g *guardServer) serveBound(ctx context.Context, conn net.Conn, name, decision string, hello []byte) {
	guest := &guestConn{
		Conn:   conn,
		reader: io.MultiReader(strings.NewReader(string(hello)), conn),
		closed: make(chan struct{}),
		count: func(in, out int64) {
			g.record(Record{Host: name, Port: originalPort, Decision: decision, BytesIn: in, BytesOut: out, Last: time.Now().Unix()})
		},
	}
	server := tls.Server(guest, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		// A key rotation or a cut can reach this connection before its handshake,
		// so the certificate comes from what the guard holds now.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			bound := g.boundFor(name)
			if bound == nil {
				return nil, errors.New("this host is no longer bound")
			}
			return &bound.cert, nil
		},
	})
	select {
	case g.proxyListener.conns <- server:
	case <-ctx.Done():
		return
	}
	select {
	case <-guest.closed:
	case <-ctx.Done():
	}
}

// serveProxy forwards one request of a guest to its bound host with the key. It
// follows no redirect: the guest receives the answer as the provider sent it.
func (g *guardServer) serveProxy(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || r.Method == http.MethodConnect {
		http.Error(w, "kiln egress guard: this method is not forwarded", http.StatusMethodNotAllowed)
		return
	}
	host := strings.ToLower(strings.TrimSuffix(r.TLS.ServerName, "."))
	escaped := r.URL.EscapedPath()
	var names []string
	logged := func(status int) {
		g.recordRequest(RequestRecord{
			Method: r.Method, Host: host, Path: truncatePath(escaped), Status: status,
			Binding: strings.Join(names, ","), At: time.Now().Unix(),
		})
	}
	reverse := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = host
			pr.Out.Host = host
			var bindings []Binding
			if bound := g.boundFor(host); bound != nil {
				bindings = bound.bindings
			}
			names = applyBindings(pr.Out.Header, bindings, r.Method, escaped)
			g.recordInjected(r.Context(), host, names)
		},
		Transport:     g.transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			logged(resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("egress guard: %s: %v", host, sanitizeProxyError(err))
			logged(http.StatusBadGateway)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	reverse.ServeHTTP(w, r)
}

// sanitizeProxyError keeps what a transport names and leaves out a request URL,
// which can hold a query string.
func sanitizeProxyError(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

func truncatePath(p string) string {
	if len(p) > maxRequestPath {
		return p[:maxRequestPath]
	}
	return p
}

// recordInjected adds each binding to its row once per guest connection.
func (g *guardServer) recordInjected(ctx context.Context, host string, names []string) {
	seen, _ := ctx.Value(connInjectedKey{}).(*connInjected)
	now := time.Now().Unix()
	for _, name := range names {
		if seen != nil {
			seen.mu.Lock()
			used := seen.names[name]
			seen.names[name] = true
			seen.mu.Unlock()
			if used {
				continue
			}
		}
		g.record(Record{Host: host, Port: originalPort, Decision: DecisionInjected, Binding: name, Count: 1, First: now, Last: now})
	}
}

func (g *guardServer) recordRequest(rec RequestRecord) {
	g.recordsMu.Lock()
	defer g.recordsMu.Unlock()
	if len(g.requests) >= maxGuardRequests {
		g.requestsDropped++
		return
	}
	g.requests = append(g.requests, rec)
}

// Covers reports whether an egress list reaches one host.
func Covers(allow []string, host string) bool {
	names, err := normalizeAllow(allow)
	if err != nil {
		return false
	}
	return matchAllow(names, host)
}

// ValidBindingPath reports whether a binding names a path prefix a request can
// match.
func ValidBindingPath(p string) bool {
	clean, ok := canonicalPath(p)
	return ok && clean == p
}
