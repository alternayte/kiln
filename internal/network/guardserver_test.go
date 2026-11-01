package network

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"testing"
	"time"
)

// A hello split across writes still yields its name, and the bytes for replay.
func TestReadHelloNamesTheServer(t *testing.T) {
	for _, tc := range []struct{ sni, want string }{
		{"PyPI.org", "pypi.org"},
		{"", ""},
	} {
		guest, guard := net.Pipe()
		go func() {
			conn := tls.Client(guest, &tls.Config{ServerName: tc.sni, InsecureSkipVerify: true})
			_ = conn.HandshakeContext(context.Background())
			guest.Close()
		}()
		_ = guard.SetDeadline(time.Now().Add(5 * time.Second))
		name, hello, err := readHello(guard)
		guard.Close()
		if err != nil {
			t.Fatalf("%q: %v", tc.sni, err)
		}
		if name != tc.want {
			t.Errorf("name %q, want %q", name, tc.want)
		}
		if len(hello) == 0 || hello[0] != 0x16 {
			t.Errorf("%q: no hello bytes for replay", tc.sni)
		}
	}
}

// Bytes that are not TLS name nothing, so the guard refuses them.
func TestReadHelloRefusesOtherBytes(t *testing.T) {
	guest, guard := net.Pipe()
	go func() {
		_, _ = guest.Write([]byte("GET / HTTP/1.1\r\nHost: pypi.org\r\n\r\n"))
		guest.Close()
	}()
	_ = guard.SetDeadline(time.Now().Add(5 * time.Second))
	defer guard.Close()
	if name, _, err := readHello(guard); err == nil {
		t.Fatalf("plain HTTP read as a hello for %q", name)
	}
}

func TestIsPublic(t *testing.T) {
	own := netip.MustParseAddr("203.0.113.9")
	for addr, want := range map[string]bool{
		"151.101.0.223":   true,
		"2606:4700::1111": true,
		"169.254.169.254": false,
		"127.0.0.1":       false,
		"10.0.0.1":        false,
		"172.31.0.1":      false,
		"192.168.1.1":     false,
		"100.64.0.1":      false,
		"::1":             false,
		"fe80::1":         false,
		"fd00:ec2::254":   false,
		"::ffff:10.0.0.1": false,
		"64:ff9b::a00:1":  false,
		"224.0.0.1":       false,
		"0.0.0.0":         false,
		"203.0.113.9":     false,
	} {
		if got := isPublic(netip.MustParseAddr(addr), []netip.Addr{own}); got != want {
			t.Errorf("isPublic(%s) = %t, want %t", addr, got, want)
		}
	}
}

func TestParseRefusal(t *testing.T) {
	short, host, port, ok := parseRefusal("4,99,1234,-;kiln_egress_abc123 IN=kiln-abc123 OUT=eth0 SRC=172.31.0.2 DST=1.2.3.4 LEN=60 PROTO=UDP SPT=5 DPT=443 LEN=8 \n")
	if !ok || short != "abc123" || host != "1.2.3.4" || port != 443 {
		t.Fatalf("got %q %q %d %t", short, host, port, ok)
	}
	if _, _, _, ok := parseRefusal("6,1,2,-;usb 1-1: new device\n"); ok {
		t.Fatal("an unrelated line read as a refusal")
	}
}

// A hostile guest cannot grow the batch without limit, and what has no room
// still counts.
func TestAddCountsWhatHasNoRoom(t *testing.T) {
	m := New("", "")
	m.heldCount = reportBatch
	m.add("a", []Record{{Host: "x", Decision: DecisionDenied, Count: 3}}, 0)
	if m.heldDropped["a"] != 3 {
		t.Fatalf("dropped %d", m.heldDropped["a"])
	}
}
