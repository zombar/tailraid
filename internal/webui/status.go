package webui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
)

// statusTTL bounds how often the admin report (a walk of every chunk) is rebuilt; every
// tab polling at 5 s shares one build (docs/13 §5, I-73).
const statusTTL = 2 * time.Second

type statusCache struct {
	mu   sync.Mutex
	at   time.Time
	rep  any
	body []byte
}

// report returns the cached status report, rebuilding it after statusTTL; nil without a
// status closure.
func (s *Server) report() any {
	if s.o.Status == nil {
		return nil
	}
	s.cache.mu.Lock() // held while building: concurrent misses wait for one build
	defer s.cache.mu.Unlock()
	now := s.o.Clk.Now()
	if s.cache.body == nil || now.Sub(s.cache.at) >= statusTTL {
		rep := s.o.Status()
		if r, ok := rep.(admin.Report); ok { // the join token is for the CLI on a member, never for a browser (I-127)
			r.JoinToken = ""
			rep = r
		}
		b, err := json.Marshal(rep)
		if err != nil {
			return nil
		}
		s.cache.rep, s.cache.body, s.cache.at = rep, b, now
	}
	return s.cache.rep
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	if s.o.Status == nil {
		writeErr(w, http.StatusServiceUnavailable, "status is not wired on this node")
		return
	}
	if s.report() == nil {
		writeErr(w, http.StatusInternalServerError, "status could not be encoded")
		return
	}
	s.cache.mu.Lock()
	body := s.cache.body
	s.cache.mu.Unlock()
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

const (
	chunksDefault = 64
	chunksMax     = 256
)

// chunks pages a file's chunk reports: each report costs one HEAD per live slot.
func (s *Server) chunks(w http.ResponseWriter, r *http.Request) {
	if s.o.Chunks == nil {
		writeErr(w, http.StatusServiceUnavailable, "chunk reports are not wired on this node")
		return
	}
	q := r.URL.Query()
	_, canonical := cleanPath(q.Get("path"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = chunksDefault
	}
	limit = min(limit, chunksMax)
	total, reps, err := s.o.Chunks(r.Context(), canonical, max(offset, 0), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	for i := range reps {
		reps[i].Events = nil // the event ring is the CLI's; the browser shows placement
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "chunks": reps})
}
