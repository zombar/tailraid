package webui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
)

// Searcher is the node's index (docs/18 §6): search.Service.
type Searcher interface {
	Search(ctx context.Context, q, under string, limit int) search.Result
}

// TextMax is the largest file the file view previews as text and /api/render renders
// (docs/13 §3, the page's TEXT_MAX).
const TextMax = 1 << 20

// search is GET /api/search (docs/13 §3, I-208): limit 50 by default, 400 outside 1..200.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if s.o.Search == nil {
		writeErr(w, http.StatusServiceUnavailable, "search: no index on this node")
		return
	}
	q := r.URL.Query()
	limit := search.DefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > search.MaxLimit {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("search: limit %q: want 1..%d", v, search.MaxLimit))
			return
		}
		limit = n
	}
	res := s.o.Search.Search(r.Context(), q.Get("q"), q.Get("under"), limit)
	if _, ok := sessionIn(r); ok { // a hit is one the user holds Read on (docs/21 §3, I-355)
		hits := res.Hits[:0]
		for i := range res.Hits {
			if s.readable(r, res.Hits[i].Ino) {
				hits = append(hits, res.Hits[i])
			}
		}
		res.Hits = hits
	}
	writeJSON(w, http.StatusOK, res)
}

// render is GET /api/render (docs/13 §4, I-209): a .md or .markdown file of at most
// TextMax bytes as an HTML fragment, by the escape-first renderer.
func (s *Server) render(w http.ResponseWriter, r *http.Request) {
	t, err := s.resolveTarget(r, r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if e := strings.ToLower(path.Ext(t.in.Name)); t.in.Type != meta.TFile || (e != ".md" && e != ".markdown") {
		writeErr(w, http.StatusBadRequest, "render "+t.canonical+": not a markdown file")
		return
	}
	if t.in.Size > TextMax {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("render %s: %d bytes, more than %d", t.canonical, t.in.Size, TextMax))
		return
	}
	tag := fmt.Sprintf(`"%d.%d.%d.md"`, t.in.Ino, t.in.Gen, t.in.Version)
	if t.snap != nil {
		tag = fmt.Sprintf(`"s%d.%d.%d.md"`, t.snap.Snapshot.ID, t.in.Ino, t.in.Gen)
	}
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", tag)
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f, err := s.open(t)
	if err != nil {
		s.fail(w, err)
		return
	}
	src, err := io.ReadAll(io.LimitReader(f, TextMax+1))
	_ = f.Close()
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(src) > TextMax { // grew since the stat
		writeErr(w, http.StatusRequestEntityTooLarge, "render "+t.canonical+": more than 1 MiB")
		return
	}
	dir := t.parts[:max(len(t.parts)-1, 0)]
	root := meta.Ino(0)
	if t.trashed {
		root = t.root.Ino
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, renderMarkdown(src, dir, root))
}
