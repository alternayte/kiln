package api

import (
	"net/http"
	"time"

	"github.com/alternayte/kiln/internal/store"
)

// snapshotRequest is one POST /v1/sandboxes/{id}/snapshot body. Every field is
// optional.
type snapshotRequest struct {
	Stop bool `json:"stop"`
}

// snapshotCreatedResponse is the answer to POST /v1/sandboxes/{id}/snapshot.
type snapshotCreatedResponse struct {
	SnapshotID string `json:"snapshot_id"`
	SizeBytes  int64  `json:"size_bytes"`
}

// copyRequest is the body of fork and restore. Count is required.
type copyRequest struct {
	Count           int  `json:"count"`
	AllowSecretFork bool `json:"allow_secret_fork"`
}

// sandboxRef names one copy without repeating the whole sandbox.
type sandboxRef struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// copyResponse is the answer to fork and restore.
type copyResponse struct {
	Sandboxes []sandboxRef `json:"sandboxes"`
}

// snapshotView is one row of GET /v1/snapshots.
type snapshotView struct {
	ID            string    `json:"id"`
	Template      string    `json:"template"`
	ParentID      string    `json:"parent_id,omitempty"`
	SizeBytes     int64     `json:"size_bytes"`
	SecretBearing bool      `json:"secret_bearing"`
	CreatedAt     time.Time `json:"created_at"`
}

func snapshotJSON(snap store.Snapshot) snapshotView {
	return snapshotView{
		ID:            snap.ID,
		Template:      snap.TemplateName,
		ParentID:      snap.ParentID,
		SizeBytes:     snap.SizeBytes,
		SecretBearing: snap.SecretBearing,
		CreatedAt:     snap.CreatedAt,
	}
}

func copyJSON(rows []store.Sandbox) copyResponse {
	out := copyResponse{Sandboxes: make([]sandboxRef, 0, len(rows))}
	for _, row := range rows {
		out.Sandboxes = append(out.Sandboxes, sandboxRef{ID: row.ID, State: row.State})
	}
	return out
}

// createSnapshot snapshots one sandbox into a listed snapshot. With stop set
// the sandbox sleeps afterwards.
func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req snapshotRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if err := s.admit(r.Context(), wantSnapshot); err != nil {
		s.writeErr(w, err)
		return
	}
	snap, err := s.Sandboxes.Snapshot(r.Context(), r.PathValue("id"), req.Stop)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshotCreatedResponse{
		SnapshotID: snap.ID,
		SizeBytes:  snap.SizeBytes,
	})
}

// forkSandbox snapshots a running sandbox and restores count copies.
func (s *Server) forkSandbox(w http.ResponseWriter, r *http.Request) {
	var req copyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if err := s.admitCount(r.Context(), req.Count); err != nil {
		s.writeErr(w, err)
		return
	}
	copies, err := s.Sandboxes.Fork(r.Context(), r.PathValue("id"), req.Count, req.AllowSecretFork)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, copyJSON(copies))
}

// listSnapshots returns every listed snapshot.
func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Sandboxes.Snapshots(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]snapshotView, 0, len(rows))
	for _, snap := range rows {
		out = append(out, snapshotJSON(snap))
	}
	writeJSON(w, http.StatusOK, out)
}

// getSnapshot returns one listed snapshot.
func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Sandboxes.SnapshotInfo(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshotJSON(snap))
}

// deleteSnapshot removes one listing once no live sandbox reads it.
func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.Sandboxes.DeleteSnapshot(r.Context(), r.PathValue("id")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restoreSnapshot copies a listed snapshot into count new sandboxes.
func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	var req copyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}
	if err := s.admitCount(r.Context(), req.Count); err != nil {
		s.writeErr(w, err)
		return
	}
	copies, err := s.Sandboxes.RestoreSnapshot(r.Context(), r.PathValue("id"), req.Count, req.AllowSecretFork)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, copyJSON(copies))
}
