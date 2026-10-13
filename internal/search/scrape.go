package search

import (
	"context"

	"github.com/zombar/tailraid/internal/meta"
)

// The scrape's plan (docs/18 §11): every library item with a sidecar or art missing,
// derived by the catalogue's own walk so a slot is missing exactly when no name the
// catalogue reads for it exists. No provider is contacted here (I-298): the plan is
// what the CLI asks the node for before it asks a provider.

// Slot is one thing the catalogue reads for an item, missing, and the name a scrape
// writes it under in the item's folder.
type Slot struct {
	Slot string `json:"slot"` // nfo | poster | fanart | thumb | cover
	Name string `json:"name"`
}

// The item kinds a scrape handles.
const (
	ScrapeMovie   = "movie"
	ScrapeShow    = "show"
	ScrapeSeason  = "season"
	ScrapeEpisode = "episode"
	ScrapeAlbum   = "album"
	ScrapeGame    = "game"
)

// ScrapeItem is one item of the plan. Dir is the volume path of the folder its files go
// in, filled by the node; a show is listed when any of its slots is missing or any of
// its seasons or episodes is listed, since those are matched through the show.
type ScrapeItem struct {
	Kind    string `json:"kind"`
	Ino     uint64 `json:"ino"`
	Dir     string `json:"dir"`
	Show    uint64 `json:"show,omitempty"` // a season's or an episode's show
	Title   string `json:"title"`
	Year    int    `json:"year,omitempty"`
	Season  int    `json:"season"` // -1: none
	Episode int    `json:"episode,omitempty"`
	Artist  string `json:"artist,omitempty"`
	// a game's: the name its box art is looked up by (the file's stem, a disc set's
	// folder name), its platform's libretro system and its regions
	Key     string   `json:"key,omitempty"`
	System  string   `json:"system,omitempty"`
	Regions []string `json:"regions,omitempty"`
	Missing []Slot   `json:"missing,omitempty"`

	DirIno meta.Ino `json:"-"` // the folder Dir names
	Track  meta.Ino `json:"-"` // an album's first track: a picture in it is a cover
}

// planItem is e's plan item and whether anything of it is missing.
func planItem(e Entry, dir meta.Ino, stem string, hasNFO bool, track meta.Ino) ScrapeItem {
	it := ScrapeItem{Ino: uint64(e.Ino), DirIno: dir, Title: e.Title, Year: e.Year, Season: e.Season, Episode: e.Episode, Artist: e.Artist, Track: track}
	miss := func(slot, name string) { it.Missing = append(it.Missing, Slot{Slot: slot, Name: name}) }
	switch e.Kind {
	case EntMovie:
		it.Kind = ScrapeMovie
		nfo, poster, fanart := "movie.nfo", "poster.jpg", "fanart.jpg"
		if stem != "" { // a film file directly in the library: its sidecars carry its stem
			nfo, poster, fanart = stem+".nfo", stem+"-poster.jpg", stem+"-fanart.jpg"
		}
		if !hasNFO {
			miss("nfo", nfo)
		}
		if e.Poster == "" {
			miss("poster", poster)
		}
		if e.Fanart == "" {
			miss("fanart", fanart)
		}
	case EntShow:
		it.Kind = ScrapeShow
		if !hasNFO {
			miss("nfo", "tvshow.nfo")
		}
		if e.Poster == "" {
			miss("poster", "poster.jpg")
		}
		if e.Fanart == "" {
			miss("fanart", "fanart.jpg")
		}
	case EntSeason:
		it.Kind, it.Show = ScrapeSeason, uint64(e.Parent)
		if e.Poster == "" {
			miss("poster", stem+"-poster.jpg")
		}
	case EntEpisode:
		it.Kind, it.Show = ScrapeEpisode, uint64(e.Parent)
		if e.Season < 0 || e.Episode <= 0 {
			return it // an air-date or absolute episode: no number to ask for
		}
		if !hasNFO {
			miss("nfo", stem+".nfo")
		}
		if e.Thumb == "" {
			miss("thumb", stem+"-thumb.jpg")
		}
	case EntAlbum:
		it.Kind = ScrapeAlbum
		if e.Poster == "" {
			miss("cover", "cover.jpg")
		}
	case EntGame:
		it.Kind, it.Key, it.System, it.Regions = ScrapeGame, stem, Libretro(e.Platform), e.Regions
		if e.Poster != "" || it.System == "" {
			return it // has art, or a platform the thumbnail set does not cover
		}
		if dir == e.Ino { // a disc set's folder: its art is boxart.* inside it
			miss("boxart", "boxart.png")
		} else {
			miss("boxart", stem+".png")
		}
	}
	return it
}

// scrapePlan walks the libraries and returns the plan in catalogue order.
func scrapePlan(src Source, nfo func(meta.Inode) (NFO, bool), tags func(meta.Ino) (Doc, bool), games func(meta.Inode) (map[string]GameInfo, bool)) []ScrapeItem {
	var all []ScrapeItem
	c := &catalogue{
		src: src, nfo: nfo, tags: tags, games: games, seen: map[meta.Ino]bool{},
		note: func(e Entry, dir meta.Ino, stem string, hasNFO bool, track meta.Ino) {
			if e.Kind == EntMovie || e.Kind == EntShow || e.Kind == EntSeason || e.Kind == EntEpisode || e.Kind == EntAlbum || e.Kind == EntGame {
				all = append(all, planItem(e, dir, stem, hasNFO, track))
			}
		},
	}
	c.walk()
	wanted := map[uint64]bool{}
	for i := range all {
		if len(all[i].Missing) > 0 && all[i].Show != 0 {
			wanted[all[i].Show] = true
		}
	}
	out := []ScrapeItem{}
	for i := range all {
		if len(all[i].Missing) > 0 || all[i].Kind == ScrapeShow && wanted[all[i].Ino] {
			out = append(out, all[i])
		}
	}
	return out
}

// ScrapePlan is this node's plan from its own FSM, its sidecars and its index's tags
// (an album's artist and title); Dir is left for the caller, which knows the paths.
func (s *Service) ScrapePlan(ctx context.Context) []ScrapeItem {
	x, _ := s.index(ctx) // no index: albums keep their folder's names
	tags := func(ino meta.Ino) (Doc, bool) {
		if x == nil {
			return Doc{}, false
		}
		if si, di, ok := x.FindDoc(ino); ok {
			return x.Segs[si].Doc(di), true
		}
		return Doc{}, false
	}
	cfg := s.Cfg
	return scrapePlan(s.Src, func(in meta.Inode) (NFO, bool) { return s.readNFO(ctx, in, cfg) }, tags,
		func(in meta.Inode) (map[string]GameInfo, bool) { return s.readGamelist(ctx, in, cfg) }) // art a gamelist names is present
}
