//go:build linux

package network

import "golang.org/x/sys/unix"

// Socket options the resolver binds its listeners with. Both pin a socket to
// one VM: the device decides the TAP, the mark decides the route table.
const (
	soBindToDevice = unix.SO_BINDTODEVICE
	soMark         = unix.SO_MARK
)
