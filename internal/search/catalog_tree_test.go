package search

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

func (e *env) catalog() map[string]Entry {
	e.t.Helper()
	cat, err := e.svc.Catalog(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]Entry{}
	for i := range cat {
		c := &cat[i]
		parts, err := e.f.FSM.PathOf(c.Ino)
		if err != nil {
			e.t.Fatal(err)
		}
		p := "/"
		for i, s := range parts {
			if i > 0 {
				p += "/"
			}
			p += s
		}
		out[p] = *c
	}
	return out
}

var img = []byte{0xff, 0xd8, 0xff, 0xe0}

// Art resolves in the documented order for each slot.
// Invariant: I-211
func TestCatalog_ArtResolutionOrder(t *testing.T) {
	for _, tc := range []struct {
		files                 []string
		poster, fanart, thumb string
	}{
		{[]string{"poster.jpg", "film-poster.jpg", "folder.jpg", "fanart.png", "film-fanart.jpg", "film-thumb.jpg"}, "poster.jpg", "fanart.png", "film-thumb.jpg"},
		{[]string{"film-poster.jpg", "folder.jpg", "film-fanart.webp"}, "film-poster.jpg", "film-fanart.webp", ""},
		{[]string{"folder.jpeg"}, "folder.jpeg", "", ""},
		{[]string{"Poster.JPG"}, "Poster.JPG", "", ""},
		{nil, "", "", ""},
	} {
		e := newEnv(t)
		e.library("/films", meta.LibraryMovies)
		e.write("/films/Film (2001)/film.mkv", []byte("video"))
		for _, f := range tc.files {
			e.write("/films/Film (2001)/"+f, img)
		}
		e.pass()
		got := e.catalog()["/films/Film (2001)"]
		if got.Poster != tc.poster || got.Fanart != tc.fanart || got.Thumb != tc.thumb {
			t.Errorf("%v: poster %q fanart %q thumb %q", tc.files, got.Poster, got.Fanart, got.Thumb)
		}
	}
}

// Each library kind's structure: films as folders and loose files, shows with season
// folders and loose episodes, artists of albums and albums directly in the library;
// a library inside a library is its own section.
// Invariant: I-211
func TestCatalog_LibraryKinds_Structure(t *testing.T) {
	e := newEnv(t)
	e.library("/films", meta.LibraryMovies)
	e.write("/films/Arrival (2016)/arrival.mkv", []byte("v"))
	e.write("/films/Arrival (2016)/movie.nfo", []byte(`<movie><title>Arrival</title><plot>Heptapods.</plot><genre>Drama</genre></movie>`))
	e.write("/films/Blade.Runner.1982.1080p.mkv", []byte("v"))
	e.write("/films/Extras/readme.txt", []byte("no video here"))
	e.library("/films/Shorts", meta.LibraryMovies) // a nested library: its own section
	e.write("/films/Shorts/Tiny (2020)/tiny.mp4", []byte("v"))

	e.library("/shows", meta.LibraryTV)
	e.write("/shows/Dark/tvshow.nfo", []byte(`<tvshow><title>Dark</title><year>2017</year></tvshow>`))
	e.write("/shows/Dark/Season 1/Dark.S01E01.mkv", []byte("v"))
	e.write("/shows/Dark/Season 1/Dark.S01E02E03.mkv", []byte("v"))
	e.write("/shows/Dark/Season 1/Episode 4.mkv", []byte("v"))
	e.write("/shows/Dark/Dark.S02E01.mkv", []byte("v"))
	e.write("/shows/Dark/season01-poster.jpg", img)
	e.write("/shows/Dark/trailer.mkv", []byte("v"))

	e.library("/music", meta.LibraryMusic)
	for i := range 3 {
		e.write(fmt.Sprintf("/music/Agnes Obel/Philharmonics/%02d.mp3", i+1), id3(0, "TIT2", fmt.Sprintf("Track %d", i), "TPE1", "Agnes Obel", "TALB", "Philharmonics", "TYER", "2010"))
	}
	e.write("/music/Agnes Obel/artist.nfo", []byte(`<artist><biography>Danish.</biography></artist>`))
	e.write("/music/Agnes Obel/Scans/a.jpg", img)
	e.write("/music/Agnes Obel/Scans/b.jpg", img)
	e.write("/music/Loose Album/01.flac", []byte("f"))
	e.write("/music/Loose Album/02.flac", []byte("g")) // audio outnumbers the cover, as the album view counts (13 §6)
	e.write("/music/Loose Album/cover.jpg", img)
	e.pass()
	cat := e.catalog()
	type want struct {
		kind                   uint8
		title                  string
		year, season, ep, epTo int
		plot, poster           string
	}
	for p, w := range map[string]want{
		"/films/Arrival (2016)":                   {EntMovie, "Arrival", 2016, -1, 0, 0, "Heptapods.", ""},
		"/films/Blade.Runner.1982.1080p.mkv":      {EntMovie, "Blade Runner", 1982, -1, 0, 0, "", ""},
		"/films/Shorts/Tiny (2020)":               {EntMovie, "Tiny", 2020, -1, 0, 0, "", ""},
		"/shows/Dark":                             {EntShow, "Dark", 2017, -1, 0, 0, "", ""},
		"/shows/Dark/Season 1":                    {EntSeason, "Season 1", 0, 1, 0, 0, "", "season01-poster.jpg"},
		"/shows/Dark/Season 1/Dark.S01E01.mkv":    {EntEpisode, "Episode 1", 0, 1, 1, 1, "", ""},
		"/shows/Dark/Season 1/Dark.S01E02E03.mkv": {EntEpisode, "Episode 2", 0, 1, 2, 3, "", ""},
		"/shows/Dark/Season 1/Episode 4.mkv":      {EntEpisode, "Episode 4", 0, 1, 4, 4, "", ""},
		"/shows/Dark/Dark.S02E01.mkv":             {EntEpisode, "Episode 1", 0, 2, 1, 1, "", ""},
		"/music/Agnes Obel":                       {EntArtist, "Agnes Obel", 0, -1, 0, 0, "Danish.", ""},
		"/music/Agnes Obel/Philharmonics":         {EntAlbum, "Philharmonics", 2010, -1, 0, 0, "", ""},
		"/music/Loose Album":                      {EntAlbum, "Loose Album", 0, -1, 0, 0, "", "cover.jpg"},
	} {
		got, ok := cat[p]
		if !ok {
			t.Errorf("%s: missing", p)
			continue
		}
		g := want{got.Kind, got.Title, got.Year, got.Season, got.Episode, got.EpisodeTo, got.Plot, got.Poster}
		if g != w {
			t.Errorf("%s:\n got %+v\nwant %+v", p, g, w)
		}
	}
	if len(cat) != 12 {
		t.Errorf("%d entries, want 12 (no Extras, no trailer, no Scans): %v", len(cat), keys(cat))
	}
	if a := cat["/music/Agnes Obel/Philharmonics"]; a.Tracks != 3 || a.Artist != "Agnes Obel" || a.Parent != e.ino("/music/Agnes Obel") {
		t.Errorf("album %+v", a)
	}
	if ep := cat["/shows/Dark/Dark.S02E01.mkv"]; ep.Parent != e.ino("/shows/Dark") || ep.Video != ep.Ino {
		t.Errorf("episode %+v", ep)
	}
	if m := cat["/films/Arrival (2016)"]; m.Video != e.ino("/films/Arrival (2016)/arrival.mkv") || !reflect.DeepEqual(m.Genres, []string{"Drama"}) || m.Lib != e.ino("/films") {
		t.Errorf("film %+v", m)
	}
	if m := cat["/films/Shorts/Tiny (2020)"]; m.Lib != e.ino("/films/Shorts") {
		t.Errorf("nested library entry %+v", m)
	}
}

func keys(m map[string]Entry) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
