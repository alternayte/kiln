//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
	"golang.org/x/sys/unix"
)

// serve handles one agent connection. One connection carries one request.
func serve(conn net.Conn) {
	defer conn.Close()
	var req guestproto.Request
	typ, err := guestproto.ReadJSON(conn, &req)
	if err != nil || typ != guestproto.FrameRequest {
		return
	}
	switch req.Op {
	case guestproto.OpHello:
		writeResult(conn, guestproto.Result{OK: true})
	case guestproto.OpShutdown:
		writeResult(conn, guestproto.Result{OK: true})
		powerOff()
	case guestproto.OpResume:
		if err := applyResume(&req); err != nil {
			writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
			return
		}
		writeResult(conn, guestproto.Result{OK: true})
	case guestproto.OpExec:
		runExec(conn, &req)
	case guestproto.OpStart:
		runStart(conn, &req)
	case guestproto.OpAwait:
		runAwait(conn, &req)
	case guestproto.OpTerminal:
		runTerminal(conn, &req)
	case guestproto.OpReadFile:
		serveReadFile(conn, &req)
	case guestproto.OpWriteFile:
		serveWriteFile(conn, &req)
	default:
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{
			Code:    guestproto.CodeInvalid,
			Message: "unknown op " + req.Op,
		}})
	}
}

func writeResult(conn net.Conn, res guestproto.Result) {
	_ = guestproto.WriteJSON(conn, guestproto.FrameResult, res)
}

// powerOff asks the kernel to power the guest off. Firecracker exits when the
// guest does. The sync flushes every mounted filesystem first, so a stopped
// sandbox leaves a consistent image behind.
func powerOff() {
	unix.Sync()
	// Leave a clean image behind. An overlay root may refuse the remount.
	_ = unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		log.Printf("poweroff: %v", err)
	}
}

// session serializes frames from the two output pumps onto one connection.
type session struct {
	conn net.Conn
	mu   sync.Mutex
}

func (s *session) stream(typ byte, r io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.mu.Lock()
			werr := guestproto.WriteFrame(s.conn, typ, buf[:n])
			s.mu.Unlock()
			if werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// runExec runs one command in its own process group. It kills the whole group
// when the timeout expires or the host drops the connection.
func runExec(conn net.Conn, req *guestproto.Request) {
	if e := guestproto.NormalizeExec(req); e != nil {
		writeResult(conn, guestproto.Result{Error: e})
		return
	}
	info, err := os.Stat(req.Cwd)
	if err != nil || !info.IsDir() {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{
			Code:    guestproto.CodeInvalid,
			Message: fmt.Sprintf("cwd %q is not a directory", req.Cwd),
		}})
		return
	}

	cmd := exec.Command(req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = guestproto.ExecEnv(injectedSecrets(), req.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	if err := cmd.Start(); err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInvalid, Message: err.Error()}})
		return
	}

	s := &session{conn: conn}
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	timer := time.AfterFunc(time.Duration(req.TimeoutSeconds)*time.Second, kill)

	var wg sync.WaitGroup
	var sendErr error
	var sendMu sync.Mutex
	pump := func(typ byte, r io.Reader) {
		defer wg.Done()
		if err := s.stream(typ, r); err != nil {
			sendMu.Lock()
			sendErr = err
			sendMu.Unlock()
		}
	}
	wg.Add(2)
	go pump(guestproto.FrameStdout, stdout)
	go pump(guestproto.FrameStderr, stderr)
	wg.Wait()

	sendMu.Lock()
	broken := sendErr != nil
	sendMu.Unlock()
	if broken {
		kill()
	}

	waitErr := cmd.Wait()
	killed := !timer.Stop()

	res := guestproto.Result{}
	if waitErr == nil {
		res.ExitCode = 0
	} else {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			res.ExitCode = exit.ExitCode()
		} else {
			res.ExitCode = -1
		}
	}
	res.TimedOut = killed && res.ExitCode == -1
	writeResult(conn, res)
}
