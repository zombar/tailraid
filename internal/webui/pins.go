package webui

import (
	"net/http"

	"github.com/zombar/tailraid/internal/pins"
)

// Pinner is what the UI needs from pins.Pins (docs/13 §4, I-162).
type Pinner interface {
	Pin(path string) (pins.Entry, error)
	Unpin(path string) error
	List() pins.Report
}

// pin keeps a folder on this node, or releases it.
func (s *Server) pin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		Pinned bool   `json:"pinned"`
	}
	if !decode(w, r, singleBodyCap, &req) {
		return
	}
	if s.o.Pins == nil {
		writeErr(w, http.StatusServiceUnavailable, "pins are not wired on this node")
		return
	}
	parts, canonical := cleanPath(req.Path)
	if reserved(parts) {
		writeErr(w, http.StatusBadRequest, "pin "+canonical+": not a user folder")
		return
	}
	if !req.Pinned {
		if err := s.o.Pins.Unpin(canonical); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": canonical, "pinned": false, "bytes": 0})
		return
	}
	e, err := s.o.Pins.Pin(canonical)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": e.Path, "pinned": true, "bytes": e.Bytes})
}

// pinList is the cache block of the status report, on its own.
func (s *Server) pinList(w http.ResponseWriter, _ *http.Request) {
	if s.o.Pins == nil {
		writeErr(w, http.StatusServiceUnavailable, "pins are not wired on this node")
		return
	}
	writeJSON(w, http.StatusOK, s.o.Pins.List())
}
