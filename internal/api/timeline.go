package api

import (
	"net/http"
	"time"

	"github.com/alternayte/kiln/internal/store"
)

// pointJSON is one timeline point.
type pointJSON struct {
	ID            string    `json:"id"`
	Label         string    `json:"label,omitempty"`
	Kind          string    `json:"kind"`
	Bytes         int64     `json:"bytes"`
	Merged        int       `json:"merged,omitempty"`
	SecretBearing bool      `json:"secret_bearing,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func timelinePointJSON(p store.TimelinePoint) pointJSON {
	return pointJSON{
		ID: p.ID, Label: p.Label, Kind: p.Kind, Bytes: p.SizeBytes, Merged: p.Merged,
		SecretBearing: p.SecretBearing, CreatedAt: p.CreatedAt,
	}
}

// markPoint marks a timeline point on one sandbox.
func (s *Server) markPoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
			return
		}
	}
	if err := s.admit(r.Context(), wantSnapshot); err != nil {
		s.writeErr(w, err)
		return
	}
	point, err := s.Sandboxes.MarkPoint(r.Context(), r.PathValue("id"), req.Label)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, timelinePointJSON(point))
}

// listPoints lists the timeline points of one sandbox, oldest first.
func (s *Server) listPoints(w http.ResponseWriter, r *http.Request) {
	points, err := s.Sandboxes.Points(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := make([]pointJSON, 0, len(points))
	for _, p := range points {
		out = append(out, timelinePointJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": out})
}

// deletePoint removes one timeline point.
func (s *Server) deletePoint(w http.ResponseWriter, r *http.Request) {
	if err := s.Sandboxes.DeletePoint(r.Context(), r.PathValue("id"), r.PathValue("point")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rewindPoint restores one timeline point under the same sandbox id.
func (s *Server) rewindPoint(w http.ResponseWriter, r *http.Request) {
	row, err := s.Sandboxes.Rewind(r.Context(), r.PathValue("id"), r.PathValue("point"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out, err := s.sandboxJSON(r.Context(), row)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
