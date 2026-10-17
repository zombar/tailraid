package webui

import (
	"cmp"
	"path"
	"sort"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
)

// MediaFile is one file of a game: a disc, or another dump of the title (docs/13 §10).
type MediaFile struct {
	Path     string   `json:"path"`
	Name     string   `json:"name"`
	Size     uint64   `json:"size"`
	Disc     int      `json:"disc,omitempty"`
	Regions  []string `json:"regions,omitempty"`
	Revision string   `json:"revision,omitempty"`
	Flags    []string `json:"flags,omitempty"`
}

// romLib is a roms library's live entries: platforms and games from the tree and the
// names alone (search.RomEntries), by platform.
type romLib struct {
	platforms []search.Entry
	games     map[meta.Ino][]search.Entry // by platform, in walk order
}

func (v mediaView) romLib(lib meta.Ino) romLib {
	r := romLib{games: map[meta.Ino][]search.Entry{}}
	es := search.RomEntries(v.s.o.Store, lib)
	for i := range es {
		e := &es[i]
		switch e.Kind {
		case search.EntPlatform:
			r.platforms = append(r.platforms, *e)
		case search.EntGame:
			r.games[e.Parent] = append(r.games[e.Parent], *e)
		}
	}
	return r
}

// platforms lists a roms library's platforms (docs/13 §10, I-279): the live structure,
// the catalogue's fields where they exist.
func (v mediaView) platforms(lib meta.Ino) []MediaItem {
	r := v.romLib(lib)
	out := make([]MediaItem, 0, len(r.platforms))
	for i := range r.platforms {
		e := &r.platforms[i]
		in, ok := v.s.o.Store.GetInode(e.Ino)
		if !ok || !v.s.readable(v.r, in.Ino) {
			continue
		}
		it := v.romFromEntry(in, v.path(e.Ino), "platform", *e)
		it.Children = len(titles(r.games[e.Ino]))
		out = append(out, it)
	}
	return out
}

// titles groups a platform's games by title: one card per title, the first dump by path
// its face (docs/13 §10).
func titles(games []search.Entry) [][]search.Entry {
	by := map[string]int{}
	var out [][]search.Entry
	for i := range games {
		g := &games[i]
		k := search.SortTitle(g.Title)
		if j, ok := by[k]; ok {
			out[j] = append(out[j], *g)
			continue
		}
		by[k] = len(out)
		out = append(out, []search.Entry{*g})
	}
	return out
}

// romFromEntry is an item of a platform or a game: the tree's entry, the catalogue's
// gamelist fields over it when the pass has read them.
func (v mediaView) romFromEntry(in meta.Inode, p, kind string, e search.Entry) MediaItem {
	it := v.item(in, p, kind)
	if c, ok := v.cat[in.Ino]; ok {
		e.Title, e.Plot, e.Year, e.Genres, e.Rating = c.Title, c.Plot, c.Year, c.Genres, c.Rating
		e.Developer, e.Publisher, e.Players = c.Developer, c.Publisher, c.Players
		// art the catalogue found wins; one it has none for keeps the tree's, so a
		// catalogue from before a platform's fallback (I-400) or the art does not hide it
		e.Poster, e.Banner = cmp.Or(c.Poster, e.Poster), cmp.Or(c.Banner, e.Banner)
	}
	dir := p
	if in.Type == meta.TFile {
		dir = path.Dir(p)
	}
	it.Title, it.SortTitle, it.Year, it.Plot, it.Genres, it.Rating = e.Title, search.SortTitle(e.Title), e.Year, e.Plot, e.Genres, e.Rating
	it.Platform, it.Regions, it.Revision, it.Flags, it.Languages, it.Discs = e.Platform, e.Regions, e.Revision, e.Flags, e.Languages, e.Discs
	it.Developer, it.Publisher, it.Players = e.Developer, e.Publisher, e.Players
	it.Poster, it.Fanart = art(dir, e.Poster), art(dir, e.Banner)
	it.Video, it.VideoIno, it.VideoVersion, it.VideoThumb = "", 0, 0, false // a game is not played (I-279)
	return it
}

// romItem is a platform with its games, one per title, or a game with its files, the
// discs of one title and its other dumps (docs/13 §10, I-279).
func (v mediaView) romItem(in meta.Inode, p string) (MediaItem, bool) {
	var lib meta.Ino
	for cur := in; ; {
		if cur.Library() == meta.LibraryRoms {
			lib = cur.Ino
			break
		}
		parent, ok := v.s.o.Store.GetInode(cur.Parent)
		if !ok || cur.Ino == meta.RootIno {
			return MediaItem{}, false
		}
		cur = parent
	}
	r := v.romLib(lib)
	for pi := range r.platforms {
		pl := &r.platforms[pi]
		if pl.Ino == in.Ino {
			it := v.romFromEntry(in, p, "platform", *pl)
			for _, group := range titles(r.games[pl.Ino]) {
				g := group[0]
				gin, ok := v.s.o.Store.GetInode(g.Ino)
				if !ok || !v.s.readable(v.r, gin.Ino) {
					continue
				}
				gi := v.romFromEntry(gin, v.path(g.Ino), "game", g)
				gi.Variants = len(group) - 1
				it.Games = append(it.Games, gi)
			}
			sort.SliceStable(it.Games, func(i, j int) bool {
				if it.Games[i].SortTitle != it.Games[j].SortTitle {
					return it.Games[i].SortTitle < it.Games[j].SortTitle
				}
				return it.Games[i].Path < it.Games[j].Path
			})
			it.Children = len(it.Games)
			return it, true
		}
		for _, group := range titles(r.games[pl.Ino]) {
			for gi := range group {
				if group[gi].Ino != in.Ino {
					continue
				}
				it := v.romFromEntry(in, p, "game", group[gi])
				for di := range group {
					it.Files = append(it.Files, v.gameFiles(&group[di])...)
				}
				return it, true
			}
		}
	}
	return MediaItem{}, false
}

// gameFiles is one game's files: a disc set's folder's files, a game's discs (its
// siblings that name the same title and tags), or the file alone.
func (v mediaView) gameFiles(e *search.Entry) []MediaFile {
	in, ok := v.s.o.Store.GetInode(e.Ino)
	if !ok || !v.s.readable(v.r, in.Ino) {
		return nil
	}
	p := v.path(e.Ino)
	file := func(k meta.Inode, kp string, disc int) MediaFile {
		return MediaFile{Path: kp, Name: k.Name, Size: k.Size, Disc: disc, Regions: e.Regions, Revision: e.Revision, Flags: e.Flags}
	}
	if in.Type == meta.TDir {
		var out []MediaFile
		for _, k := range v.liveKids(in.Ino) {
			if k.Type == meta.TFile {
				out = append(out, file(*k, path.Join(p, k.Name), 0))
			}
		}
		return out
	}
	if e.Discs < 2 {
		return []MediaFile{file(in, p, 0)}
	}
	first := search.Analyze(in.Name, nil, search.CtxRoms)
	var out []MediaFile
	for _, k := range v.liveKids(in.Parent) {
		if k.Type != meta.TFile {
			continue
		}
		if n := search.Analyze(k.Name, nil, search.CtxRoms); search.SameGame(first, n) {
			out = append(out, file(*k, path.Join(path.Dir(p), k.Name), n.Disc))
		}
	}
	return out
}
