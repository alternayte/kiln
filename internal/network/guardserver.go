package network

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alternayte/kiln/internal/runtime"
)

const (
	// helloTimeout bounds how long a guest takes to send its first bytes.
	helloTimeout = 10 * time.Second
	// helloLimit bounds what the guard buffers before it names a destination.
	helloLimit = 64 << 10
	// dialTimeout bounds one lookup and one connection to a destination.
	dialTimeout = 10 * time.Second
	// maxGuardConns bounds what one guest holds open through its guard, and
	// with it the host memory that guest can use here.
	maxGuardConns = 1024
	// copyBuffer is the buffer of one direction of one connection.
	copyBuffer = 16 << 10
	// maxGuardRecords bounds the destinations a guard holds while no daemon
	// reads them.
	maxGuardRecords = 4096
)

// GuardConfig names the link one guard serves.
type GuardConfig struct {
	Sock string
	Tap  string
	Mark int
}

// guardServer is the egress guard process of one sandbox.
type guardServer struct {
	cancel context.CancelFunc
	port   int

	policyMu sync.RWMutex
	allow    map[string]bool
	observe  bool
	// generation rises with each cut, so a connection that dialled under an
	// older policy does not start.
	generation int64

	connsMu sync.Mutex
	conns   map[net.Conn]struct{}

	recordsMu sync.Mutex
	records   map[recordKey]*Record
	dropped   int64
	inflight  map[int64]sentBatch
	batch     int64

	clientMu sync.Mutex
	client   net.Conn
	enc      *json.Encoder
}

// sentBatch is a report no daemon has acknowledged.
type sentBatch struct {
	records []Record
	dropped int64
}

// ServeGuard runs until the context ends or the daemon sends a stop. It holds
// no policy until a daemon gives it one, so it refuses everything at first.
func ServeGuard(ctx context.Context, cfg GuardConfig) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g := &guardServer{
		cancel: cancel, allow: map[string]bool{},
		conns: map[net.Conn]struct{}{}, records: map[recordKey]*Record{}, inflight: map[int64]sentBatch{},
	}
	lc := net.ListenConfig{Control: bindControl(cfg.Tap, cfg.Mark)}
	ln, err := lc.Listen(ctx, "tcp4", net.JoinHostPort(runtime.GuestGateway, "0"))
	if err != nil {
		return fmt.Errorf("egress guard: listen: %w", err)
	}
	defer ln.Close()
	g.port = ln.Addr().(*net.TCPAddr).Port
	_ = os.Remove(cfg.Sock)
	control, err := net.Listen("unix", cfg.Sock)
	if err != nil {
		return fmt.Errorf("egress guard: control: %w", err)
	}
	defer control.Close()
	if err := os.Chmod(cfg.Sock, 0o600); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		control.Close()
	}()
	go g.serveControl(ctx, control)
	go g.flushLoop(ctx)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("egress guard: accept: %w", err)
		}
		go g.handle(ctx, conn)
	}
}

// serveControl takes one daemon at a time. A newer daemon replaces an older
// one, and the guard keeps serving the guest between them.
func (g *guardServer) serveControl(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		g.setClient(conn)
		go g.readClient(conn)
	}
}

// setClient makes one connection the daemon. What the last daemon did not
// acknowledge goes back to be sent again.
func (g *guardServer) setClient(conn net.Conn) {
	g.clientMu.Lock()
	defer g.clientMu.Unlock()
	if g.client != nil {
		_ = g.client.SetWriteDeadline(time.Now().Add(time.Second))
		_ = g.enc.Encode(guardReport{Replaced: true})
		g.client.Close()
	}
	g.client, g.enc = conn, json.NewEncoder(conn)
	_ = conn.SetWriteDeadline(time.Now().Add(guardAnswer))
	g.recordsMu.Lock()
	for batch, sent := range g.inflight {
		g.restore(sent)
		delete(g.inflight, batch)
	}
	g.recordsMu.Unlock()
	if err := g.enc.Encode(guardReport{PID: os.Getpid(), Port: g.port}); err != nil {
		log.Printf("egress guard: hello: %v", err)
	}
}

func (g *guardServer) readClient(conn net.Conn) {
	dec := json.NewDecoder(conn)
	for {
		var cmd guardCommand
		if err := dec.Decode(&cmd); err != nil {
			g.clientMu.Lock()
			if g.client == conn {
				g.client, g.enc = nil, nil
			}
			g.clientMu.Unlock()
			return
		}
		switch {
		case cmd.Stop:
			g.flush()
			g.cancel()
			return
		case cmd.Batch > 0:
			g.recordsMu.Lock()
			delete(g.inflight, cmd.Batch)
			g.recordsMu.Unlock()
		case cmd.Seq > 0:
			g.setPolicy(cmd)
			g.reply(conn, guardReport{Seq: cmd.Seq})
		}
	}
}

func (g *guardServer) reply(conn net.Conn, report guardReport) {
	g.clientMu.Lock()
	defer g.clientMu.Unlock()
	if g.client == conn {
		_ = conn.SetWriteDeadline(time.Now().Add(guardAnswer))
		_ = g.enc.Encode(report)
	}
}

func (g *guardServer) setPolicy(cmd guardCommand) {
	names, err := normalizeAllow(cmd.Allow)
	if err != nil {
		// The daemon validated this list, so a bad one refuses everything.
		log.Printf("egress guard: %v", err)
		names = map[string]bool{}
	}
	g.policyMu.Lock()
	g.allow, g.observe = names, cmd.Observe
	if cmd.Cut {
		g.generation++
	}
	g.policyMu.Unlock()
	if cmd.Cut {
		g.connsMu.Lock()
		for conn := range g.conns {
			conn.Close()
		}
		g.connsMu.Unlock()
	}
}

// decide returns the decision for one name.
func (g *guardServer) decide(name string) (decision string, generation int64) {
	g.policyMu.RLock()
	defer g.policyMu.RUnlock()
	switch {
	case g.observe:
		return DecisionObserved, g.generation
	case matchAllow(g.allow, name):
		return DecisionAllowed, g.generation
	}
	return DecisionDenied, g.generation
}

// track adds a connection to what a cut closes, unless the policy moved past
// the generation it passed under.
func (g *guardServer) track(generation int64, conns ...net.Conn) bool {
	g.policyMu.RLock()
	defer g.policyMu.RUnlock()
	if generation != g.generation {
		return false
	}
	g.connsMu.Lock()
	defer g.connsMu.Unlock()
	for _, conn := range conns {
		g.conns[conn] = struct{}{}
	}
	return true
}

func (g *guardServer) untrack(conns ...net.Conn) {
	g.connsMu.Lock()
	defer g.connsMu.Unlock()
	for _, conn := range conns {
		delete(g.conns, conn)
	}
}

// handle reads the name one guest connection asks for, and forwards its bytes
// to that name or refuses it. It never decrypts anything.
func (g *guardServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	g.connsMu.Lock()
	full := len(g.conns) >= 2*maxGuardConns
	if !full {
		g.conns[conn] = struct{}{}
	}
	g.connsMu.Unlock()
	if full {
		return
	}
	defer g.untrack(conn)

	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
	name, hello, err := readHello(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil || name == "" {
		// A guard cannot name the destination of this connection, so it
		// records the address the guest chose.
		g.refuse(conn, guestDestination(conn), originalPort)
		return
	}
	decision, generation := g.decide(name)
	if decision == DecisionDenied {
		g.refuse(conn, name, originalPort)
		return
	}
	lookup, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	addrs, err := resolvePublic(lookup, name)
	if errors.Is(err, errNonPublic) {
		log.Printf("egress guard: %s: %v", name, err)
		g.refuse(conn, name, originalPort)
		return
	}
	if err != nil {
		log.Printf("egress guard: %s: %v", name, err)
		return
	}
	upstream, err := dialAny(lookup, addrs)
	if err != nil {
		log.Printf("egress guard: %s: %v", name, err)
		return
	}
	defer upstream.Close()
	if !g.track(generation, upstream) {
		return
	}
	defer g.untrack(upstream)
	now := time.Now().Unix()
	g.record(Record{Host: name, Port: originalPort, Decision: decision, Count: 1, First: now, Last: now})
	if _, err := upstream.Write(hello); err != nil {
		return
	}
	g.record(Record{Host: name, Port: originalPort, Decision: decision, BytesOut: int64(len(hello)), Last: now})

	done := make(chan struct{}, 2)
	go func() {
		g.pipe(upstream, conn, func(n int64) {
			g.record(Record{Host: name, Port: originalPort, Decision: decision, BytesOut: n, Last: time.Now().Unix()})
		})
		done <- struct{}{}
	}()
	go func() {
		g.pipe(conn, upstream, func(n int64) {
			g.record(Record{Host: name, Port: originalPort, Decision: decision, BytesIn: n, Last: time.Now().Unix()})
		})
		done <- struct{}{}
	}()
	<-done
	<-done
}

// originalPort is the one port a guard serves. nftables sends it nothing else.
const originalPort = 443

// accessDenied is a TLS fatal alert, so a client reports a refusal and not a
// broken network.
var accessDenied = []byte{0x15, 0x03, 0x01, 0x00, 0x02, 0x02, 0x31}

func (g *guardServer) refuse(conn net.Conn, host string, port int) {
	now := time.Now().Unix()
	g.record(Record{Host: host, Port: port, Decision: DecisionDenied, Count: 1, First: now, Last: now})
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write(accessDenied)
}

// pipe copies one direction and passes the end of the stream on. The buffer
// stays in user space because each read counts toward the egress record.
func (g *guardServer) pipe(dst, src net.Conn, count func(int64)) {
	buf := make([]byte, copyBuffer)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			count(int64(n))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				_ = src.Close()
				return
			}
		}
		if err != nil {
			if half, ok := dst.(*net.TCPConn); ok {
				_ = half.CloseWrite()
			}
			return
		}
	}
}

func (g *guardServer) record(rec Record) {
	g.recordsMu.Lock()
	defer g.recordsMu.Unlock()
	if !merge(g.records, recordKey{rec.Host, rec.Port, rec.Decision}, rec, maxGuardRecords-len(g.records)) {
		if rec.Count > 0 {
			g.dropped += rec.Count
		}
	}
}

// restore puts one unacknowledged report back. The caller holds recordsMu.
func (g *guardServer) restore(sent sentBatch) {
	for _, rec := range sent.records {
		if !merge(g.records, recordKey{rec.Host, rec.Port, rec.Decision}, rec, maxGuardRecords-len(g.records)) {
			g.dropped += max(rec.Count, 1)
		}
	}
	g.dropped += sent.dropped
}

func (g *guardServer) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.flush()
		}
	}
}

// flush sends what the guard holds to the daemon, when one is there.
func (g *guardServer) flush() {
	g.clientMu.Lock()
	defer g.clientMu.Unlock()
	if g.enc == nil {
		return
	}
	g.recordsMu.Lock()
	if len(g.records) == 0 && g.dropped == 0 {
		g.recordsMu.Unlock()
		return
	}
	sent := sentBatch{records: make([]Record, 0, len(g.records)), dropped: g.dropped}
	for _, rec := range g.records {
		sent.records = append(sent.records, *rec)
	}
	g.records, g.dropped = map[recordKey]*Record{}, 0
	g.batch++
	batch := g.batch
	g.inflight[batch] = sent
	g.recordsMu.Unlock()
	_ = g.client.SetWriteDeadline(time.Now().Add(guardAnswer))
	if err := g.enc.Encode(guardReport{Batch: batch, Records: sent.records, Dropped: sent.dropped}); err != nil {
		log.Printf("egress guard: report: %v", err)
	}
}

// errHelloRead ends the handshake once the ClientHello is parsed.
var errHelloRead = errors.New("client hello read")

// readHello returns the server name of a TLS ClientHello and the bytes it read
// to find it. The crypto/tls parser does the reading, so a hello split across
// records or packets needs nothing more, and no reply reaches the guest.
func readHello(conn net.Conn) (string, []byte, error) {
	var seen bytes.Buffer
	var name string
	server := tls.Server(helloConn{
		Conn:   conn,
		reader: io.TeeReader(io.LimitReader(conn, helloLimit), &seen),
	}, &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			name = hello.ServerName
			return nil, errHelloRead
		},
	})
	err := server.Handshake()
	if !errors.Is(err, errHelloRead) {
		return "", seen.Bytes(), fmt.Errorf("not a TLS client hello: %w", err)
	}
	return strings.ToLower(strings.TrimSuffix(name, ".")), seen.Bytes(), nil
}

// helloConn reads from a limited copy of a connection and writes nowhere.
type helloConn struct {
	net.Conn
	reader io.Reader
}

func (c helloConn) Read(b []byte) (int, error)  { return c.reader.Read(b) }
func (c helloConn) Write(b []byte) (int, error) { return len(b), nil }

// errNonPublic marks a name that resolves into a network no guest may reach.
var errNonPublic = errors.New("the name resolves to an address that is not public")

// nonPublic holds the ranges net/netip has no method for: shared address
// space, benchmarking, the reserved block, and the IPv6 forms that embed an
// IPv4 address.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2002::/16"),
}

// isPublic reports whether a guest may reach an address. Private, loopback,
// link-local and metadata addresses all fail, and so does each address of this
// host.
func isPublic(addr netip.Addr, local []netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublic {
		if prefix.Contains(addr) {
			return false
		}
	}
	for _, own := range local {
		if own == addr {
			return false
		}
	}
	return true
}

// localAddrs lists the addresses of this host.
func localAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if addr, ok := netip.AddrFromSlice(ipnet.IP); ok {
				out = append(out, addr.Unmap())
			}
		}
	}
	return out
}

// resolvePublic resolves a name here, so a guest cannot choose the address. One
// address outside the public network refuses the whole name: a name that mixes
// them is an attack on whichever address gets dialled.
func resolvePublic(ctx context.Context, name string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", name)
	if err != nil {
		return nil, err
	}
	local := localAddrs()
	for _, addr := range addrs {
		if !isPublic(addr, local) {
			return nil, fmt.Errorf("%w: %s", errNonPublic, addr)
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no address for %s", name)
	}
	return addrs, nil
}

// dialAny connects to the first address that answers.
func dialAny(ctx context.Context, addrs []netip.Addr) (net.Conn, error) {
	var last error
	var d net.Dialer
	for _, addr := range addrs {
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr.Unmap().String(), strconv.Itoa(originalPort)))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// guestDestination names the address a guest connected to before the redirect.
func guestDestination(conn net.Conn) string {
	if tcp, ok := conn.(*net.TCPConn); ok {
		if addr := originalDestination(tcp); addr != "" {
			return addr
		}
	}
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return conn.LocalAddr().String()
	}
	return host
}
