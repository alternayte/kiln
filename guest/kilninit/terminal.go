//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"github.com/alternayte/kiln/internal/guestproto"
	"golang.org/x/sys/unix"
)

// runTerminal opens a pty, runs a shell as the session leader on its slave,
// and copies bytes both ways until either side closes. The connection stays
// open for the life of the shell, unlike an exec, which ends when the command
// does.
func runTerminal(conn net.Conn, req *guestproto.Request) {
	size := guestproto.Winsize{Cols: req.Cols, Rows: req.Rows}
	if !size.Valid() {
		size = guestproto.Winsize{Cols: 80, Rows: 24}
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "/"
	}

	ptm, name, err := openpty()
	if err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	defer ptm.Close()
	pts, err := os.OpenFile(name, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	setWinsize(ptm, size)

	cmd := exec.Command(terminalShell(), "-l")
	cmd.Dir = cwd
	// TERM makes a curses program usable. Without it the shell assumes dumb.
	cmd.Env = append(guestproto.ExecEnv(injectedSecrets(), req.Env), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	// Setsid plus Setctty makes the shell the session leader of the pty, so
	// job control, Ctrl-C and a resize signal reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		pts.Close()
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInternal, Message: err.Error()}})
		return
	}
	// The child holds the slave now. The parent's copy would keep the master
	// readable forever after the shell exits.
	pts.Close()

	s := &session{conn: conn}
	// The shell's output is the only thing the host reads, so one pump is
	// enough; stderr shares the pty.
	done := make(chan struct{})
	go func() {
		_ = s.stream(guestproto.FrameStdout, ptm)
		close(done)
	}()

	readErr := pumpTerminalInput(conn, ptm)
	// A closed connection leaves an orphan shell holding the pty, so the
	// shell dies with the connection.
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	// The shell has gone, so the master reaches EOF and the pump returns.
	ptm.Close()
	<-done

	res := guestproto.Result{}
	var exit *exec.ExitError
	switch {
	case waitErr == nil:
		res.ExitCode = 0
	case errors.As(waitErr, &exit):
		res.ExitCode = exit.ExitCode()
	default:
		res.ExitCode = -1
	}
	writeResult(conn, res)
}

// terminalShell picks the best shell the image carries. bash gives tab
// completion, history and arrow keys; dash, which /bin/sh usually is, gives
// none of them and feels broken to a person typing into it.
func terminalShell() string {
	for _, shell := range []string{"/bin/bash", "/usr/bin/bash", "/bin/zsh"} {
		if info, err := os.Stat(shell); err == nil && !info.IsDir() {
			return shell
		}
	}
	return guestproto.TerminalShell
}

// pumpTerminalInput copies keystrokes and window sizes from the host onto the
// pty until the host closes the connection.
func pumpTerminalInput(conn net.Conn, ptm *os.File) error {
	for {
		typ, payload, err := guestproto.ReadFrame(conn)
		if err != nil {
			return err
		}
		switch typ {
		case guestproto.FrameStdin:
			if _, err := ptm.Write(payload); err != nil {
				return err
			}
		case guestproto.FrameResize:
			var size guestproto.Winsize
			if json.Unmarshal(payload, &size) == nil && size.Valid() {
				setWinsize(ptm, size)
			}
		default:
			// An unknown frame is a host newer than this guest. Ignoring it
			// keeps an old sandbox usable after a host upgrade.
		}
	}
}

// openpty allocates a pty and returns the master and the slave path.
func openpty() (*os.File, string, error) {
	ptm, err := os.OpenFile("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	// Unlock the slave and read its number. Without the unlock the open of
	// the slave fails with EIO.
	var unlock int32
	if err := unix.IoctlSetPointerInt(int(ptm.Fd()), unix.TIOCSPTLCK, int(unlock)); err != nil {
		ptm.Close()
		return nil, "", err
	}
	n, err := unix.IoctlGetInt(int(ptm.Fd()), unix.TIOCGPTN)
	if err != nil {
		ptm.Close()
		return nil, "", err
	}
	return ptm, "/dev/pts/" + itoa(n), nil
}

// setWinsize tells the pty how large the client's window is, which makes the
// kernel send SIGWINCH to the shell.
func setWinsize(ptm *os.File, size guestproto.Winsize) {
	ws := unix.Winsize{Col: uint16(size.Cols), Row: uint16(size.Rows)}
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, ptm.Fd(), uintptr(unix.TIOCSWINSZ), uintptr(unsafe.Pointer(&ws)))
}

// itoa avoids strconv in PID 1 for one small number.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
