package search

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// scrapeEnv is a volume with a library of each kind: some items bare, some with every
// sidecar under one of the other names the catalogue accepts.
func scrapeEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.library("/films", meta.LibraryMovies)
	e.library("/tv", meta.LibraryTV)
	e.library("/music", meta.LibraryMusic)
	video := []byte("not really a video")
	nfo := func(root string) []byte { return []byte("<" + root + "><title>x</title></" + root + ">") }
	img := []byte("jpeg")
	// a bare film folder, a complete one under the other names, a loose file
	e.mkdir("/films/Arrival (2016)")
	e.write("/films/Arrival (2016)/Arrival.2016.1080p.mkv", video)
	e.mkdir("/films/Moon (2009)")
	e.write("/films/Moon (2009)/Moon.mkv", video)
	e.write("/films/Moon (2009)/Moon.nfo", nfo("movie"))
	e.write("/films/Moon (2009)/folder.jpg", img)
	e.write("/films/Moon (2009)/Moon-fanart.png", img)
	e.write("/films/Heat.1995.mkv", video)
	// a complete show whose first season lacks its poster and an episode its sidecars
	e.mkdir("/tv/The Expanse/Season 01")
	e.mkdir("/tv/The Expanse/Season 02")
	e.write("/tv/The Expanse/tvshow.nfo", nfo("tvshow"))
	e.write("/tv/The Expanse/poster.jpg", img)
	e.write("/tv/The Expanse/fanart.jpg", img)
	e.write("/tv/The Expanse/Season 01/The.Expanse.S01E01.mkv", video)
	e.write("/tv/The Expanse/Season 02/poster.jpg", img)
	e.write("/tv/The Expanse/Season 02/The.Expanse.S02E01.mkv", video)
	e.write("/tv/The Expanse/Season 02/The.Expanse.S02E01.nfo", nfo("episodedetails"))
	e.write("/tv/The Expanse/Season 02/The.Expanse.S02E01-thumb.jpg", img)
	// an album without a cover and one with
	e.mkdir("/music/Deftones/Around the Fur")
	e.write("/music/Deftones/Around the Fur/01 - My Own Summer.mp3", id3(0, "TIT2", "My Own Summer"))
	e.mkdir("/music/Deftones/White Pony")
	e.write("/music/Deftones/White Pony/01 - Feiticeira.mp3", id3(0, "TIT2", "Feiticeira"))
	e.write("/music/Deftones/White Pony/cover.jpg", img)
	return e
}

// planOf is the plan as "kind dir: slot=name …" lines, each item's dir resolved.
func planOf(t *testing.T, e *env, s *Service) []string {
	t.Helper()
	var out []string
	plan := s.ScrapePlan(context.Background())
	for i := range plan {
		it := &plan[i]
		parts, err := e.f.FSM.PathOf(it.DirIno)
		if err != nil {
			t.Fatal(err)
		}
		line := it.Kind + " /" + strings.Join(parts, "/") + ":"
		for _, m := range it.Missing {
			line += " " + m.Slot + "=" + m.Name
		}
		out = append(out, line)
	}
	return out
}

// A slot is missing exactly when no name the catalogue reads for it exists, a show is
// listed for its season and episode, and the names a scrape writes are the first ones the
// catalogue reads: once they exist, the plan is empty.
// Invariant: I-299, I-300
func TestScrapePlan_Slots(t *testing.T) {
	e := scrapeEnv(t)
	want := []string{
		"movie /films/Arrival (2016): nfo=movie.nfo poster=poster.jpg fanart=fanart.jpg",
		"movie /films: nfo=Heat.1995.nfo poster=Heat.1995-poster.jpg fanart=Heat.1995-fanart.jpg",
		"show /tv/The Expanse:",
		"season /tv/The Expanse: poster=season01-poster.jpg",
		"episode /tv/The Expanse/Season 01: nfo=The.Expanse.S01E01.nfo thumb=The.Expanse.S01E01-thumb.jpg",
		"album /music/Deftones/Around the Fur: cover=cover.jpg",
	}
	got := planOf(t, e, e.svc)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan:\n got %q\nwant %q", got, want)
	}

	// write every missing name (an .nfo as its kind's Kodi root): nothing is missing then
	roots := map[string]string{ScrapeMovie: "movie", ScrapeShow: "tvshow", ScrapeEpisode: "episodedetails"}
	plan := e.svc.ScrapePlan(context.Background())
	for i := range plan {
		it := &plan[i]
		parts, _ := e.f.FSM.PathOf(it.DirIno)
		for _, m := range it.Missing {
			data := []byte("img")
			if m.Slot == "nfo" {
				data = []byte(fmt.Sprintf("<%s><title>t</title></%s>", roots[it.Kind], roots[it.Kind]))
			}
			e.write(path.Join("/", path.Join(parts...), m.Name), data)
		}
	}
	if left := planOf(t, e, e.service()); len(left) != 0 {
		t.Errorf("after writing every missing name the plan still lists %q", left)
	}
}

// Two nodes over the same volume give the same plan.
// Invariant: I-299
func TestScrapePlan_SameOnEveryNode(t *testing.T) {
	e := scrapeEnv(t)
	a, b := planOf(t, e, e.svc), planOf(t, e, e.service())
	if !reflect.DeepEqual(a, b) || len(a) == 0 {
		t.Fatalf("plans differ:\n%q\n%q", a, b)
	}
}

// The plan carries what a provider is asked with: the analysed title and year, a
// season's and an episode's numbers and show, an album's first track.
func TestScrapePlan_ItemFields(t *testing.T) {
	e := scrapeEnv(t)
	byKind := map[string]ScrapeItem{}
	plan := e.svc.ScrapePlan(context.Background())
	for i := range plan {
		if _, seen := byKind[plan[i].Kind]; !seen {
			byKind[plan[i].Kind] = plan[i]
		}
	}
	if m := byKind[ScrapeMovie]; m.Title != "Arrival" || m.Year != 2016 {
		t.Errorf("movie: %+v", m)
	}
	show := byKind[ScrapeShow]
	if show.Title == "" || show.Ino == 0 {
		t.Errorf("show: %+v", show)
	}
	if s := byKind[ScrapeSeason]; s.Season != 1 || s.Show != show.Ino {
		t.Errorf("season: %+v", s)
	}
	if ep := byKind[ScrapeEpisode]; ep.Season != 1 || ep.Episode != 1 || ep.Show != show.Ino {
		t.Errorf("episode: %+v", ep)
	}
	if al := byKind[ScrapeAlbum]; al.Track == 0 || al.Title != "Around the Fur" {
		t.Errorf("album: %+v", al)
	}
}
