package webui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
)

// Searcher is the node's index (docs/18 §6, §6a): search.Service.
type Searcher interface {
	Search(ctx context.Context, q, under string, limit int) search.Result
	Media(ctx context.Context, q string, limit int) ([]search.Entry, error)
	Suggest(ctx context.Context, q string, limit int, allow func(*search.Entry) bool) ([]search.SuggestGroup, error)
}

// queryErr answers a malformed query: 400 with the offset of the fault (docs/18 §6a).
func queryErr(w http.ResponseWriter, err error) bool {
	var qe *search.QueryError
	if !errors.As(err, &qe) {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": qe.Error(), "at": qe.At})
	return true
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
	switch q.Get("scope") {
	case "", "files":
	case "media":
		s.mediaSearch(w, r, q.Get("q"), limit)
		return
	default:
		writeErr(w, http.StatusBadRequest, "search: scope "+strconv.Quote(q.Get("scope"))+": want files or media")
		return
	}
	res := s.o.Search.Search(r.Context(), q.Get("q"), q.Get("under"), limit)
	if res.Err != nil {
		queryErr(w, res.Err)
		return
	}
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

// MediaHits is one library's items in GET /api/search?scope=media (docs/13 §10d).
type MediaHits struct {
	Path  string      `json:"path"`
	Kind  string      `json:"kind"`
	Items []MediaItem `json:"items"`
}

// mediaSearch is GET /api/search?scope=media (docs/13 §10d, docs/18 §6a, I-412): the
// catalogue items the query matches, as the Media tab lists them, grouped by library in
// the libraries' order; an item the request may not read, under the reserved folder, in
// the trash or no longer under a library is left out.
func (s *Server) mediaSearch(w http.ResponseWriter, r *http.Request, q string, limit int) {
	es, err := s.o.Search.Media(r.Context(), q, limit)
	if err != nil {
		if !queryErr(w, err) {
			s.fail(w, err)
		}
		return
	}
	v := s.mediaView(r)
	byLib := map[meta.Ino][]MediaItem{}
	for i := range es {
		if it, ok := s.hitItem(r, v, &es[i]); ok {
			byLib[es[i].Lib] = append(byLib[es[i].Lib], it)
		}
	}
	libs := []MediaHits{}
	for _, l := range s.o.Store.Libraries() {
		if items := byLib[l.Ino]; len(items) > 0 {
			libs = append(libs, MediaHits{Path: l.Path, Kind: l.Kind.String(), Items: items})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "libs": libs})
}

// hitItem is a matched catalogue entry as the library lists it.
func (s *Server) hitItem(r *http.Request, v mediaView, e *search.Entry) (MediaItem, bool) {
	in, ok := s.o.Store.GetInode(e.Ino)
	if !ok || !s.readable(r, e.Ino) {
		return MediaItem{}, false
	}
	parts, err := s.o.Store.PathOf(e.Ino)
	if err != nil || len(parts) == 0 || parts[0] == meta.ReservedDir {
		return MediaItem{}, false
	}
	p := "/" + strings.Join(parts, "/")
	_, _, lib := s.effective(in)
	switch {
	case lib == meta.LibraryMovies && e.Kind == search.EntMovie:
		it := v.item(in, p, "movie")
		if in.Type == meta.TDir && it.Video == "" {
			if vid, ok := v.firstVideo(in.Ino); ok {
				it.Video = path.Join(p, vid.Name)
				v.video(&it)
			}
		}
		v.decorate(&it, nil)
		return it, true
	case lib == meta.LibraryTV && e.Kind == search.EntShow && in.Type == meta.TDir:
		return v.showItem(v.showGroup(in.Parent, &in), path.Dir(p)), true
	case lib == meta.LibraryMusic && e.Kind == search.EntAlbum:
		it := v.item(in, p, "album")
		v.decorate(&it, nil)
		return it, true
	case lib == meta.LibraryRoms && e.Kind == search.EntGame:
		return v.romItem(in, p)
	}
	return MediaItem{}, false
}

// suggest is GET /api/search/suggest (docs/13 §10d, docs/18 §6a, I-413): limit 8 by
// default, 400 outside 1..50; only items the request may read are counted.
func (s *Server) suggest(w http.ResponseWriter, r *http.Request) {
	if s.o.Search == nil {
		writeErr(w, http.StatusServiceUnavailable, "search: no index on this node")
		return
	}
	q := r.URL.Query()
	if sc := q.Get("scope"); sc != "media" {
		writeErr(w, http.StatusBadRequest, "suggest: scope "+strconv.Quote(sc)+": want media")
		return
	}
	limit := search.DefaultSuggest
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > search.MaxSuggest {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("suggest: limit %q: want 1..%d", v, search.MaxSuggest))
			return
		}
		limit = n
	}
	var allow func(*search.Entry) bool
	if _, ok := sessionIn(r); ok {
		allow = func(e *search.Entry) bool { return s.readable(r, e.Ino) }
	}
	groups, err := s.o.Search.Suggest(r.Context(), q.Get("q"), limit, allow)
	if err != nil {
		if !queryErr(w, err) {
			s.fail(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
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
