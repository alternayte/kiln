package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/store"
)

// healthResponse is the answer to GET /v1/health.
type healthResponse struct {
	OK          bool   `json:"ok"`
	KVM         bool   `json:"kvm"`
	Firecracker string `json:"firecracker"`
	Sandboxes   int    `json:"sandboxes"`
}

// eventView is one state transition on the events stream.
type eventView struct {
	ID        int64     `json:"id"`
	SandboxID string    `json:"sandbox_id,omitempty"`
	FromState string    `json:"from_state,omitempty"`
	ToState   string    `json:"to_state,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

func eventJSON(e store.Event) eventView {
	return eventView{
		ID:        e.ID,
		SandboxID: e.SandboxID,
		FromState: e.FromState,
		ToState:   e.ToState,
		Reason:    e.Reason,
		At:        e.At,
	}
}

// health reports the daemon's view of the host.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListSandboxes(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	live := 0
	for _, row := range rows {
		if row.DestroyedAt == nil {
			live++
		}
	}
	writeJSON(w, http.StatusOK, healthResponse{
		OK:          true,
		KVM:         runtime.KVMReady(),
		Firecracker: s.FirecrackerVersion,
		Sandboxes:   live,
	})
}

// events streams state transitions as server-sent events. ?sandbox_id=
// filters one sandbox; ?after= starts from a known event id, and the default
// starts at the latest event so a fresh reader is not replayed all history.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "streaming is not supported")
		return
	}
	filter := r.URL.Query().Get("sandbox_id")
	after := int64(-1)
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalid, "after must be an event id")
			return
		}
		after = n
	} else if last, err := s.Store.LastEventID(r.Context()); err == nil {
		after = last
	} else {
		after = 0
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		list, err := s.Store.ListEventsSince(r.Context(), after)
		if err != nil {
			return
		}
		for _, e := range list {
			after = e.ID
			if filter != "" && e.SandboxID != filter {
				continue
			}
			b, err := json.Marshal(eventJSON(e))
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: transition\ndata: %s\n\n", b); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}
