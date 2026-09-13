//go:build linux

package main

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"strings"

	"github.com/alternayte/kiln/internal/guestproto"
)

// guestPath validates one host-supplied guest path. It must be absolute, must
// not contain a ".." element, and must not name a directory root.
func guestPath(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "/") {
		return "", errors.New("path must be absolute")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errors.New("path must not contain ..")
		}
	}
	clean := path.Clean(p)
	if clean == "/" || clean == "." {
		return "", errors.New("path must name a file")
	}
	return clean, nil
}

func fileError(err error, missing bool) *guestproto.Error {
	if missing || errors.Is(err, fs.ErrNotExist) {
		return &guestproto.Error{Code: guestproto.CodeNotFound, Message: "no such file"}
	}
	return &guestproto.Error{Code: guestproto.CodeInvalid, Message: err.Error()}
}

// serveReadFile streams one regular file as stdout frames.
func serveReadFile(conn net.Conn, req *guestproto.Request) {
	p, err := guestPath(req.Path)
	if err != nil {
		writeResult(conn, guestproto.Result{Error: fileError(err, false)})
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		writeResult(conn, guestproto.Result{Error: fileError(err, true)})
		return
	}
	if !fi.Mode().IsRegular() {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInvalid, Message: "not a regular file"}})
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeResult(conn, guestproto.Result{Error: fileError(err, false)})
		return
	}
	defer f.Close()
	buf := make([]byte, 32<<10)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := guestproto.WriteFrame(conn, guestproto.FrameStdout, buf[:n]); err != nil {
				return
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: readErr.Error()}})
			return
		}
	}
	writeResult(conn, guestproto.Result{OK: true})
}

// serveWriteFile reads data frames until a zero-length frame, writes a
// temporary file in the target directory, and renames it over the path.
func serveWriteFile(conn net.Conn, req *guestproto.Request) {
	p, err := guestPath(req.Path)
	if err != nil {
		writeResult(conn, guestproto.Result{Error: fileError(err, false)})
		return
	}
	if fi, err := os.Stat(p); err == nil && !fi.Mode().IsRegular() {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInvalid, Message: "not a regular file"}})
		return
	}
	dir := path.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInvalid, Message: err.Error()}})
		return
	}
	tmp := path.Join(dir, "."+path.Base(p)+".kiln-tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	for {
		typ, payload, err := guestproto.ReadFrame(conn)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return
		}
		if typ != guestproto.FrameData {
			f.Close()
			os.Remove(tmp)
			writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInvalid, Message: "unexpected frame"}})
			return
		}
		if len(payload) == 0 {
			break
		}
		if _, err := f.Write(payload); err != nil {
			f.Close()
			os.Remove(tmp)
			writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
			return
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	writeResult(conn, guestproto.Result{OK: true})
}
