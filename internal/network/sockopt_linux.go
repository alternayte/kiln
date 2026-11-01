//go:build linux

package network

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Socket options the resolver binds its listeners with. Both pin a socket to
// one VM: the device decides the TAP, the mark decides the route table.
const (
	soBindToDevice = unix.SO_BINDTODEVICE
	soMark         = unix.SO_MARK
)

// errRingOverrun is what a read of /dev/kmsg returns when the ring moved past
// the reader.
var errRingOverrun = syscall.EPIPE

// originalDestination returns the address a connection named before nftables
// redirected it.
func originalDestination(conn *net.TCPConn) string {
	raw, err := conn.SyscallConn()
	if err != nil {
		return ""
	}
	var out string
	if err := raw.Control(func(fd uintptr) {
		// The result is a sockaddr_in. The kernel returns it through a
		// getsockopt of this size, and this call is the one x/sys offers.
		addr, err := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if err == nil {
			out = net.IP(addr.Multiaddr[4:8]).String()
		}
	}); err != nil {
		return ""
	}
	return out
}
