package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/alternayte/kiln/internal/guestproto"
)

// Terminal is one interactive terminal in a running microVM. Read gives
// the shell's output, Write sends keystrokes, and Resize tells the guest how
// large the client's window is. Unlike an exec, the session lives until the
// client closes it or the shell exits.
type Terminal struct {
	ac   *agentConn
	mu   sync.Mutex // one writer at a time on the shared connection
	rest []byte
	done bool
}

// Terminal opens a terminal. It fails before any byte is exchanged when
// the agent refuses the request.
func (vm *VM) Terminal(ctx context.Context, size guestproto.Winsize, cwd string, env map[string]string) (*Terminal, error) {
	if !size.Valid() {
		size = guestproto.Winsize{Cols: 80, Rows: 24}
	}
	ac, err := vm.dialAgent(ctx)
	if err != nil {
		return nil, err
	}
	if err := guestproto.WriteJSON(ac.conn, guestproto.FrameRequest, guestproto.Request{
		Op:   guestproto.OpTerminal,
		Cwd:  cwd,
		Env:  env,
		Cols: size.Cols,
		Rows: size.Rows,
	}); err != nil {
		ac.Close()
		return nil, err
	}
	return &Terminal{ac: ac}, nil
}

// Read returns the next of the shell's output. It returns io.EOF when the
// shell exits.
func (t *Terminal) Read(p []byte) (int, error) {
	if len(t.rest) > 0 {
		n := copy(p, t.rest)
		t.rest = t.rest[n:]
		return n, nil
	}
	if t.done {
		return 0, io.EOF
	}
	for {
		typ, payload, err := guestproto.ReadFrame(t.ac.br)
		if err != nil {
			t.done = true
			return 0, err
		}
		switch typ {
		case guestproto.FrameStdout, guestproto.FrameStderr:
			if len(payload) == 0 {
				continue
			}
			n := copy(p, payload)
			if n < len(payload) {
				t.rest = payload[n:]
			}
			return n, nil
		case guestproto.FrameResult:
			t.done = true
			if err := resultError(payload); err != nil {
				return 0, err
			}
			return 0, io.EOF
		default:
			t.done = true
			return 0, fmt.Errorf("runtime: unexpected frame type %q", typ)
		}
	}
}

// Write sends keystrokes to the shell.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// One keystroke frame never fills the limit, but a paste can, so a large
	// write goes as several frames.
	for off := 0; off < len(p); off += guestproto.MaxFrame {
		end := min(off+guestproto.MaxFrame, len(p))
		if err := guestproto.WriteFrame(t.ac.conn, guestproto.FrameStdin, p[off:end]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// Resize tells the guest how large the client's window is, so the kernel
// signals the shell and a curses program redraws.
func (t *Terminal) Resize(size guestproto.Winsize) error {
	if !size.Valid() {
		return &ExecError{Code: guestproto.CodeInvalid, Message: "the window size is out of range"}
	}
	b, err := json.Marshal(size)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return guestproto.WriteFrame(t.ac.conn, guestproto.FrameResize, b)
}

// Close ends the session. The agent kills the shell when the connection goes.
func (t *Terminal) Close() error { return t.ac.Close() }
