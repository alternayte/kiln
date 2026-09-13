package network

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/alternayte/kiln/internal/runtime"
	"golang.org/x/sys/unix"
)

// maxDNS is the largest DNS message either side accepts.
const maxDNS = 65535

// resolver answers DNS for one VM. It answers only names in that VM's
// allowlist, forwards those upstream, and installs every returned address in
// the VM's nftables set for the record's TTL.
type resolver struct {
	id    string
	tap   string
	set   string
	allow map[string]bool
	up    []string

	udp *net.UDPConn
	tcp net.Listener

	cancel context.CancelFunc
	wg     sync.WaitGroup
	sem    chan struct{}
	nft    sync.Mutex
}

// newResolver binds UDP and TCP sockets on the gateway address. The sockets
// are bound to the VM's TAP device and firewall mark, so replies leave
// through the right TAP. The guest reaches them through a DNAT rule.
func newResolver(parent context.Context, a *Attachment, allow []string, upstream []string) (*resolver, error) {
	ctx, cancel := context.WithCancel(parent)
	names, err := normalizeAllow(allow)
	if err != nil {
		cancel()
		return nil, err
	}
	r := &resolver{
		id:     a.ID,
		tap:    a.TAPName,
		set:    a.setName(),
		allow:  names,
		up:     upstream,
		cancel: cancel,
		sem:    make(chan struct{}, 16),
	}
	lc := net.ListenConfig{Control: r.control(a.mark)}
	pc, err := lc.ListenPacket(ctx, "udp4", net.JoinHostPort(runtime.GuestGateway, "0"))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("network: resolver udp: %w", err)
	}
	r.udp = pc.(*net.UDPConn)
	ln, err := lc.Listen(ctx, "tcp4", net.JoinHostPort(runtime.GuestGateway, "0"))
	if err != nil {
		r.udp.Close()
		cancel()
		return nil, fmt.Errorf("network: resolver tcp: %w", err)
	}
	r.tcp = ln
	r.wg.Add(2)
	go r.serveUDP(ctx)
	go r.serveTCP(ctx)
	return r, nil
}

// control binds the socket to the VM's TAP and mark before it is used.
func (r *resolver) control(mark int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var opErr error
		if err := c.Control(func(fd uintptr) {
			if err := unix.SetsockoptString(int(fd), unix.SOL_SOCKET, soBindToDevice, r.tap); err != nil {
				opErr = err
				return
			}
			if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, soMark, mark); err != nil {
				opErr = err
			}
		}); err != nil {
			return err
		}
		return opErr
	}
}

func (r *resolver) udpPort() int { return r.udp.LocalAddr().(*net.UDPAddr).Port }

func (r *resolver) tcpPort() int { return r.tcp.Addr().(*net.TCPAddr).Port }

// Close stops both listeners. In-flight queries finish on their own; their
// answers to a closed socket fail.
func (r *resolver) Close() {
	r.cancel()
	_ = r.udp.Close()
	_ = r.tcp.Close()
	r.wg.Wait()
}

func (r *resolver) serveUDP(ctx context.Context) {
	defer r.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, addr, err := r.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		q := make([]byte, n)
		copy(q, buf[:n])
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		go func() {
			defer func() { <-r.sem }()
			if resp := r.handle(q); resp != nil {
				_, _ = r.udp.WriteToUDP(resp, addr)
			}
		}()
	}
}

func (r *resolver) serveTCP(ctx context.Context) {
	defer r.wg.Done()
	for {
		c, err := r.tcp.Accept()
		if err != nil {
			return
		}
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			c.Close()
			return
		}
		go func() {
			defer func() { <-r.sem }()
			r.handleTCP(c)
		}()
	}
}

func (r *resolver) handleTCP(c net.Conn) {
	defer c.Close()
	for {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		var length [2]byte
		if _, err := io.ReadFull(c, length[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(length[:]))
		if n < 12 {
			return
		}
		q := make([]byte, n)
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		resp := r.handle(q)
		if resp == nil {
			return
		}
		out := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(out[:2], uint16(len(resp)))
		copy(out[2:], resp)
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// handle answers one query. A name outside the allowlist is refused; nothing
// is forwarded for it.
func (r *resolver) handle(query []byte) []byte {
	name, _, err := question(query)
	if err != nil {
		return response(query, dnsFormErr)
	}
	if !r.allowed(name) {
		return response(query, dnsRefused)
	}
	resp, err := r.forward(query)
	if err != nil {
		log.Printf("network: resolver %s: %s: %v", r.id, name, err)
		return response(query, dnsServFail)
	}
	r.install(resp)
	return resp
}

func (r *resolver) allowed(name string) bool {
	return r.allow[name]
}

// forward sends the query to the host resolvers. A truncated UDP answer is
// retried over TCP.
func (r *resolver) forward(query []byte) ([]byte, error) {
	if len(r.up) == 0 {
		return nil, errors.New("no upstream resolver")
	}
	var last error
	for _, up := range r.up {
		resp, err := exchangeUDP(up, query)
		if err != nil {
			last = err
			continue
		}
		if len(resp) >= 3 && resp[2]&0x02 != 0 {
			if tcpResp, tcpErr := exchangeTCP(up, query); tcpErr == nil {
				return tcpResp, nil
			}
		}
		return resp, nil
	}
	return nil, last
}

func exchangeUDP(upstream string, query []byte) ([]byte, error) {
	c, err := net.DialTimeout("udp", net.JoinHostPort(upstream, "53"), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, maxDNS)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func exchangeTCP(upstream string, query []byte) ([]byte, error) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(upstream, "53"), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	out := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(out[:2], uint16(len(query)))
	copy(out[2:], query)
	if _, err := c.Write(out); err != nil {
		return nil, err
	}
	var length [2]byte
	if _, err := io.ReadFull(c, length[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(length[:]))
	if n < 12 {
		return nil, errors.New("dns: short tcp answer")
	}
	resp := make([]byte, n)
	if _, err := io.ReadFull(c, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// install adds every A record address to the VM's set for the record's TTL.
func (r *resolver) install(resp []byte) {
	records, err := answers(resp)
	if err != nil {
		return
	}
	for _, rec := range records {
		if rec.typ != dnsTypeA || len(rec.data) != 4 {
			continue
		}
		ttl := rec.ttl
		if ttl < 1 {
			ttl = 1
		}
		if ttl > 86400 {
			ttl = 86400
		}
		r.addElement(net.IP(rec.data).String(), ttl)
	}
}

// addElement refreshes one address in the VM's nftables set. The delete
// makes a stale, shorter timeout give way to the new TTL.
func (r *resolver) addElement(ip string, ttl uint32) {
	r.nft.Lock()
	defer r.nft.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = run(ctx, "nft", "delete", "element", "inet", Table, r.set, "{", ip, "}")
	if _, err := run(ctx, "nft", "add", "element", "inet", Table, r.set, "{", ip,
		"timeout", fmt.Sprintf("%ds", ttl), "}"); err != nil {
		log.Printf("network: resolver %s: allow %s: %v", r.id, ip, err)
	}
}
