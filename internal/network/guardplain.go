package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// plainConn is one guest connection on port 80. The guard decides each request
// on it by its Host header, so one connection cannot reach a second host
// through an allowed one.
type plainConn struct {
	net.Conn
	g *guardServer
	// dest is the address the guest chose, read before the server can close
	// the socket.
	dest string

	once   sync.Once
	closed chan struct{}

	mu sync.Mutex
	// host is the last allowed host of this connection, and it takes the bytes.
	// What crosses before the first allowed request waits in pendingIn and
	// pendingOut.
	host       string
	decision   string
	pendingIn  int64
	pendingOut int64
	// served is true once one request reached the handler.
	served bool
}

func (c *plainConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.count(0, int64(n))
	}
	return n, err
}

func (c *plainConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.count(int64(n), 0)
	}
	return n, err
}

func (c *plainConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *plainConn) count(in, out int64) {
	c.mu.Lock()
	host, decision := c.host, c.decision
	if host == "" {
		c.pendingIn += in
		c.pendingOut += out
	}
	c.mu.Unlock()
	if host != "" {
		c.g.record(Record{Host: host, Port: plainPort, Decision: decision, BytesIn: in, BytesOut: out, Last: time.Now().Unix()})
	}
}

// serving marks that a request reached the handler.
func (c *plainConn) serving() {
	c.mu.Lock()
	c.served = true
	c.mu.Unlock()
}

// allowed makes one host take the bytes of this connection from now on.
func (c *plainConn) allowed(host, decision string) {
	c.mu.Lock()
	c.host, c.decision = host, decision
	in, out := c.pendingIn, c.pendingOut
	c.pendingIn, c.pendingOut = 0, 0
	c.mu.Unlock()
	if in > 0 || out > 0 {
		c.g.record(Record{Host: host, Port: plainPort, Decision: decision, BytesIn: in, BytesOut: out, Last: time.Now().Unix()})
	}
}

type plainConnKey struct{}

// addrsKey carries the addresses the handler resolved to the dial, so the
// transport connects where the decision looked.
type addrsKey struct{}

// startPlain starts the HTTP server of port 80. It lives as long as the guard.
func (g *guardServer) startPlain(ctx context.Context) {
	g.plainListener = &connListener{conns: make(chan net.Conn), done: make(chan struct{})}
	g.plainTransport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			addrs, _ := ctx.Value(addrsKey{}).([]netip.Addr)
			if len(addrs) == 0 {
				return nil, errors.New("no address for this request")
			}
			dial, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			return g.dial(dial, addrs, plainPort)
		},
		// A pooled connection belongs to one host and the addresses one request
		// resolved. Each request dials its own, so a pooled connection never
		// carries a second lookup.
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(g.servePlain),
		ReadHeaderTimeout: helloTimeout,
		IdleTimeout:       5 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnContext: func(base context.Context, conn net.Conn) context.Context {
			return context.WithValue(base, plainConnKey{}, conn)
		},
	}
	go func() {
		<-ctx.Done()
		g.plainListener.Close()
	}()
	go func() { _ = server.Serve(g.plainListener) }()
}

// handlePlain gives one port 80 connection to the HTTP server and holds it
// until the server ends it. A connection that sends no request the server can
// read is refused by the address the guest chose.
func (g *guardServer) handlePlain(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if !g.admit(conn) {
		return
	}
	defer g.untrack(conn)
	guest := &plainConn{Conn: conn, g: g, dest: guestDestination(conn), closed: make(chan struct{})}
	select {
	case g.plainListener.conns <- guest:
	case <-ctx.Done():
		return
	}
	select {
	case <-guest.closed:
	case <-ctx.Done():
	}
	guest.mu.Lock()
	served := guest.served
	guest.mu.Unlock()
	if !served {
		now := time.Now().Unix()
		g.record(Record{Host: guest.dest, Port: plainPort, Decision: DecisionDenied, Count: 1, First: now, Last: now})
	}
}

// servePlain decides one request by its Host header and forwards it unchanged
// when the host is allowed. It follows no redirect and injects no credential.
func (g *guardServer) servePlain(w http.ResponseWriter, r *http.Request) {
	conn, _ := r.Context().Value(plainConnKey{}).(*plainConn)
	if conn != nil {
		conn.serving()
	}
	host := plainHost(r.Host)
	now := time.Now().Unix()
	deny := func(name, reason string) {
		g.record(Record{Host: name, Port: plainPort, Decision: DecisionDenied, Count: 1, First: now, Last: now})
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "kiln egress guard: %s\n", reason)
	}
	if host == "" {
		name := ""
		if conn != nil {
			name = conn.dest
		}
		deny(name, "the request names no host")
		return
	}
	if r.Method == http.MethodConnect {
		deny(host, "this method is not forwarded")
		return
	}
	decision, _ := g.decide(host)
	if decision == DecisionDenied {
		deny(host, host+" is not on the egress allow list")
		return
	}
	lookup, cancel := context.WithTimeout(r.Context(), dialTimeout)
	addrs, err := g.resolve(lookup, host)
	cancel()
	if errors.Is(err, errNonPublic) {
		log.Printf("egress guard: %s: %v", host, err)
		deny(host, host+" resolves to an address that is not public")
		return
	}
	if err != nil {
		log.Printf("egress guard: %s: %v", host, err)
		http.Error(w, "kiln egress guard: no address for "+host, http.StatusBadGateway)
		return
	}
	g.record(Record{Host: host, Port: plainPort, Decision: decision, Count: 1, First: now, Last: now})
	if conn != nil {
		conn.allowed(host, decision)
	}
	r = r.WithContext(context.WithValue(r.Context(), addrsKey{}, addrs))
	reverse := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = host
			pr.Out.Host = r.Host
		},
		Transport:     g.plainTransport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("egress guard: %s: %v", host, sanitizeProxyError(err))
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	reverse.ServeHTTP(w, r)
}

// plainHost returns the name of a Host header without its port.
func plainHost(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	return strings.ToLower(host)
}
