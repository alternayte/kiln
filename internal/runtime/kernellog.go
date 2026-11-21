package runtime

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// kernelLogReadLimit bounds the bytes one read takes from the end of
	// the file. A kernel line is at most 1 KiB, so 2000 lines fit in it.
	kernelLogReadLimit = 4 << 20
	// kernelLogTrimAt is the size at which a read trims the file to its
	// last kernelLogKeep bytes. The serial rate limit bounds how fast the
	// file grows, and the trim bounds its size.
	kernelLogTrimAt = 8 << 20
	kernelLogKeep   = 1 << 20
)

// kernelLogTrim serialises trims, so two reads do not both write the tail.
var kernelLogTrim sync.Mutex

// KernelLogLines returns the last n lines of the serial output of the VM that
// last ran in dir, oldest first. The guest kernel and kilninit write there.
// Each VM start truncates the file, so the lines belong to one VM. A missing
// file gives no lines.
func KernelLogLines(dir string, n int) ([]string, error) {
	path := filepath.Join(dir, "console.log")
	if err := trimKernelLog(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := max(size-kernelLogReadLimit, 0)
	b := make([]byte, size-offset)
	if _, err := f.ReadAt(b, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// A read that starts inside the file starts inside a line, so that
	// line is dropped.
	if offset > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			b = nil
		}
	}
	text := strings.ReplaceAll(string(b), "\r", "")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{}, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// trimKernelLog cuts a file larger than kernelLogTrimAt to its last
// kernelLogKeep bytes, from a line start. Firecracker writes with O_APPEND, so
// its next write lands after the kept tail. A line written between the read
// and the truncate is lost; the rate limit makes that rare.
func trimKernelLog(path string) error {
	kernelLogTrim.Lock()
	defer kernelLogTrim.Unlock()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= kernelLogTrimAt {
		return nil
	}
	tail := make([]byte, kernelLogKeep)
	if _, err := f.ReadAt(tail, info.Size()-kernelLogKeep); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.Write(tail)
	return err
}
