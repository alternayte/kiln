//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/alternayte/kiln/internal/guestproto"
)

// processOutputDrain is how long the guest reads a process's pipes after the
// process exits. A daemon it left behind can hold them open forever, and the
// exit must still report.
const processOutputDrain = 10 * time.Second

// processSignals maps the names a caller may send to their numbers.
var processSignals = map[string]syscall.Signal{
	"TERM": syscall.SIGTERM,
	"KILL": syscall.SIGKILL,
	"INT":  syscall.SIGINT,
	"HUP":  syscall.SIGHUP,
	"QUIT": syscall.SIGQUIT,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
}

// outputBuffer keeps the end of one output stream and numbers its bytes from
// the start of the stream, so an offset stays valid after older bytes go.
type outputBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
	total int64
}

func (o *outputBuffer) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.total += int64(len(p))
	o.buf = append(o.buf, p...)
	if len(o.buf) > o.limit {
		o.buf = append(o.buf[:0], o.buf[len(o.buf)-o.limit:]...)
	}
	return len(p), nil
}

// read returns at most max bytes after an offset, the offset of the next read,
// the bytes the buffer no longer holds, and whether output remains. A read
// that ends inside a UTF-8 character stops before it, because the answer is
// text and the rest of the character may not exist yet. A read of a stream
// that has ended returns what is there.
func (o *outputBuffer) read(offset int64, max int, ended bool) (data []byte, next, lost int64, more bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	start := o.total - int64(len(o.buf))
	if offset > o.total {
		offset = o.total
	}
	if offset < start {
		lost = start - offset
		offset = start
	}
	rest := o.buf[offset-start:]
	data = rest
	if len(data) > max {
		data = data[:max]
		more = true
	}
	if !ended || more {
		data = withoutPartialRune(data)
	}
	return append([]byte(nil), data...), offset + int64(len(data)), lost, more
}

// withoutPartialRune drops a character cut at the end of data. Bytes that are
// not UTF-8 at all stay.
func withoutPartialRune(data []byte) []byte {
	for back := 1; back <= utf8.UTFMax-1 && back <= len(data); back++ {
		i := len(data) - back
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				return data[:i]
			}
			return data
		}
	}
	return data
}

// process is one detached command.
type process struct {
	id     string
	cmd    *exec.Cmd
	stdout *outputBuffer
	stderr *outputBuffer
	// exited closes when the command ends. exitCode is read only after it.
	exited   chan struct{}
	exitCode int
}

func (p *process) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// processTable holds every process this guest remembers, oldest first. It lives
// in guest memory, so it survives a snapshot and a sleep, and a fork holds its
// own copy.
var processTable = struct {
	sync.Mutex
	next int
	rows []*process
}{}

// makeRoomForProcess removes the oldest exited record when the table is full.
// The caller holds the table lock.
func makeRoomForProcess() error {
	if len(processTable.rows) < guestproto.ProcessRecordLimit {
		return nil
	}
	for i, p := range processTable.rows {
		if !p.running() {
			processTable.rows = append(processTable.rows[:i], processTable.rows[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%d processes are running, which is the limit", guestproto.ProcessRecordLimit)
}

func findProcess(id string) *process {
	processTable.Lock()
	defer processTable.Unlock()
	for _, p := range processTable.rows {
		if p.id == id {
			return p
		}
	}
	return nil
}

func processError(conn net.Conn, code, message string) {
	writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: code, Message: message}})
}

// runProcessStart starts one detached command and answers once it runs. It has
// its own session, so nothing on this connection reaches it, and no deadline,
// because the caller owns that.
func runProcessStart(conn net.Conn, req *guestproto.Request) {
	if len(req.Cmd) == 0 {
		processError(conn, guestproto.CodeInvalid, "cmd is required")
		return
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "/"
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		processError(conn, guestproto.CodeInvalid, fmt.Sprintf("cwd %q is not a directory", cwd))
		return
	}
	processTable.Lock()
	defer processTable.Unlock()
	if err := makeRoomForProcess(); err != nil {
		processError(conn, guestproto.CodeInvalid, err.Error())
		return
	}
	p := &process{
		stdout: &outputBuffer{limit: guestproto.ProcessOutputLimit},
		stderr: &outputBuffer{limit: guestproto.ProcessOutputLimit},
		exited: make(chan struct{}),
	}
	p.cmd = exec.Command(req.Cmd[0], req.Cmd[1:]...)
	p.cmd.Dir = cwd
	p.cmd.Env = processEnv(req.Env)
	p.cmd.SysProcAttr = withUser(&syscall.SysProcAttr{Setsid: true})
	p.cmd.Stdout = p.stdout
	p.cmd.Stderr = p.stderr
	p.cmd.WaitDelay = processOutputDrain
	if err := p.cmd.Start(); err != nil {
		processError(conn, guestproto.CodeInvalid, err.Error())
		return
	}
	processTable.next++
	p.id = strconv.Itoa(processTable.next)
	processTable.rows = append(processTable.rows, p)
	go func() {
		err := p.cmd.Wait()
		var exit *exec.ExitError
		switch {
		case err == nil, errors.Is(err, exec.ErrWaitDelay):
			p.exitCode = 0
		case errors.As(err, &exit):
			p.exitCode = exit.ExitCode()
		default:
			p.exitCode = -1
		}
		close(p.exited)
	}()
	writeResult(conn, guestproto.Result{Process: &guestproto.ProcessState{ID: p.id, Running: true}})
}

// runProcessRead sends the output after each offset, then the state. It reads
// the state first, so a process reported as ended has written everything the
// answer can hold.
func runProcessRead(conn net.Conn, req *guestproto.Request) {
	p := findProcess(req.Process)
	if p == nil {
		processError(conn, guestproto.CodeNotFound, "no such process")
		return
	}
	running := p.running()
	state := &guestproto.ProcessState{ID: p.id, Running: running}
	if !running {
		state.ExitCode = p.exitCode
	}
	stdout, next, lost, more := p.stdout.read(req.StdoutOffset, guestproto.ProcessReadLimit, !running)
	state.StdoutOffset, state.StdoutLost, state.More = next, lost, more
	stderr, next, lost, more := p.stderr.read(req.StderrOffset, guestproto.ProcessReadLimit, !running)
	state.StderrOffset, state.StderrLost, state.More = next, lost, state.More || more
	if err := writeOutput(conn, guestproto.FrameStdout, stdout); err != nil {
		return
	}
	if err := writeOutput(conn, guestproto.FrameStderr, stderr); err != nil {
		return
	}
	writeResult(conn, guestproto.Result{Process: state})
}

func writeOutput(conn net.Conn, typ byte, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), 32<<10)
		if err := guestproto.WriteFrame(conn, typ, data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// runProcessSignal signals the process group. A process that has ended takes no
// signal: its pid may belong to something else by now.
func runProcessSignal(conn net.Conn, req *guestproto.Request) {
	p := findProcess(req.Process)
	if p == nil {
		processError(conn, guestproto.CodeNotFound, "no such process")
		return
	}
	signal, ok := processSignals[strings.TrimPrefix(req.Signal, "SIG")]
	if !ok {
		processError(conn, guestproto.CodeInvalid, fmt.Sprintf("unknown signal %q", req.Signal))
		return
	}
	if p.running() {
		if err := syscall.Kill(-p.cmd.Process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			processError(conn, guestproto.CodeInternal, err.Error())
			return
		}
	}
	writeResult(conn, guestproto.Result{OK: true})
}

// runProcessCount answers how many detached processes run now.
func runProcessCount(conn net.Conn) {
	processTable.Lock()
	running := 0
	for _, p := range processTable.rows {
		if p.running() {
			running++
		}
	}
	processTable.Unlock()
	writeResult(conn, guestproto.Result{OK: true, Running: running})
}
