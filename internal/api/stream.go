package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/coder/websocket"

	"github.com/alternayte/kiln/internal/sandbox"
)

// streamReadLimit bounds one client message. A TLS record is at most 16 KiB
// plus overhead, so a client never needs more than this.
const streamReadLimit = 1 << 20

// streamSandbox carries one raw TCP connection to a guest port over a
// WebSocket. A binary message carries bytes in each direction. The client
// runs its own TLS inside, so the guest's certificate reaches it.
func (s *Server) streamSandbox(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, "port must be a number from 1 to 65535")
		return
	}
	// The guest is dialled before the upgrade, so a missing sandbox, a refused
	// tenant or a port with no listener answers with an HTTP error.
	st, err := s.Sandboxes.OpenStream(r.Context(), r.PathValue("id"), port)
	if err != nil {
		if errors.Is(err, sandbox.ErrNoListener) {
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
			return
		}
		s.writeErr(w, err)
		return
	}
	defer st.Close()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(streamReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The guest's bytes go to the client until either end closes.
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := st.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				switch {
				case st.Stopped():
					_ = conn.Close(websocket.StatusGoingAway, "the sandbox stopped; reconnect to wake it")
				case errors.Is(err, io.EOF):
					_ = conn.Close(websocket.StatusNormalClosure, "the guest closed the connection")
				default:
					_ = conn.Close(websocket.StatusInternalError, closeReason(err))
				}
				return
			}
		}
	}()

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			_ = conn.Close(websocket.StatusUnsupportedData, "a stream carries binary messages only")
			return
		}
		if _, err := st.Write(data); err != nil {
			if st.Stopped() {
				_ = conn.Close(websocket.StatusGoingAway, "the sandbox stopped; reconnect to wake it")
			}
			return
		}
	}
}
