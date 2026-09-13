//go:build linux

// Command kilninit is PID 1 inside a Kiln microVM. It mounts /proc, /sys and
// /dev, then serves exec, hello and shutdown requests on a vsock port.
package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("kilninit: ")
	if err := mountBase(); err != nil {
		log.Fatalf("mount: %v", err)
	}
	l, err := vsock.Listen(guestproto.Port, nil)
	if err != nil {
		log.Fatalf("vsock listen: %v", err)
	}
	log.Printf("agent listening on vsock port %d", guestproto.Port)
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go serve(conn)
	}
}

// mountBase mounts the filesystems every exec expects. The kernel may have
// mounted some of them already; EBUSY means it is already done.
func mountBase() error {
	mounts := []struct{ source, target, fstype string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"devpts", "/dev/pts", "devpts"},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(m.source, m.target, m.fstype, 0, ""); err != nil && err != unix.EBUSY {
			return fmt.Errorf("%s: %w", m.target, err)
		}
	}
	return nil
}
