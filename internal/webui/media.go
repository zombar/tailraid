package webui

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
)

// Cataloger is the node's media catalogue (docs/18 §8): search.Service.
type Cataloger interface {
	Catalog(ctx context.Context) ([]search.Entry, error)
}

// MediaPage is the most items one /api/media?lib= answer lists (docs/13 §10, I-212).
const MediaPage = 500

// MediaLib is one library folder.
type MediaLib struct {
	Path   string   `json:"path"`
	Kind   string   `json:"kind"`
	Count  int      `json:"count"`
	Genres []string `json:"genres,omitempty"`
}

// MediaItem is a film, show, season, episode or album (docs/13 §3).
type MediaItem struct {
	Ino        meta.Ino      `json:"ino"`
	Path       string        `json:"path"`
	Kind       string        `json:"kind"` // movie | show | season | episode | album
	Title      string        `json:"title"`
	SortTitle  string        `json:"sort_title"`
	Year       int           `json:"year,omitempty"`
	Genres     []string      `json:"genres,omitempty"`
	Rating     string        `json:"rating,omitempty"`
	Runtime    int           `json:"runtime,omitempty"`
	MPAA       string        `json:"mpaa,omitempty"`
	Artist     string        `json:"artist,omitempty"`
	Poster     string        `json:"poster,omitempty"` // volume paths
	Fanart     string        `json:"fanart,omitempty"`
	Thumb      string        `json:"thumb,omitempty"`
	Video      string        `json:"video,omitempty"` // the file a film or an episode plays
	Size       uint64        `json:"size,omitempty"`
	Added      time.Time     `json:"added"`
	NSFW       bool          `json:"nsfw"`
	Children   int           `json:"children,omitempty"` // a show's seasons, an album's tracks
	Plot       string        `json:"plot,omitempty"`
	People     []string      `json:"people,omitempty"`
	Season     int           `json:"season,omitempty"`
	Episode    int           `json:"episode,omitempty"`
	EpisodeTo  int           `json:"episode_to,omitempty"`
	Aired      string        `json:"aired,omitempty"`
	Seasons    []MediaSeason `json:"seasons,omitempty"`
	Tracks     []string      `json:"tracks,omitempty"` // an album's audio files, by name
	Catalogued bool          `json:"catalogued"`       // false: named from the tree only, until the next pass
}

// MediaSeason is one season of a show and its episodes.
type MediaSeason struct {
	N        int         `json:"n"`
	Title    string      `json:"title"`
	Poster   string      `json:"poster,omitempty"`
	Episodes []MediaItem `json:"episodes"`
}

// mediaView is one request's view: the catalogue by inode.
type mediaView struct {
	s   *Server
	cat map[meta.Ino]search.Entry
}

func (s *Server) mediaView(ctx context.Context) mediaView {
	v := mediaView{s: s, cat: map[meta.Ino]search.Entry{}}
	if s.o.Catalog != nil {
		if es, err := s.o.Catalog.Catalog(ctx); err == nil {
			for i := range es {
				v.cat[es[i].Ino] = es[i]
			}
		}
	}
	return v
}

// media is GET /api/media: the libraries, a library's items, or one item (docs/13 §10).
func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := s.mediaView(r.Context())
	switch {
	case q.Has("item"):
		s.mediaItem(w, r, v)
	case q.Has("lib"):
		s.mediaLib(w, r, v)
	default:
		libs := []MediaLib{}
		for _, l := range s.o.Store.Libraries() {
			if strings.HasPrefix(l.Path, "/"+meta.ReservedDir+"/") {
				continue
			}
			libs = append(libs, MediaLib{Path: l.Path, Kind: l.Kind.String()})
		}
		writeJSON(w, http.StatusOK, map[string]any{"libs": libs})
	}
}

func (v mediaView) path(ino meta.Ino) string {
	parts, err := v.s.o.Store.PathOf(ino)
	if err != nil {
		return ""
	}
	return "/" + strings.Join(parts, "/")
}

// art is a catalogue art name made a volume path: beside the item, or in the
// library for a loose file.
func art(dir, name string) string {
	if name == "" {
		return ""
	}
	return path.Join(dir, name)
}

// item builds an item from the tree, then the catalogue's fields where an entry exists.
func (v mediaView) item(in meta.Inode, p, kind string) MediaItem {
	it := MediaItem{Ino: in.Ino, Path: p, Kind: kind, Added: time.Unix(0, in.Mtime).UTC(), Size: in.Size}
	_, it.NSFW, _ = v.s.effective(in)
	name := in.Name
	if in.Type == meta.TFile {
		name = strings.TrimSuffix(name, path.Ext(name))
	}
	it.Title, it.Year = search.CleanName(name)
	if kind == "album" || kind == "season" {
		it.Title, it.Year = in.Name, 0
	}
	dir := p
	if in.Type == meta.TFile {
		dir = path.Dir(p)
		it.Video = p
	}
	if e, ok := v.cat[in.Ino]; ok {
		it.Catalogued = true
		it.Title, it.SortTitle, it.Year, it.Genres, it.Rating, it.Runtime = e.Title, e.SortTitle, e.Year, e.Genres, e.Rating, e.Runtime
		it.MPAA, it.Plot, it.People, it.Aired, it.Artist = e.MPAA, e.Plot, e.People, e.Aired, e.Artist
		it.Season, it.Episode, it.EpisodeTo = max(e.Season, 0), e.Episode, e.EpisodeTo
		it.Poster, it.Fanart, it.Thumb = art(dir, e.Poster), art(dir, e.Fanart), art(dir, e.Thumb)
		if e.Video != 0 && e.Video != in.Ino {
			if vp := v.path(e.Video); vp != "" {
				it.Video = vp
			}
		}
	}
	if it.SortTitle == "" {
		it.SortTitle = search.SortTitle(it.Title)
	}
	return it
}

// liveKids is a folder's children a listing shows: live, not ephemeral, not hidden.
func (v mediaView) liveKids(dir meta.Ino) []*meta.Inode {
	kids, _ := v.s.o.Store.ReadDir(dir)
	out := make([]*meta.Inode, 0, len(kids))
	for i := range kids {
		if !meta.IsEphemeral(kids[i].Name) && !strings.HasPrefix(kids[i].Name, ".") {
			out = append(out, &kids[i])
		}
	}
	return out
}

func (v mediaView) firstVideo(dir meta.Ino) (meta.Inode, bool) {
	for _, k := range v.liveKids(dir) {
		if k.Type == meta.TFile && search.IsVideo(k.Name) {
			return *k, true
		}
	}
	return meta.Inode{}, false
}

// album reports a folder the album view would show as one (13 §6) and its tracks.
func (v mediaView) album(dir meta.Ino) ([]string, bool) {
	var tracks []string
	other := 0
	for _, k := range v.liveKids(dir) {
		switch {
		case k.Type != meta.TFile:
		case search.IsAudio(k.Name):
			tracks = append(tracks, k.Name)
		default:
			other++
		}
	}
	return tracks, len(tracks) > 0 && len(tracks) > other
}

func (s *Server) mediaLib(w http.ResponseWriter, r *http.Request, v mediaView) {
	q := r.URL.Query()
	parts, canonical := cleanPath(q.Get("lib"))
	in, err := s.o.Store.Resolve(parts)
	if err != nil || in.Type != meta.TDir || in.Library() == meta.LibraryNone {
		writeErr(w, http.StatusNotFound, "media "+canonical+": not a library folder")
		return
	}
	offset := 0
	if o := q.Get("offset"); o != "" {
		if offset, err = strconv.Atoi(o); err != nil || offset < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("media: offset %q", o))
			return
		}
	}
	order := q.Get("sort")
	switch order {
	case "", "title", "year", "added":
	default:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("media: sort %q: want title, year or added", order))
		return
	}
	kind := in.Library()
	var items []MediaItem
	for _, k := range v.liveKids(in.Ino) {
		p := path.Join(canonical, k.Name)
		if k.Type == meta.TDir && k.Library() != meta.LibraryNone {
			continue // a library inside a library is its own section
		}
		switch kind {
		case meta.LibraryMovies:
			if k.Type == meta.TDir {
				if _, ok := v.firstVideo(k.Ino); ok {
					items = append(items, v.item(*k, p, "movie"))
				}
			} else if k.Type == meta.TFile && search.IsVideo(k.Name) {
				items = append(items, v.item(*k, p, "movie"))
			}
		case meta.LibraryTV:
			if k.Type == meta.TDir {
				it := v.item(*k, p, "show")
				for _, sk := range v.liveKids(k.Ino) {
					if _, ok := search.SeasonOf(sk.Name); ok && sk.Type == meta.TDir {
						it.Children++
					}
				}
				items = append(items, it)
			}
		case meta.LibraryMusic:
			if k.Type != meta.TDir {
				continue
			}
			if tracks, ok := v.album(k.Ino); ok {
				it := v.item(*k, p, "album")
				it.Children = len(tracks)
				items = append(items, it)
				continue
			}
			for _, a := range v.liveKids(k.Ino) {
				if a.Type != meta.TDir || a.Library() != meta.LibraryNone {
					continue
				}
				if tracks, ok := v.album(a.Ino); ok {
					it := v.item(*a, path.Join(p, a.Name), "album")
					it.Children = len(tracks)
					if it.Artist == "" {
						it.Artist = k.Name
					}
					items = append(items, it)
				}
			}
		case meta.LibraryNone:
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := &items[i], &items[j]
		switch order {
		case "year":
			if a.Year != b.Year {
				return a.Year > b.Year
			}
		case "added":
			if !a.Added.Equal(b.Added) {
				return a.Added.After(b.Added)
			}
		}
		if a.SortTitle != b.SortTitle {
			return a.SortTitle < b.SortTitle
		}
		return a.Path < b.Path
	})
	genres := map[string]bool{}
	for i := range items {
		for _, g := range items[i].Genres {
			genres[strings.ToLower(g)] = true
		}
	}
	lib := MediaLib{Path: canonical, Kind: kind.String(), Count: len(items)}
	for g := range genres {
		lib.Genres = append(lib.Genres, g)
	}
	sort.Strings(lib.Genres)
	page := []MediaItem{}
	if offset < len(items) {
		page = items[offset:min(offset+MediaPage, len(items))]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lib": lib, "items": page})
}

func (s *Server) mediaItem(w http.ResponseWriter, r *http.Request, v mediaView) {
	t, err := s.resolveTarget(r, r.URL.Query().Get("item"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if t.trashed || (len(t.parts) > 0 && t.parts[0] == meta.ReservedDir) || t.in.Library() != meta.LibraryNone {
		writeErr(w, http.StatusNotFound, "media "+t.canonical+": not in a library")
		return
	}
	_, _, lib := s.effective(t.in)
	in, p := t.in, t.canonical
	var it MediaItem
	switch {
	case lib == meta.LibraryMovies:
		it = v.item(in, p, "movie")
		if in.Type == meta.TDir && it.Video == "" {
			if vid, ok := v.firstVideo(in.Ino); ok {
				it.Video = path.Join(p, vid.Name)
			}
		}
	case lib == meta.LibraryTV && in.Type == meta.TDir:
		it = v.item(in, p, "show")
		it.Seasons = v.seasons(in, p)
		it.Children = len(it.Seasons)
	case lib == meta.LibraryMusic && in.Type == meta.TDir:
		tracks, _ := v.album(in.Ino)
		it = v.item(in, p, "album")
		it.Tracks, it.Children = tracks, len(tracks)
	default:
		writeErr(w, http.StatusNotFound, "media "+p+": not an item of a library")
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// seasons walks a show live: episode files in the show and in its season folders, with
// the catalogue's fields where they exist, grouped by season, specials last.
func (v mediaView) seasons(show meta.Inode, p string) []MediaSeason {
	by := map[int]*MediaSeason{}
	get := func(n int) *MediaSeason {
		if s, ok := by[n]; ok {
			return s
		}
		title := "Season " + strconv.Itoa(n)
		if n == 0 {
			title = "Specials"
		}
		s := &MediaSeason{N: n, Title: title, Episodes: []MediaItem{}}
		by[n] = s
		return s
	}
	episodes := func(dir meta.Ino, dp string, folder int) {
		for _, f := range v.liveKids(dir) {
			if f.Type != meta.TFile || !search.IsVideo(f.Name) {
				continue
			}
			season, ep, to, ok := search.ParseEpisode(strings.TrimSuffix(f.Name, path.Ext(f.Name)))
			if !ok {
				continue
			}
			if season < 0 {
				season = max(folder, 1)
			}
			it := v.item(*f, path.Join(dp, f.Name), "episode")
			if !it.Catalogued {
				it.Season, it.Episode, it.EpisodeTo = season, ep, to
				showTitle, _ := search.CleanName(show.Name)
				if e, ok := v.cat[show.Ino]; ok {
					showTitle = e.Title
				}
				it.Title = search.EpisodeTitle(strings.TrimSuffix(f.Name, path.Ext(f.Name)), showTitle, ep)
				it.SortTitle = search.SortTitle(it.Title)
			}
			s := get(it.Season)
			s.Episodes = append(s.Episodes, it)
		}
	}
	episodes(show.Ino, p, -1)
	for _, k := range v.liveKids(show.Ino) {
		n, ok := search.SeasonOf(k.Name)
		if k.Type != meta.TDir || !ok {
			continue
		}
		s := get(n)
		s.Title = k.Name
		if e, ok := v.cat[k.Ino]; ok && e.Poster != "" {
			// seasonNN-poster.* lives in the show's folder, poster.* or folder.* in the season's (docs/18 §8)
			if strings.HasPrefix(strings.ToLower(e.Poster), "season") {
				s.Poster = path.Join(p, e.Poster)
			} else {
				s.Poster = path.Join(p, k.Name, e.Poster)
			}
		}
		episodes(k.Ino, path.Join(p, k.Name), n)
	}
	out := make([]MediaSeason, 0, len(by))
	for _, s := range by {
		sort.SliceStable(s.Episodes, func(i, j int) bool {
			a, b := &s.Episodes[i], &s.Episodes[j]
			if a.Episode != b.Episode {
				return a.Episode < b.Episode
			}
			return a.Path < b.Path
		})
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].N == 0) != (out[j].N == 0) {
			return out[j].N == 0 // specials last
		}
		return out[i].N < out[j].N
	})
	return out
}
