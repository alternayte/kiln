//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sync"
	"unsafe"

	"github.com/alternayte/kiln/internal/guestproto"
	"golang.org/x/sys/unix"
)

// Resume state. The host sends the hook data once, when the restored sandbox
// becomes running.
var (
	resumeMu sync.Mutex
	secrets  map[string]string
	pivoted  bool
)

// applyResume runs the resume hooks in order: the overlay root, the clock, the
// entropy pool, the hostname and the secrets.
func applyResume(req *guestproto.Request) error {
	if err := pivotOverlay(); err != nil {
		return fmt.Errorf("overlay: %w", err)
	}
	if req.UnixNanos > 0 {
		ts := unix.NsecToTimespec(req.UnixNanos)
		if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
			return fmt.Errorf("clock: %w", err)
		}
	}
	if len(req.Entropy) > 0 {
		if err := addEntropy(req.Entropy); err != nil {
			return fmt.Errorf("entropy: %w", err)
		}
	}
	if req.Hostname != "" {
		if err := unix.Sethostname([]byte(req.Hostname)); err != nil {
			return fmt.Errorf("hostname: %w", err)
		}
	}
	resumeMu.Lock()
	secrets = req.Secrets
	resumeMu.Unlock()
	return nil
}

// injectedSecrets returns the secrets sent by the host.
func injectedSecrets() map[string]string {
	resumeMu.Lock()
	defer resumeMu.Unlock()
	return secrets
}

const (
	overlayDevice = "/dev/vdb"
	// scratchDir and newRootDir live on the /tmp tmpfs that kilninit mounted
	// at boot, so the read-only rootfs is never written before the pivot.
	scratchDir = "/tmp/kiln-upper"
	newRootDir = "/tmp/kiln-root"
)

// pivotOverlay mounts the sandbox overlay and pivots the root into it. The
// template rootfs is the lower layer and /dev/vdb is the writable upper. A
// cold boot has no overlay drive and does nothing.
func pivotOverlay() error {
	resumeMu.Lock()
	defer resumeMu.Unlock()
	if pivoted {
		return nil
	}
	if _, err := os.Stat(overlayDevice); err != nil {
		pivoted = true
		return nil
	}
	if isOverlayRoot() {
		pivoted = true
		return nil
	}
	if err := mountAndPivot(); err != nil {
		return err
	}
	pivoted = true
	return nil
}

func isOverlayRoot() bool {
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		return false
	}
	return st.Type == unix.OVERLAYFS_SUPER_MAGIC
}

func mountAndPivot() error {
	for _, dir := range []string{scratchDir, newRootDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount(overlayDevice, scratchDir, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s: %w", overlayDevice, err)
	}
	log.Printf("pivot: mounted %s", overlayDevice)
	upper := scratchDir + "/upper"
	work := scratchDir + "/work"
	for _, dir := range []string{upper, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	options := "lowerdir=/,upperdir=" + upper + ",workdir=" + work
	if err := unix.Mount("overlay", newRootDir, "overlay", 0, options); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	log.Printf("pivot: mounted overlay")
	// Keep the pseudofilesystems in the new root, and give the sandbox a
	// memory-backed /tmp of its own.
	for _, target := range []string{"proc", "sys", "dev", "tmp"} {
		if err := os.MkdirAll(newRootDir+"/"+target, 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("tmpfs", newRootDir+"/tmp", "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount new tmpfs: %w", err)
	}
	for _, m := range []struct{ source, target string }{
		{"/proc", newRootDir + "/proc"},
		{"/sys", newRootDir + "/sys"},
		{"/dev", newRootDir + "/dev"},
	} {
		if err := unix.Mount(m.source, m.target, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move %s: %w", m.source, err)
		}
	}
	log.Printf("pivot: moved pseudofilesystems")
	if err := os.MkdirAll(newRootDir+"/.kiln-oldroot", 0o755); err != nil {
		return err
	}
	if err := unix.Chdir(newRootDir); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", ".kiln-oldroot"); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	_ = unix.Unmount("/.kiln-oldroot", unix.MNT_DETACH)
	_ = os.Remove("/.kiln-oldroot")
	log.Printf("pivot: root is the overlay")
	return nil
}

// addEntropy feeds host bytes to the kernel entropy pool before user
// processes run. Without it every restored copy would generate the same
// random values.
func addEntropy(b []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 8+len(b))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(b))*8)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(b)))
	copy(buf[8:], b)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
