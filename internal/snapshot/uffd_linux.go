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
	"os/signal"
	"syscall"
	"time"
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
	// fillBlock is how much one step of a fill copies between two looks at the
	// fault queue.
	fillBlock = 2 << 20
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
// read from the file one at a time, and preloaded only when a SIGUSR1 asks for
// a fill before a snapshot.
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
	return servePages(ctx, memPath, mappings, uffd)
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

func servePages(ctx context.Context, memPath string, mappings []mapping, uffd int) (err error) {
	faults := 0
	defer func() { log.Printf("snapshot: done: %v (%d faults)", err, faults) }()
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
	fillAsked := make(chan os.Signal, 1)
	signal.Notify(fillAsked, syscall.SIGUSR1)
	defer signal.Stop(fillAsked)
	var active *fill
	filled := false
	// The fill runs on this one thread, between faults, so a fault never waits
	// for more than one block and a removed page cannot race with a copy.
	step := func() {
		if active == nil {
			return
		}
		done, err := active.next(mem, mappings, uffd)
		if err != nil {
			// Faults still serve what the fill did not reach.
			log.Printf("snapshot: fill stopped: %v", err)
			active = nil
		} else if done {
			log.Printf("snapshot: fill done in %s", time.Since(active.began))
			active = nil
			filled = true
		}
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-fillAsked:
			if active == nil && !filled {
				active = &fill{began: time.Now(), buf: make([]byte, fillBlock)}
			}
		default:
		}
		timeout := 200
		if active != nil {
			timeout = 0
		}
		pfds := []unix.PollFd{{Fd: int32(uffd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfds, timeout)
		if err != nil && err != unix.EINTR {
			return fmt.Errorf("snapshot: poll uffd: %w", err)
		}
		if n == 0 || pfds[0].Revents&unix.POLLIN == 0 {
			if pfds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
				return nil
			}
			step()
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
				faults++
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
		step()
	}
}

// fill walks guest memory and copies every page the guest has not touched.
type fill struct {
	began  time.Time
	buf    []byte
	region int
	offset uint64
}

// next copies one block and reports whether the walk reached the end.
func (f *fill) next(mem *os.File, mappings []mapping, uffd int) (bool, error) {
	if f.region >= len(mappings) {
		return true, nil
	}
	m := mappings[f.region]
	pageSize := m.PageSize
	if pageSize == 0 {
		pageSize = 4096
	}
	size := uint64(len(f.buf)) &^ (pageSize - 1)
	if size == 0 {
		size = pageSize
		f.buf = make([]byte, size)
	}
	if rest := m.Size - f.offset; rest < size {
		size = rest
	}
	buf := f.buf[:size]
	clear(buf)
	if _, err := mem.ReadAt(buf, int64(m.Offset+f.offset)); err != nil && err != io.EOF {
		return false, fmt.Errorf("read memory file: %w", err)
	}
	if err := copyRange(uffd, m.BaseHostVirtAddr+f.offset, buf, pageSize); err != nil {
		return false, err
	}
	f.offset += size
	if f.offset >= m.Size {
		f.region++
		f.offset = 0
	}
	return f.region >= len(mappings), nil
}

// copyRange copies buf to dst and steps over each page already in place. The
// kernel stops a copy at the first such page and reports how far it got.
func copyRange(uffd int, dst uint64, buf []byte, pageSize uint64) error {
	for done := uint64(0); done < uint64(len(buf)); {
		args := uffdioCopyArgs{
			Dst: dst + done,
			Src: uint64(uintptr(unsafe.Pointer(&buf[done]))),
			Len: uint64(len(buf)) - done,
		}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioCopyNumber, uintptr(unsafe.Pointer(&args)))
		switch {
		case errno == 0:
			return nil
		case errno == unix.EEXIST:
			done += pageSize
		case errno == unix.EAGAIN:
			if args.Copy > 0 {
				done += uint64(args.Copy)
			}
		default:
			return fmt.Errorf("uffdio_copy %#x: %w", args.Dst, errno)
		}
	}
	return nil
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
