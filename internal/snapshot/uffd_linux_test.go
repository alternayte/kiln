//go:build linux

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	uffdUserModeOnly   = 1
	uffdAPI            = 0xAA
	uffdioAPINumber    = 0xc018aa3f
	uffdioRegisterNum  = 0xc020aa00
	uffdioRegisterMiss = 1
	testPage           = 4096
	testPages          = 2*faultBlock/testPage + 3
	testRegionSize     = testPages * testPage
)

// TestFaultCopiesTheBlockAroundIt pins that one fault maps the aligned block
// from the memory file, that the last block of a region stops at its end, and
// that a block steps over pages already in place.
func TestFaultCopiesTheBlockAroundIt(t *testing.T) {
	debug.SetGCPercent(-1)
	defer debug.SetGCPercent(100)

	content := make([]byte, testRegionSize)
	for p := 0; p < testPages; p++ {
		for i := 0; i < testPage; i++ {
			content[p*testPage+i] = byte(p + 1)
		}
	}
	memPath := filepath.Join(t.TempDir(), "mem")
	if err := os.WriteFile(memPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	region, err := unix.Mmap(-1, 0, testRegionSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(region)
	base := uint64(uintptr(unsafe.Pointer(&region[0])))

	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly, 0, 0)
	if errno != 0 {
		t.Fatalf("userfaultfd with UFFD_USER_MODE_ONLY: %v", errno)
	}
	uffd := int(fd)
	defer unix.Close(uffd)
	api := [3]uint64{uffdAPI, 0, 0}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uffdioAPINumber, uintptr(unsafe.Pointer(&api))); errno != 0 {
		t.Fatalf("uffdio_api: %v", errno)
	}
	reg := [4]uint64{base, testRegionSize, uffdioRegisterMiss, 0}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uffdioRegisterNum, uintptr(unsafe.Pointer(&reg))); errno != 0 {
		t.Fatalf("uffdio_register: %v", errno)
	}

	mappings := []mapping{{BaseHostVirtAddr: base, Size: testRegionSize, PageSize: testPage}}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- servePages(ctx, memPath, mappings, uffd) }()
	defer func() {
		cancel()
		<-served
	}()

	// The second block has two removed pages, which the server zeroes before
	// the guest touches the block.
	blockPages := faultBlock / testPage
	removed := uint64(blockPages + 5)
	if err := zeroRange(mappings, uffd, base+removed*testPage, base+(removed+2)*testPage); err != nil {
		t.Fatal(err)
	}

	touch := func(page int) byte {
		done := make(chan byte)
		go func() {
			runtime.LockOSThread()
			done <- region[page*testPage]
		}()
		return <-done
	}

	if got := touch(3); got != 4 {
		t.Fatalf("page 3 = %d, want 4", got)
	}
	// The kernel wakes the faulting thread after the first chunk of a copy,
	// so wait for the server to finish the block.
	var resident []bool
	for deadline := time.Now().Add(5 * time.Second); ; {
		resident = residentPages(t, region)
		if resident[blockPages-1] || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for p := 0; p < testPages; p++ {
		want := p < blockPages || (uint64(p) >= removed && uint64(p) < removed+2)
		if resident[p] != want {
			t.Fatalf("after one fault in the first block, page %d resident = %v, want %v", p, resident[p], want)
		}
	}

	touch(blockPages)
	touch(testPages - 1)
	for p := 0; p < testPages; p++ {
		want := byte(p + 1)
		if uint64(p) >= removed && uint64(p) < removed+2 {
			want = 0
		}
		for _, i := range []int{0, testPage - 1} {
			if got := region[p*testPage+i]; got != want {
				t.Fatalf("page %d byte %d = %d, want %d", p, i, got, want)
			}
		}
	}
}

func residentPages(t *testing.T, region []byte) []bool {
	t.Helper()
	vec := make([]byte, len(region)/testPage)
	if _, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&region[0])), uintptr(len(region)), uintptr(unsafe.Pointer(&vec[0]))); errno != 0 {
		t.Fatalf("mincore: %v", errno)
	}
	out := make([]bool, len(vec))
	for i, v := range vec {
		out[i] = v&1 != 0
	}
	return out
}
