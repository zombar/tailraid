package webui

import (
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/search"
)

// Watching (docs/13 §10c): a viewer's playback rows, read and reported through the web UI.

const (
	// PlaybackEvery is the least time between two positions the node proposes for one
	// viewer and file; a report sooner that is not final is dropped (I-408).
	PlaybackEvery = 10 * time.Second
	// ContinueMax bounds the Continue watching row (I-409).
	ContinueMax = 20
	// ResumeMin is the least position the page offers to resume from and the row lists.
	ResumeMin = 60 * 1000 // ms
	// LongTrack is the length past which a track keeps a position like a video.
	LongTrack   = 20 * 60 * 1000 // ms
	playLastMax = 4096           // the coalescer's remembered pairs before it forgets the old ones
)

// Progress is a viewer's row of a file as the page reads it.
type Progress struct {
	Pos     uint64    `json:"pos"` // ms
	Dur     uint64    `json:"dur"` // ms
	Watched bool      `json:"watched"`
	Plays   uint32    `json:"plays"`
	Hidden  bool      `json:"hidden,omitempty"`
	At      time.Time `json:"at"`
	On      string    `json:"on,omitempty"`
}

func progressOf(r meta.PlayRow) *Progress {
	return &Progress{Pos: r.Pos, Dur: r.Dur, Watched: r.Watched, Plays: r.Plays, Hidden: r.Hidden, At: time.Unix(0, r.At).UTC(), On: r.On}
}

// TrackPlay is an album track's plays and, for a long one, its position.
type TrackPlay struct {
	Name     string    `json:"name"`
	Plays    uint32    `json:"plays"`
	Progress *Progress `json:"progress,omitempty"`
}

// viewerOf is the request's viewer (I-407): the signed-in user once the cluster has
// users, else the Tailscale login behind the request, else none.
func (s *Server) viewerOf(r *http.Request) string {
	if se, ok := sessionIn(r); ok {
		return meta.ViewerUserPrefix + strconv.Itoa(int(se.user.Idx))
	}
	src, ok := rpc.SourceAddr(r)
	if !ok {
		return ""
	}
	_, login, err := s.o.Auth.User(r.Context(), src)
	if err != nil || login == "" || len(meta.ViewerLoginPrefix+login) > meta.ViewerMax {
		return ""
	}
	return meta.ViewerLoginPrefix + login
}

// playCoalesce remembers when the node last proposed a position for each pair (I-408).
type playCoalesce struct {
	mu   sync.Mutex
	last map[playKey]time.Time
}

type playKey struct {
	viewer string
	ino    meta.Ino
}

// due reports whether a position for k may be proposed now, and if so records it.
func (c *playCoalesce) due(k playKey, now time.Time, final bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[playKey]time.Time{}
	}
	if t, ok := c.last[k]; ok && !final && now.Sub(t) < PlaybackEvery {
		return false
	}
	if len(c.last) >= playLastMax {
		for o, t := range c.last {
			if now.Sub(t) >= PlaybackEvery {
				delete(c.last, o)
			}
		}
	}
	c.last[k] = now
	return true
}

// playbackGet is GET /api/playback?path= (docs/13 §3).
func (s *Server) playbackGet(w http.ResponseWriter, r *http.Request) {
	viewer := s.viewerOf(r)
	if viewer == "" {
		writeErr(w, http.StatusNotFound, "playback: no viewer for this request")
		return
	}
	in, err := s.playFile(r, r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	row, ok := s.o.Store.PlaybackRow(viewer, in.Ino)
	if !ok {
		writeErr(w, http.StatusNotFound, "playback: no row")
		return
	}
	writeJSON(w, http.StatusOK, progressOf(row))
}

// playFile resolves a live file the request may read.
func (s *Server) playFile(r *http.Request, raw string) (meta.Inode, error) {
	parts, canonical := cleanPath(raw)
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		return in, err
	}
	if in.Type != meta.TFile || (len(parts) > 0 && parts[0] == meta.ReservedDir) {
		return in, fmt.Errorf("playback %s: not a file: %w", canonical, meta.ErrNotExist)
	}
	if !s.readable(r, in.Ino) {
		return in, fmt.Errorf("playback %s: %w", canonical, syscall.EACCES)
	}
	return in, nil
}

type playReq struct {
	Path   string   `json:"path"`
	Paths  []string `json:"paths"`
	Pos    uint64   `json:"pos"`
	Dur    uint64   `json:"dur"`
	Final  bool     `json:"final"`
	Played bool     `json:"played"`
	Action string   `json:"action"`
}

// playbackPost is POST /api/playback (docs/13 §3, §10c, I-407, I-408).
func (s *Server) playbackPost(w http.ResponseWriter, r *http.Request) {
	var q playReq
	if !decode(w, r, batchBodyCap, &q) {
		return
	}
	viewer := s.viewerOf(r)
	if viewer == "" {
		writeErr(w, http.StatusForbidden, "playback: no viewer for this request (a signed-in user or a Tailscale login)")
		return
	}
	p := meta.Playback{Viewer: viewer, On: s.o.NodeName}
	switch q.Action {
	case "watched", "unwatched":
		if len(q.Paths) == 0 || len(q.Paths) > MaxBatch {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("playback: %d paths: want 1..%d", len(q.Paths), MaxBatch))
			return
		}
		p.Action = meta.PlayWatched
		if q.Action == "unwatched" {
			p.Action = meta.PlayUnwatched
		}
		failed := []Failure{}
		for _, raw := range q.Paths {
			in, err := s.playFile(r, raw)
			if err != nil {
				failed = append(failed, Failure{Path: raw, Error: err.Error(), Status: errStatus(err)})
				continue
			}
			p.Inos = append(p.Inos, in.Ino)
		}
		if len(p.Inos) > 0 {
			if err := s.fs(r).Playback(r.Context(), p); err != nil {
				s.fail(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"failed": failed})
		return
	case "hide":
		p.Action = meta.PlayHide
	case "":
		p.Action = meta.PlayPosition
		if q.Played {
			p.Action = meta.PlayPlayed
		} else if q.Dur == 0 {
			writeErr(w, http.StatusBadRequest, "playback: a position needs dur")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "playback: action "+strconv.Quote(q.Action)+": want watched, unwatched or hide")
		return
	}
	in, err := s.playFile(r, q.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	p.Inos, p.Pos, p.Dur = []meta.Ino{in.Ino}, q.Pos, q.Dur
	if err := s.fs(r).PlaybackReady(); err != nil {
		s.fail(w, err) // refused before the window is asked: a dropped report still says so (I-408)
		return
	}
	if p.Action == meta.PlayPosition && !s.play.due(playKey{viewer, in.Ino}, s.o.Clk.Now(), q.Final) {
		w.WriteHeader(http.StatusNoContent) // within the window: the next report carries it (I-408)
		return
	}
	if err := s.fs(r).Playback(r.Context(), p); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- the viewer's state on media items (I-410)

// playState is one request's view of its viewer's rows.
type playState struct {
	viewer string
	rows   map[meta.Ino]meta.PlayRow
	under  map[meta.Ino]bool // every folder above a file with a row
}

func (s *Server) playState(r *http.Request) playState {
	ps := playState{viewer: s.viewerOf(r)}
	if ps.viewer == "" {
		return ps
	}
	ps.rows = s.o.Store.PlaybackRows(ps.viewer)
	ps.under = map[meta.Ino]bool{}
	for ino := range ps.rows {
		for cur := ino; ; {
			in, ok := s.o.Store.GetInodeAny(cur)
			if !ok || in.Parent == 0 || in.Ino == meta.RootIno || ps.under[in.Parent] {
				break
			}
			ps.under[in.Parent] = true
			cur = in.Parent
		}
	}
	return ps
}

func (ps playState) row(ino meta.Ino) (meta.PlayRow, bool) {
	r, ok := ps.rows[ino]
	return r, ok
}

// decorate fills an item's viewer state: a film's and an episode's progress, a show's
// watched and next (group are its folders), an album's plays.
func (v mediaView) decorate(it *MediaItem, group []showDir) {
	ps := v.play
	if len(ps.rows) == 0 {
		return
	}
	switch it.Kind {
	case "movie", "episode":
		ino := it.VideoIno
		if ino == 0 && it.Kind == "movie" { // a film folder the catalogue has not named a video of
			if vid, ok := v.firstVideo(it.Ino); ok {
				ino = vid.Ino
			}
		}
		if r, ok := ps.row(ino); ok {
			it.Progress = progressOf(r)
		}
	case "show":
		seen := false
		for _, d := range group {
			seen = seen || ps.under[d.in.Ino]
		}
		if !seen {
			return
		}
		next, all := v.showNext(v.seasons(group))
		it.Watched = all
		if next != nil {
			it.Next = next.Path
		}
	case "album":
		for _, k := range v.liveKids(it.Ino) {
			if r, ok := ps.row(k.Ino); ok && k.Type == meta.TFile {
				it.Plays += int(r.Plays)
			}
		}
	}
}

// regular is a show's numbered seasons' episodes in order, Specials left out.
func regular(seasons []MediaSeason) []*MediaItem {
	var out []*MediaItem
	for i := range seasons {
		if seasons[i].N == 0 {
			continue
		}
		for j := range seasons[i].Episodes {
			out = append(out, &seasons[i].Episodes[j])
		}
	}
	return out
}

// showNext is a show's next episode for the viewer: the one in progress (the newest),
// else the one after the furthest watched, else the first; and whether every episode of
// its numbered seasons is watched.
func (v mediaView) showNext(seasons []MediaSeason) (*MediaItem, bool) {
	eps := regular(seasons)
	if len(eps) == 0 {
		return nil, false
	}
	var inProgress *MediaItem
	var at int64
	last, all := -1, true
	for i, e := range eps {
		r, ok := v.play.row(e.VideoIno)
		if ok {
			e.Progress = progressOf(r)
		}
		switch {
		case ok && r.Watched:
			last = i
		case ok && r.Pos >= ResumeMin && (inProgress == nil || r.At > at):
			inProgress, at, all = e, r.At, false
		default:
			all = false
		}
	}
	if inProgress != nil {
		return inProgress, false
	}
	if last+1 < len(eps) {
		return eps[last+1], all
	}
	return eps[0], all
}

// trackPlays lists an album's tracks the viewer has rows for.
func (v mediaView) trackPlays(album meta.Ino) []TrackPlay {
	var out []TrackPlay
	for _, k := range v.liveKids(album) {
		r, ok := v.play.row(k.Ino)
		if !ok || k.Type != meta.TFile {
			continue
		}
		tp := TrackPlay{Name: k.Name, Plays: r.Plays}
		if r.Dur > LongTrack {
			tp.Progress = progressOf(r)
		}
		out = append(out, tp)
	}
	return out
}

// --- Continue watching (I-409)

// continueRow is GET /api/media?continue=1.
func (s *Server) continueRow(w http.ResponseWriter, v mediaView) {
	items := []MediaItem{}
	ps := v.play
	type rowAt struct {
		ino meta.Ino
		r   meta.PlayRow
	}
	rows := make([]rowAt, 0, len(ps.rows))
	for ino, r := range ps.rows {
		rows = append(rows, rowAt{ino, r})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].r.At != rows[j].r.At {
			return rows[i].r.At > rows[j].r.At
		}
		return rows[i].ino < rows[j].ino
	})
	seenShow := map[meta.Ino]bool{}
	seenItem := map[string]bool{}
	add := func(it MediaItem) {
		if !seenItem[it.Path] && len(items) < ContinueMax {
			seenItem[it.Path] = true
			items = append(items, it)
		}
	}
	resumable := func(r meta.PlayRow) bool { return !r.Watched && !r.Hidden && r.Pos >= ResumeMin }
	for _, ra := range rows {
		if len(items) >= ContinueMax {
			break
		}
		in, ok := s.o.Store.GetInode(ra.ino)
		if !ok || in.Type != meta.TFile || !s.readable(v.r, in.Ino) {
			continue
		}
		parts, err := s.o.Store.PathOf(in.Ino)
		if err != nil || len(parts) == 0 || parts[0] == meta.ReservedDir {
			continue
		}
		p := "/" + strings.Join(parts, "/")
		_, _, lib := s.effective(in)
		switch lib {
		case meta.LibraryMovies:
			if !resumable(ra.r) {
				continue
			}
			if it, ok := v.movieOf(in, p); ok {
				it.Progress = progressOf(ra.r)
				add(it)
			}
		case meta.LibraryTV:
			show, ok := v.showOf(in)
			if !ok {
				continue
			}
			newest := !seenShow[show.Ino]
			seenShow[show.Ino] = true
			switch {
			case resumable(ra.r):
				it := v.item(in, p, "episode")
				it.Progress, it.Show = progressOf(ra.r), v.showTitle(show)
				add(it)
			case newest && ra.r.Watched && !ra.r.Hidden:
				sp, err := s.o.Store.PathOf(show.Ino)
				if err != nil {
					continue
				}
				group := v.showGroup(show.Parent, &show)
				for i := range group {
					group[i].path = path.Join("/"+strings.Join(sp[:len(sp)-1], "/"), group[i].path)
				}
				eps := regular(v.seasons(group))
				for i, e := range eps {
					if e.VideoIno != ra.ino || i+1 >= len(eps) {
						continue
					}
					if _, has := ps.row(eps[i+1].VideoIno); !has {
						it := *eps[i+1]
						it.NextUp, it.Show = true, v.showTitle(show)
						add(it)
					}
					break
				}
			}
		case meta.LibraryMusic:
			if !resumable(ra.r) || ra.r.Dur <= LongTrack {
				continue
			}
			it := MediaItem{Ino: in.Ino, Path: p, Kind: "track", Title: strings.TrimSuffix(in.Name, path.Ext(in.Name)), Added: time.Unix(0, in.Mtime).UTC(), Progress: progressOf(ra.r)}
			if e, ok := v.cat[in.Parent]; ok {
				it.Album, it.Artist, it.Year = e.Title, e.Artist, e.Year
			}
			add(it)
		case meta.LibraryNone, meta.LibraryRoms:
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// movieOf is the film a video belongs to: its folder, or the file itself when it lies
// loose in the library.
func (v mediaView) movieOf(in meta.Inode, p string) (MediaItem, bool) {
	parent, ok := v.s.o.Store.GetInode(in.Parent)
	if !ok {
		return MediaItem{}, false
	}
	if parent.Library() != meta.LibraryNone {
		it := v.item(in, p, "movie")
		return it, true
	}
	it := v.item(parent, path.Dir(p), "movie")
	it.Video = p
	v.video(&it)
	return it, true
}

// showOf is the show folder an episode file lies under: the folder whose parent is the
// library.
func (v mediaView) showOf(in meta.Inode) (meta.Inode, bool) {
	cur := in
	for range 8 {
		parent, ok := v.s.o.Store.GetInode(cur.Parent)
		if !ok {
			return meta.Inode{}, false
		}
		if parent.Library() != meta.LibraryNone {
			return cur, cur.Type == meta.TDir
		}
		cur = parent
	}
	return meta.Inode{}, false
}

func (v mediaView) showTitle(show meta.Inode) string {
	if e, ok := v.cat[show.Ino]; ok && e.Title != "" {
		return e.Title
	}
	return search.Analyze(show.Name, nil, search.CtxTV).Title
}
