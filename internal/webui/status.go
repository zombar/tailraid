package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// statusTTL bounds how often the admin report (a walk of every chunk) is rebuilt; every
// tab polling at 5 s shares one build (docs/13 §7, I-73).
const statusTTL = 2 * time.Second

// RestoreShown is how long the dashboard shows a done or cancelled restore job after it
// ends; a failed one stays while its host lists it (docs/16 §6, I-390).
const RestoreShown = time.Hour

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
			r.Restores = shownRestores(r.Restores, now)
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
	if in, err := s.o.Store.Resolve(meta.SplitPath(canonical)); err == nil && !s.readable(r, in.Ino) {
		s.fail(w, fmt.Errorf("chunks %s: %w", canonical, syscall.EACCES))
		return
	}
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

// shownRestores is the jobs the dashboard draws: every unfinished or failed one, and a
// done or cancelled one within RestoreShown of its end (I-390). jobs is not changed.
func shownRestores(jobs []rpc.RestoreJob, now time.Time) []rpc.RestoreJob {
	var out []rpc.RestoreJob
	for i := range jobs {
		j := &jobs[i]
		if (j.State == rpc.RestoreDone || j.State == rpc.RestoreCancelled) && now.Sub(time.Unix(0, j.Ended)) > RestoreShown {
			continue
		}
		out = append(out, *j)
	}
	return out
}
