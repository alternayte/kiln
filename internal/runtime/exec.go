package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/alternayte/kiln/internal/guestproto"
)

// Exec runs one command in the VM and buffers its output. One call opens one
// agent connection and one guest process group.
func (vm *VM) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	return vm.ExecStream(ctx, req, nil)
}

// ExecStream runs one command and calls onOutput for every stdout and stderr
// frame before the final result. With a callback the output is not buffered
// into the result; a nil callback buffers both streams.
func (vm *VM) ExecStream(ctx context.Context, req ExecRequest, onOutput func(typ byte, data []byte) error) (ExecResult, error) {
	creq := guestproto.Request{
		Op:             guestproto.OpExec,
		Cmd:            req.Cmd,
		Cwd:            req.Cwd,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
	}
	if e := guestproto.NormalizeExec(&creq); e != nil {
		return ExecResult{}, &ExecError{Code: e.Code, Message: e.Message}
	}
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return ExecResult{}, err
	}
	defer ac.Close()
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, creq); err != nil {
		return ExecResult{}, err
	}
	var stdout, stderr strings.Builder
	for {
		typ, payload, err := guestproto.ReadFrame(ac.br)
		if err != nil {
			return ExecResult{}, err
		}
		switch typ {
		case guestproto.FrameStdout, guestproto.FrameStderr:
			if onOutput == nil {
				if typ == guestproto.FrameStdout {
					stdout.Write(payload)
				} else {
					stderr.Write(payload)
				}
				continue
			}
			if err := onOutput(typ, payload); err != nil {
				return ExecResult{}, err
			}
		case guestproto.FrameResult:
			var res guestproto.Result
			if err := json.Unmarshal(payload, &res); err != nil {
				return ExecResult{}, fmt.Errorf("runtime: agent result: %w", err)
			}
			if res.Error != nil {
				return ExecResult{}, &ExecError{Code: res.Error.Code, Message: res.Error.Message}
			}
			return ExecResult{
				ExitCode: res.ExitCode,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				TimedOut: res.TimedOut,
			}, nil
		default:
			return ExecResult{}, fmt.Errorf("runtime: unexpected frame type %q", typ)
		}
	}
}

// OpenFile starts a guest file read and returns a reader for its contents. It
// fails before any data is read when the agent rejects the path or the file.
func (vm *VM) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return nil, err
	}
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, guestproto.Request{
		Op:   guestproto.OpReadFile,
		Path: path,
	}); err != nil {
		ac.Close()
		return nil, err
	}
	typ, payload, err := guestproto.ReadFrame(ac.br)
	if err != nil {
		ac.Close()
		return nil, err
	}
	if typ == guestproto.FrameResult {
		ac.Close()
		return nil, resultError(payload)
	}
	if typ != guestproto.FrameStdout {
		ac.Close()
		return nil, fmt.Errorf("runtime: unexpected frame type %q", typ)
	}
	return &fileReader{ac: ac, first: payload}, nil
}

type fileReader struct {
	ac    *agentConn
	first []byte
	done  bool
}

func (r *fileReader) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	if r.done {
		return 0, io.EOF
	}
	for {
		typ, payload, err := guestproto.ReadFrame(r.ac.br)
		if err != nil {
			r.done = true
			return 0, err
		}
		switch typ {
		case guestproto.FrameStdout:
			if len(payload) == 0 {
				continue
			}
			n := copy(p, payload)
			if n < len(payload) {
				r.first = payload[n:]
			}
			return n, nil
		case guestproto.FrameResult:
			r.done = true
			if err := resultError(payload); err != nil {
				return 0, err
			}
			return 0, io.EOF
		default:
			r.done = true
			return 0, fmt.Errorf("runtime: unexpected frame type %q", typ)
		}
	}
}

func (r *fileReader) Close() error { return r.ac.Close() }

// WriteFile writes r to a guest path. The agent creates parent directories
// and writes through a temporary file plus a rename.
func (vm *VM) WriteFile(ctx context.Context, path string, r io.Reader) error {
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return err
	}
	defer ac.Close()
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, guestproto.Request{
		Op:   guestproto.OpWriteFile,
		Path: path,
	}); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if err := guestproto.WriteFrame(ac.conn, guestproto.FrameData, buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := guestproto.WriteFrame(ac.conn, guestproto.FrameData, nil); err != nil {
		return err
	}
	typ, payload, err := guestproto.ReadFrame(ac.br)
	if err != nil {
		return err
	}
	if typ != guestproto.FrameResult {
		return fmt.Errorf("runtime: unexpected frame type %q", typ)
	}
	return resultError(payload)
}

// Resume sends the resume hook data and waits for the agent's answer.
func (vm *VM) ResumeHooks(ctx context.Context, entropy []byte, unixNanos int64, hostname string, secrets map[string]string) error {
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return err
	}
	defer ac.Close()
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, guestproto.Request{
		Op:        guestproto.OpResume,
		Entropy:   entropy,
		UnixNanos: unixNanos,
		Hostname:  hostname,
		Secrets:   secrets,
	}); err != nil {
		return err
	}
	typ, payload, err := guestproto.ReadFrame(ac.br)
	if err != nil {
		return err
	}
	if typ != guestproto.FrameResult {
		return fmt.Errorf("runtime: unexpected frame type %q", typ)
	}
	return resultError(payload)
}

// resultError decodes one result frame and returns its error, if any.
func resultError(payload []byte) error {
	var res guestproto.Result
	if err := json.Unmarshal(payload, &res); err != nil {
		return fmt.Errorf("runtime: agent result: %w", err)
	}
	if res.Error != nil {
		return &ExecError{Code: res.Error.Code, Message: res.Error.Message}
	}
	return nil
}

// IsExecError reports whether err is a stable agent error with the given code.
func IsExecError(err error, code string) bool {
	var ee *ExecError
	return errors.As(err, &ee) && ee.Code == code
}
