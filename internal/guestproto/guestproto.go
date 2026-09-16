// Package guestproto is the wire protocol between the host and the kilninit
// agent over one vsock connection.
//
// The host writes one JSON request frame. The agent replies with zero or more
// output frames and exactly one result frame, then closes the connection.
package guestproto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Port is the guest vsock port the agent listens on.
const Port uint32 = 52

// DefaultPath is the base PATH every exec gets, and the PATH PID 1 needs so
// exec.LookPath can resolve a command by name.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// MaxFrame is the largest frame either side accepts, in bytes.
const MaxFrame = 1 << 20

// Frame types.
const (
	FrameRequest byte = 'j' // JSON Request
	FrameStdout  byte = 'o'
	FrameStderr  byte = 'e'
	FrameData    byte = 'd' // raw write_file chunk, host to guest
	FrameResult  byte = 'r' // JSON Result
	FrameStdin   byte = 'i' // raw terminal keystrokes, host to guest
	FrameResize  byte = 'z' // JSON Winsize, host to guest
)

// Request operations.
const (
	OpHello     = "hello"
	OpExec      = "exec"
	OpShutdown  = "shutdown"
	OpResume    = "resume"
	OpReadFile  = "read_file"
	OpWriteFile = "write_file"
	OpTerminal  = "terminal"
	OpStart     = "start"
	OpAwait     = "await"
)

// StartDeadlineSeconds is how long the host waits for a started application
// to accept a connection on its port. A runtime that boots and binds fits; a
// build belongs in setup.
const StartDeadlineSeconds = 120

// Terminal bounds. A window outside these is a client bug, and a pty ioctl
// with a wild size confuses every curses program in the sandbox.
const (
	// TerminalShell is the last resort. Every image has it, and the agent
	// prefers a better one when the image carries it.
	TerminalShell   = "/bin/sh"
	TerminalMaxCols = 1000
	TerminalMaxRows = 1000
)

// Winsize is the terminal window the client shows. The host sends one on
// connect and one on every resize.
type Winsize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Valid reports whether a window size is usable.
func (w Winsize) Valid() bool {
	return w.Cols > 0 && w.Rows > 0 && w.Cols <= TerminalMaxCols && w.Rows <= TerminalMaxRows
}

// Exec request bounds.
const (
	ExecTimeoutDefault = 60
	ExecTimeoutMax     = 3600
)

// Error codes. The API layer reports these unchanged.
const (
	CodeInvalid  = "invalid"
	CodeNotFound = "not_found"
	CodeInternal = "internal"
)

// Request is one host request.
type Request struct {
	Op             string            `json:"op"`
	Cmd            []string          `json:"cmd,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	// Path is the guest path of a file request.
	Path string `json:"path,omitempty"`
	// Port is the port a start command listens on. The guest waits for it
	// before it answers.
	Port int `json:"port,omitempty"`
	// Cols and Rows are the terminal window at the moment the client opens it.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
	// Entropy, UnixNanos, Hostname and Secrets carry the resume hook data.
	Entropy   []byte            `json:"entropy,omitempty"`
	UnixNanos int64             `json:"unix_nanos,omitempty"`
	Hostname  string            `json:"hostname,omitempty"`
	Secrets   map[string]string `json:"secrets,omitempty"`
}

// Error is a stable error from the agent.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Result is the final frame of a connection.
type Result struct {
	OK       bool   `json:"ok,omitempty"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Error    *Error `json:"error,omitempty"`
}

// NormalizeExec applies the exec defaults and rejects values outside the
// bounds. The host calls it before sending; the agent calls it again.
func NormalizeExec(req *Request) *Error {
	if len(req.Cmd) == 0 {
		return &Error{Code: CodeInvalid, Message: "cmd is required"}
	}
	if req.Cwd == "" {
		req.Cwd = "/"
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = ExecTimeoutDefault
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > ExecTimeoutMax {
		return &Error{Code: CodeInvalid, Message: fmt.Sprintf("timeout_seconds must be between 1 and %d", ExecTimeoutMax)}
	}
	return nil
}

// ExecEnv returns the agent's fixed base environment with the sandbox's
// injected secrets and the request entries merged on top. Later layers win.
func ExecEnv(secrets, extra map[string]string) []string {
	base := map[string]string{
		"PATH": DefaultPath,
		"HOME": "/root",
	}
	for k, v := range secrets {
		base[k] = v
	}
	for k, v := range extra {
		base[k] = v
	}
	keys := make([]string, 0, len(base))
	for k := range base {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+base[k])
	}
	return env
}

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("guestproto: frame of %d bytes exceeds the limit", len(payload))
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("guestproto: frame of %d bytes exceeds the limit", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// WriteJSON marshals v and writes it as one frame.
func WriteJSON(w io.Writer, typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, typ, b)
}

// ReadJSON reads one frame and decodes it as JSON into v. It returns the frame
// type so the caller can reject a wrong frame.
func ReadJSON(r io.Reader, v any) (byte, error) {
	typ, payload, err := ReadFrame(r)
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return typ, err
	}
	return typ, nil
}
