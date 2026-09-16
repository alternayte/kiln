//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/alternayte/kiln/internal/guestproto"
)

// startOutput is how much of the application's output the guest keeps. It is
// what the host reports when the application exits, so it holds the end of a
// stack trace and not a log file.
const startOutput = 8 << 10

// runStart runs the application and answers as soon as it listens. The
// command keeps running after this connection closes, unlike an exec, which
// kills its process group when the call ends.
//
// The guest waits for the port itself, because it can see a listener on
// localhost that the host cannot reach until the sandbox is attached.
func runStart(conn net.Conn, req *guestproto.Request) {
	if len(req.Cmd) == 0 {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{
			Code: guestproto.CodeInvalid, Message: "cmd is required",
		}})
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{
			Code: guestproto.CodeInvalid, Message: "port is required",
		}})
		return
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "/"
	}

	cmd := exec.Command(req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = cwd
	cmd.Env = guestproto.ExecEnv(injectedSecrets(), req.Env)
	// Setsid detaches it from this connection, so the shutdown of one agent
	// request never reaches the application.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	tail := &ring{limit: startOutput}
	cmd.Stdout = tail
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{
			Code: guestproto.CodeInvalid, Message: err.Error(),
		}})
		return
	}
	started[req.Port] = &application{cmd: cmd, tail: tail}
	go reap(req.Port)

	deadline := time.Now().Add(time.Duration(req.TimeoutSeconds) * time.Second)
	address := fmt.Sprintf("127.0.0.1:%d", req.Port)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			c.Close()
			writeResult(conn, guestproto.Result{OK: true})
			return
		}
		// A command that exits before it listens never will.
		if done(req.Port) {
			writeResult(conn, guestproto.Result{Error: &guestproto.Error{
				Code: guestproto.CodeInvalid,
				Message: fmt.Sprintf("the start command exited before it listened on port %d: %s",
					req.Port, tail.String()),
			}})
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	writeResult(conn, guestproto.Result{Error: &guestproto.Error{
		Code: guestproto.CodeInvalid,
		Message: fmt.Sprintf("the start command did not listen on port %d in time: %s",
			req.Port, tail.String()),
	}})
}

// application is a start command this guest runs. The guest keeps it so a
// later request can report how it ended.
type application struct {
	cmd  *exec.Cmd
	tail *ring
	// exited is closed when the command ends. Exit and Output are read only
	// after it closes.
	exited   chan struct{}
	exitCode int
}

// started holds one application per port. A guest runs one start command, so
// the map never grows past that.
var started = map[int]*application{}

// reap waits for the application and remembers how it ended, so the host can
// ask later instead of holding a connection for the life of the sandbox.
func reap(port int) {
	app := started[port]
	if app == nil {
		return
	}
	app.exited = make(chan struct{})
	err := app.cmd.Wait()
	app.exitCode = 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			app.exitCode = exit.ExitCode()
		} else {
			app.exitCode = -1
		}
	}
	close(app.exited)
}

// done reports whether the application has ended.
func done(port int) bool {
	app := started[port]
	if app == nil || app.exited == nil {
		return false
	}
	select {
	case <-app.exited:
		return true
	default:
		return false
	}
}

// ring keeps the last bytes written to it. The application's output is a
// stream with no end, and only its tail explains an exit.
type ring struct {
	limit int
	buf   []byte
	mu    sync.Mutex
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.limit {
		r.buf = r.buf[len(r.buf)-r.limit:]
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// runAwait answers when the application ends, and says how. The host holds
// this connection for the life of the sandbox, so it learns of a crash
// without polling.
func runAwait(conn net.Conn, req *guestproto.Request) {
	app := started[req.Port]
	if app == nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{
			Code:    guestproto.CodeNotFound,
			Message: fmt.Sprintf("no start command runs on port %d", req.Port),
		}})
		return
	}
	// exited is made by the reaper, which starts with the application.
	for app.exited == nil {
		time.Sleep(50 * time.Millisecond)
	}
	<-app.exited
	writeResult(conn, guestproto.Result{
		ExitCode: app.exitCode,
		Error: &guestproto.Error{
			Code:    guestproto.CodeInternal,
			Message: app.tail.String(),
		},
	})
}
