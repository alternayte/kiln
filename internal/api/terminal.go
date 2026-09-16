package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/alternayte/kiln/internal/guestproto"
	"github.com/alternayte/kiln/internal/runtime"
)

// terminalReadLimit bounds one client frame. Keystrokes and a paste fit; a
// client that sends more is a client trying to exhaust the host.
const terminalReadLimit = 1 << 20

// terminalSandbox opens an interactive shell in a sandbox over a WebSocket.
// A binary message carries keystrokes to the guest and the shell's output
// back. A text message carries {"cols":n,"rows":n}.
func (s *Server) terminalSandbox(w http.ResponseWriter, r *http.Request) {
	size := guestproto.Winsize{
		Cols: atoiDefault(r.URL.Query().Get("cols"), 80),
		Rows: atoiDefault(r.URL.Query().Get("rows"), 24),
	}
	// The sandbox is opened before the upgrade, so a missing sandbox or a
	// refused tenant answers with a normal HTTP error the client can read.
	term, err := s.Sandboxes.Terminal(r.Context(), r.PathValue("id"), size)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	defer term.Close()

	// The gateway is the only browser-facing origin, and it forwards this
	// route, so the host accepts the origin the gateway presents.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(terminalReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The guest's output goes to the client until the shell exits.
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					// The shell exited. Tell the client why, rather than
					// dropping the socket and leaving it to guess.
					_ = conn.Close(websocket.StatusNormalClosure, "the shell exited")
					return
				}
				// Any other failure closes with its reason too. A socket
				// that dies with no close frame leaves the client retrying
				// a thing that will never work.
				_ = conn.Close(websocket.StatusInternalError, closeReason(err))
				return
			}
		}
	}()

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := term.Write(data); err != nil {
				return
			}
		case websocket.MessageText:
			var next guestproto.Winsize
			if json.Unmarshal(data, &next) == nil {
				_ = term.Resize(next)
			}
		}
	}
}

// closeReason fits a failure into the 123 bytes a close frame allows, and
// names the one failure a person can act on: a sandbox whose guest agent
// predates the terminal, which no amount of retrying will fix.
func closeReason(err error) string {
	if runtime.IsExecError(err, guestproto.CodeInvalid) &&
		strings.Contains(err.Error(), guestproto.OpTerminal) {
		return "this sandbox runs a guest agent with no terminal; rebuild its template"
	}
	reason := err.Error()
	if len(reason) > 123 {
		reason = reason[:123]
	}
	return reason
}

func atoiDefault(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}
