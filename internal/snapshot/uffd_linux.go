//go:build linux

package snapshot

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mapping mirrors Firecracker's GuestRegionUffdMapping.
type mapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
}

const (
	// Event numbers from include/uapi/linux/userfaultfd.h.
	uffdEventPagefault = 0x12
	uffdEventRemove    = 0x15
	uffdMsgSize        = 32
	// ioctl numbers for struct uffdio_copy and struct uffdio_zeropage on
	// 64-bit Linux: _IOWR(0xAA, number, 40 or 32 bytes).
	uffdioCopyNumber     = 0xc028aa03
	uffdioZeropageNumber = 0xc020aa04
)

type uffdioCopyArgs struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

type uffdioZeropageArgs struct {
	Dst      uint64
	Len      uint64
	Mode     uint64
	ZeroPage int64
}

// Serve listens on sockPath, takes the UFFD descriptor and the guest memory
// layout from Firecracker, and serves pages from memPath on demand. Pages are
// read from the file one at a time, never preloaded.
func Serve(ctx context.Context, memPath, sockPath string, ready func()) error {
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("snapshot: socket: %w", err)
	}
	defer unix.Close(listener)
	if err := unix.Bind(listener, &unix.SockaddrUnix{Name: sockPath}); err != nil {
		return fmt.Errorf("snapshot: bind %s: %w", sockPath, err)
	}
	defer os.Remove(sockPath)
	if err := unix.Listen(listener, 1); err != nil {
		return fmt.Errorf("snapshot: listen: %w", err)
	}
	if err := os.Chmod(sockPath, 0o666); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}

	conn, err := acceptContext(ctx, listener)
	if err != nil {
		return err
	}
	defer unix.Close(conn)
	mappings, uffd, err := receiveUffd(conn)
	if err != nil {
		return err
	}
	defer unix.Close(uffd)
	log.Printf("snapshot: serving %d region(s) from %s", len(mappings), memPath)
	err = servePages(ctx, memPath, mappings, uffd)
	log.Printf("snapshot: done: %v", err)
	return err
}

func acceptContext(ctx context.Context, listener int) (int, error) {
	for {
		pfds := []unix.PollFd{{Fd: int32(listener), Events: unix.POLLIN}}
		n, err := unix.Poll(pfds, 200)
		if err != nil && err != unix.EINTR {
			return 0, fmt.Errorf("snapshot: poll: %w", err)
		}
		if n > 0 && pfds[0].Revents&unix.POLLIN != 0 {
			fd, _, err := unix.Accept(listener)
			if err != nil {
				return 0, fmt.Errorf("snapshot: accept: %w", err)
			}
			return fd, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
	}
}

// receiveUffd reads Firecracker's message: the memory layout as JSON and the
// UFFD descriptor as an SCM_RIGHTS control message.
func receiveUffd(conn int) ([]mapping, int, error) {
	buf := make([]byte, 64<<10)
	oob := make([]byte, 1024)
	n, oobn, _, _, err := unix.Recvmsg(conn, buf, oob, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("snapshot: recvmsg: %w", err)
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, 0, fmt.Errorf("snapshot: control message: %w", err)
	}
	if len(scms) == 0 {
		return nil, 0, fmt.Errorf("snapshot: no control message with the uffd")
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		return nil, 0, fmt.Errorf("snapshot: rights: %w", err)
	}
	if len(fds) == 0 {
		return nil, 0, fmt.Errorf("snapshot: no uffd descriptor received")
	}
	var mappings []mapping
	if err := json.Unmarshal(buf[:n], &mappings); err != nil {
		return nil, 0, fmt.Errorf("snapshot: memory layout: %w", err)
	}
	if len(mappings) == 0 {
		return nil, 0, fmt.Errorf("snapshot: empty guest memory layout")
	}
	return mappings, fds[0], nil
}

func servePages(ctx context.Context, memPath string, mappings []mapping, uffd int) error {
	mem, err := os.Open(memPath)
	if err != nil {
		return fmt.Errorf("snapshot: memory file: %w", err)
	}
	defer mem.Close()

	var maxPage uint64
	for _, m := range mappings {
		if m.PageSize > maxPage {
			maxPage = m.PageSize
		}
	}
	if maxPage == 0 {
		maxPage = 4096
	}
	page := make([]byte, maxPage)
	events := make([]byte, uffdMsgSize*16)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pfds := []unix.PollFd{{Fd: int32(uffd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfds, 200)
		if err != nil && err != unix.EINTR {
			return fmt.Errorf("snapshot: poll uffd: %w", err)
		}
		if n == 0 || pfds[0].Revents&unix.POLLIN == 0 {
			if pfds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
				return nil
			}
			continue
		}
		rn, err := unix.Read(uffd, events)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return fmt.Errorf("snapshot: read uffd: %w", err)
		}
		if rn <= 0 {
			return nil
		}
		for off := 0; off+uffdMsgSize <= rn; off += uffdMsgSize {
			switch events[off] {
			case uffdEventPagefault:
				addr := binary.LittleEndian.Uint64(events[off+16 : off+24])
				if err := servePage(mem, page, mappings, uffd, addr); err != nil {
					return err
				}
			case uffdEventRemove:
				start := binary.LittleEndian.Uint64(events[off+8 : off+16])
				end := binary.LittleEndian.Uint64(events[off+16 : off+24])
				if err := zeroRange(page, mappings, uffd, start, end); err != nil {
					return err
				}
			}
		}
	}
}

// servePage copies one page from the memory file into the faulting address.
func servePage(mem *os.File, page []byte, mappings []mapping, uffd int, fault uint64) error {
	m, ok := regionAt(mappings, fault)
	if !ok {
		return fmt.Errorf("snapshot: fault at %#x is outside the guest memory layout", fault)
	}
	size := m.PageSize
	if size == 0 {
		size = 4096
	}
	if uint64(len(page)) < size {
		return fmt.Errorf("snapshot: page buffer is smaller than %d", size)
	}
	buf := page[:size]
	for i := range buf {
		buf[i] = 0
	}
	dst := fault &^ (size - 1)
	off := m.Offset + (dst - m.BaseHostVirtAddr)
	if _, err := mem.ReadAt(buf, int64(off)); err != nil && err != io.EOF {
		return fmt.Errorf("snapshot: read memory file: %w", err)
	}
	args := uffdioCopyArgs{
		Dst: dst,
		Src: uint64(uintptr(unsafe.Pointer(&buf[0]))),
		Len: size,
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioCopyNumber, uintptr(unsafe.Pointer(&args))); errno != 0 {
		if errno == unix.EEXIST {
			// Another handler already resolved the page.
			return nil
		}
		return fmt.Errorf("snapshot: uffdio_copy %#x: %w", dst, errno)
	}
	return nil
}

// zeroRange resolves removed (ballooned) pages with zeroes.
func zeroRange(page []byte, mappings []mapping, uffd int, start, end uint64) error {
	for addr := start; addr < end; {
		m, ok := regionAt(mappings, addr)
		if !ok {
			return nil
		}
		size := m.PageSize
		if size == 0 {
			size = 4096
		}
		if end-addr < size {
			size = end - addr
		}
		args := uffdioZeropageArgs{Dst: addr &^ (size - 1), Len: size}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioZeropageNumber, uintptr(unsafe.Pointer(&args))); errno != 0 && errno != unix.EEXIST {
			return fmt.Errorf("snapshot: uffdio_zeropage %#x: %w", addr, errno)
		}
		addr += size
	}
	return nil
}

func regionAt(mappings []mapping, addr uint64) (mapping, bool) {
	for _, m := range mappings {
		if addr >= m.BaseHostVirtAddr && addr < m.BaseHostVirtAddr+m.Size {
			return m, true
		}
	}
	return mapping{}, false
}
