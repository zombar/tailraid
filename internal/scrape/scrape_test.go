package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/search"
)

// stepClock is a clock whose waits return at once, the time moving on by each wait,
// and which records them.
type stepClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *stepClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }
func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

func (c *stepClock) NewTimer(d time.Duration) clock.Timer { return clock.NewFake(c.Now()).NewTimer(d) }

func (c *stepClock) total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t time.Duration
	for _, w := range c.waits {
		t += w
	}
	return t
}

// provider is a fake of the four services on one server: the TMDB responses below, a
// MusicBrainz search and a Cover Art Archive redirect to an "archive" image.
type provider struct {
	*httptest.Server
	mu       sync.Mutex
	reqs     []*http.Request
	movies   []map[string]any // /3/search/movie results
	mbAt     []time.Time
	clk      *stepClock
	override map[string]http.HandlerFunc
}

func newProvider(t *testing.T) (*provider, *Client) {
	t.Helper()
	p := &provider{clk: &stepClock{now: time.Unix(1_700_000_000, 0)}, override: map[string]http.HandlerFunc{}}
	p.movies = []map[string]any{{"id": 329865, "title": "Arrival", "original_title": "Arrival", "release_date": "2016-11-10", "overview": "Linguist."}}
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/3/configuration", func(w http.ResponseWriter, _ *http.Request) { js(w, map[string]any{}) })
	mux.HandleFunc("/3/search/movie", func(w http.ResponseWriter, _ *http.Request) { js(w, map[string]any{"results": p.movies}) })
	mux.HandleFunc("/3/movie/{id}", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{
			"id": 329865, "title": "Arrival", "original_title": "Arrival", "release_date": "2016-11-10", "overview": "A linguist.",
			"runtime": 116, "genres": []any{map[string]any{"name": "Drama"}, map[string]any{"name": "Science Fiction"}}, "vote_average": 7.6,
			"poster_path": "/p.jpg", "backdrop_path": "/b.jpg",
			"credits": map[string]any{
				"cast": []any{map[string]any{"name": "Jeremy Renner", "order": 1}, map[string]any{"name": "Amy Adams", "order": 0}},
				"crew": []any{map[string]any{"name": "Denis Villeneuve", "job": "Director"}, map[string]any{"name": "Someone", "job": "Writer"}},
			},
			"release_dates": map[string]any{"results": []any{map[string]any{"iso_3166_1": "US", "release_dates": []any{map[string]any{"certification": "PG-13"}}}}},
		})
	})
	mux.HandleFunc("/3/search/tv", func(w http.ResponseWriter, _ *http.Request) {
		js(w, map[string]any{"results": []any{map[string]any{"id": 63639, "name": "The Expanse", "original_name": "The Expanse", "first_air_date": "2015-12-14"}}})
	})
	mux.HandleFunc("/3/tv/{id}", func(w http.ResponseWriter, _ *http.Request) {
		js(w, map[string]any{
			"id": 63639, "name": "The Expanse", "first_air_date": "2015-12-14", "overview": "Belt.", "episode_run_time": []int{45},
			"genres": []any{map[string]any{"name": "Drama"}}, "vote_average": 8.1, "status": "Ended", "poster_path": "/sp.jpg", "backdrop_path": "/sb.jpg",
			"created_by": []any{map[string]any{"name": "Mark Fergus"}}, "content_ratings": map[string]any{"results": []any{map[string]any{"iso_3166_1": "US", "rating": "TV-14"}}},
		})
	})
	mux.HandleFunc("/3/tv/{id}/season/{n}", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"poster_path": "/s1.jpg", "episodes": []any{map[string]any{
			"id": 1, "episode_number": 1, "season_number": 1, "name": "Dulcinea",
			"overview": "Ice.", "air_date": "2015-12-14", "runtime": 44, "still_path": "/e1.jpg", "vote_average": 7.9,
		}}})
	})
	mux.HandleFunc("/t/p/{size}/{img}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "image:"+r.PathValue("size")+"/"+r.PathValue("img"))
	})
	mux.HandleFunc("/ws/2/release-group/", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.mbAt = append(p.mbAt, p.clk.Now())
		p.mu.Unlock()
		js(w, map[string]any{"release-groups": []any{map[string]any{
			"id": "rg-1", "title": "Around the Fur", "first-release-date": "1997-10-28",
			"artist-credit": []any{map[string]any{"name": "Deftones"}},
		}}})
	})
	mux.HandleFunc("/release-group/{id}/front-500", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/archive/"+r.PathValue("id")+".jpg", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/archive/{img}", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "cover:"+r.PathValue("img")) })
	// the libretro thumbnail set: an Apache index of a system's box art, and the images
	mux.HandleFunc("/libretro/{system}/Named_Boxarts/{$}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><table><tr><td><a href="?C=N;O=D">Name</a></td></tr><tr><td><a href="/Sony%20-%20PlayStation/">Parent Directory</a></td></tr>`)
		for _, n := range []string{"Final Fantasy VII (USA) (Disc 1)", "Final Fantasy VII (Europe) (Disc 1)", "Super Mario World (USA)", "Mega Man X (USA)", "Mega Man X (Japan)", "Q_bert (USA)"} {
			fmt.Fprintf(w, `<tr><td><a href="%s.png">%s.png</a></td></tr>`, url.PathEscape(n), n)
		}
		fmt.Fprint(w, `</table></body></html>`)
	})
	mux.HandleFunc("/libretro/{system}/Named_Boxarts/{img}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "boxart:"+r.PathValue("system")+"/"+r.PathValue("img"))
	})
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.reqs = append(p.reqs, r)
		h := p.override[r.URL.Path]
		p.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	c := New("k3y-secret", "test")
	c.Clock = p.clk
	base := strings.Replace(p.URL, "127.0.0.1", "localhost", 1)
	c.TMDB, c.Images, c.MB, c.CAA, c.Libretro = base, base, base, base, base+"/libretro"
	c.Hosts = []string{"localhost"}
	return p, c
}

// --- matching (I-301)

// Invariant: I-301
func TestClear_Table(t *testing.T) {
	arrival := Candidate{ID: "1", Title: "Arrival", Year: 2016}
	for _, tc := range []struct {
		name  string
		title string
		year  int
		cs    []Candidate
		want  int
	}{
		{"one equal", "Arrival", 2016, []Candidate{arrival}, 0},
		{"punctuation and case", "arrival.", 2016, []Candidate{arrival}, 0},
		{"accents fold", "Amelie", 2001, []Candidate{{ID: "2", Title: "Amélie", Year: 2001}}, 0},
		{"original title", "Le Fabuleux Destin", 2001, []Candidate{{ID: "2", Title: "Amélie", Original: "Le fabuleux destin", Year: 2001}}, 0},
		{"year differs", "Arrival", 2015, []Candidate{arrival}, -1},
		{"no year: the one equal title", "Arrival", 0, []Candidate{arrival, {ID: "3", Title: "Arrivals"}}, 0},
		{"two equal", "Dune", 0, []Candidate{{ID: "4", Title: "Dune", Year: 1984}, {ID: "5", Title: "Dune", Year: 2021}}, -1},
		{"year breaks the tie", "Dune", 2021, []Candidate{{ID: "4", Title: "Dune", Year: 1984}, {ID: "5", Title: "Dune", Year: 2021}}, 1},
		{"title differs", "Arrival", 2016, []Candidate{{ID: "6", Title: "The Arrival", Year: 2016}}, -1},
		{"none", "Arrival", 2016, nil, -1},
		{"empty title", "", 0, []Candidate{{ID: "7", Title: ""}}, -1},
	} {
		i, ok := Clear(tc.title, tc.year, tc.cs)
		if i != tc.want || ok != (tc.want >= 0) {
			t.Errorf("%s: Clear = %d %v, want %d", tc.name, i, ok, tc.want)
		}
	}
}

// Invariant: I-301
func TestClearAlbum_Table(t *testing.T) {
	cs := []Candidate{{ID: "a", Title: "Around the Fur", Artist: "Deftones"}, {ID: "b", Title: "Around the Fur", Artist: "Tribute Band"}}
	for _, tc := range []struct {
		artist, title string
		want          int
	}{
		{"Deftones", "Around the Fur", 0}, {"deftones", "around the fur", 0}, {"", "Around the Fur", -1}, {"Deftones", "White Pony", -1},
	} {
		if i, _ := ClearAlbum(tc.artist, tc.title, 0, cs); i != tc.want {
			t.Errorf("ClearAlbum(%q, %q) = %d, want %d", tc.artist, tc.title, i, tc.want)
		}
	}
}

// A game's box art is its own name when the set has it, else the only name of its title,
// else the only one of those tagged with one of its regions.
// Invariant: I-301
func TestClearGame_Table(t *testing.T) {
	names := []string{"Super Mario World (USA)", "Super Mario World (Europe) (Rev 1)", "Mega Man X (USA)", "Mega Man X (Japan)", "Mega Man X (USA) (Rev 1)", "Q_bert (USA)", "Zoda (World)"}
	for _, tc := range []struct {
		key, title string
		regions    []string
		want       string // "" none clear
		cands      int
	}{
		{"Super Mario World (USA)", "Super Mario World", []string{"USA"}, "Super Mario World (USA)", 1},
		{"SMW", "Super Mario World", []string{"Europe"}, "Super Mario World (Europe) (Rev 1)", 2},
		{"Q*bert (USA)", "Q*bert", []string{"USA"}, "Q_bert (USA)", 1},
		{"Zoda", "Zoda", nil, "Zoda (World)", 1},
		{"megaman x", "Mega Man X", []string{"Japan"}, "Mega Man X (Japan)", 3},
		{"megaman x", "Mega Man X", []string{"USA"}, "", 3},
		{"megaman x", "Mega Man X", nil, "", 3},
		{"Unknown (USA)", "Unknown", []string{"USA"}, "", 0},
	} {
		cs, i, ok := ClearGame(tc.key, tc.title, tc.regions, names)
		got := ""
		if ok {
			got = cs[i].ID
		}
		if got != tc.want || len(cs) != tc.cands {
			t.Errorf("%q: %q with %d candidates, want %q with %d", tc.key, got, len(cs), tc.want, tc.cands)
		}
	}
}

// Without a TMDB key a run keeps the keyless groups: every group asked narrows to music
// and games, and asking only for TMDB's is ErrNoKey.
func TestWithoutKey_Table(t *testing.T) {
	for _, tc := range []struct {
		only    []string
		want    []string
		dropped int
		err     bool
	}{
		{nil, []string{OnlyGames, OnlyMusic}, 3, false},
		{[]string{OnlyMovies, OnlyGames}, []string{OnlyGames}, 1, false},
		{[]string{OnlyMusic}, []string{OnlyMusic}, 0, false},
		{[]string{OnlyMovies, OnlyTV}, nil, 2, true},
	} {
		in := map[string]bool{}
		for _, g := range tc.only {
			in[g] = true
		}
		got, dropped, err := WithoutKey(in)
		var names []string
		for g := range got {
			names = append(names, g)
		}
		sort.Strings(names)
		if (err != nil) != tc.err || !reflect.DeepEqual(names, tc.want) || len(dropped) != tc.dropped || tc.err && !errors.Is(err, ErrNoKey) {
			t.Errorf("%v: %v dropped %v err %v", tc.only, names, dropped, err)
		}
	}
}

// --- sidecars (I-300)

// What a scrape writes, the catalogue's reader takes back field for field.
// Invariant: I-300
func TestNFO_RoundTrip_Table(t *testing.T) {
	actors := []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7", "A8"}
	for _, m := range []Meta{
		{
			Root: "movie", TMDB: 329865, Title: "Arrival", Original: "Arrival (orig)", Year: 2016, Premiered: "2016-11-10", Plot: "A linguist & <aliens>.",
			Runtime: 116, Genres: []string{"Drama", "Science Fiction"}, Rating: 7.62, MPAA: "PG-13", Directors: []string{"Denis Villeneuve"}, Actors: actors, Season: -1, Episode: -1,
		},
		{Root: "tvshow", TMDB: 63639, Title: "The Expanse", Year: 2015, Premiered: "2015-12-14", Plot: "Belt.", Genres: []string{"Drama"}, Rating: 8.1, MPAA: "TV-14", Status: "Ended", Season: -1, Episode: -1},
		{Root: "episodedetails", TMDB: 1, Title: "Dulcinea", Year: 2015, Aired: "2015-12-14", Plot: "Ice.", Runtime: 44, Rating: 7.9, Season: 1, Episode: 1},
		{Root: "episodedetails", Title: "Specials", Season: 0, Episode: 3},
	} {
		n, ok := search.DecodeNFO(NFO(m))
		if !ok {
			t.Fatalf("%s: the catalogue does not read the sidecar:\n%s", m.Root, NFO(m))
		}
		people := append(append([]string{}, m.Directors...), m.Actors[:min(6, len(m.Actors))]...)
		wantRating := ""
		if m.Rating > 0 {
			wantRating = fmt.Sprintf("%.1f", m.Rating)
		}
		want := search.NFO{
			Root: m.Root, Title: m.Title, Original: m.Original, Year: m.Year, Plot: m.Plot, Genres: m.Genres, Rating: wantRating,
			Runtime: m.Runtime, MPAA: m.MPAA, People: people, Directors: len(m.Directors), Season: m.Season, Episode: m.Episode, Aired: m.Aired, Status: m.Status,
		}
		if len(want.People) == 0 {
			want.People = nil
		}
		if len(want.Genres) == 0 {
			want.Genres = nil
		}
		if !reflect.DeepEqual(n, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", m.Root, n, want)
		}
		if m.TMDB != 0 && !strings.Contains(string(NFO(m)), fmt.Sprintf(`<uniqueid type="tmdb" default="true">%d</uniqueid>`, m.TMDB)) {
			t.Errorf("%s: no tmdb uniqueid", m.Root)
		}
	}
}

// --- the client (I-302)

// The key goes only to TMDB (as api_key, or as a bearer token for a v4 token), every
// request carries the user agent, and no error names the key.
// Invariant: I-302
func TestClient_KeyOnlyToTMDB_UserAgent_Redacted(t *testing.T) {
	ctx := context.Background()
	p, c := newProvider(t)
	if _, err := c.SearchMovie(ctx, "Arrival", 2016, "en-US"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchAlbum(ctx, "Deftones", "Around the Fur"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Image(ctx, c.Images+"/t/p/w780/p.jpg"); err != nil {
		t.Fatal(err)
	}
	for _, r := range p.reqs {
		tmdb := strings.HasPrefix(r.URL.Path, "/3/")
		if got := r.URL.Query().Get("api_key"); tmdb != (got == "k3y-secret") {
			t.Errorf("%s: api_key %q", r.URL.Path, got)
		}
		if r.Header.Get("User-Agent") != "tailraid/test (+https://github.com/zombar/tailraid)" {
			t.Errorf("%s: user agent %q", r.URL.Path, r.Header.Get("User-Agent"))
		}
	}
	// a v4 token travels as a bearer token to TMDB only
	c.Key = "eyJhbGciOiJIUzI1NiJ9.token"
	p.reqs = nil
	_, _ = c.SearchMovie(ctx, "Arrival", 0, "en-US")
	_, _ = c.Image(ctx, c.Images+"/t/p/w780/p.jpg")
	for _, r := range p.reqs {
		tmdb := strings.HasPrefix(r.URL.Path, "/3/")
		if (r.Header.Get("Authorization") == "Bearer "+c.Key) != tmdb || r.URL.Query().Has("api_key") {
			t.Errorf("%s: authorization %q, query %q", r.URL.Path, r.Header.Get("Authorization"), r.URL.RawQuery)
		}
	}
	// a transport error carries the URL: the key is not in it
	c.Key = "k3y-secret"
	c.TMDB = "http://localhost:1"
	_, err := c.SearchMovie(ctx, "Arrival", 0, "en-US")
	if err == nil || strings.Contains(err.Error(), "k3y-secret") {
		t.Errorf("error %v names the key", err)
	}
}

// Only the provider hosts are asked, a redirect elsewhere included.
// Invariant: I-302
func TestClient_ProviderHostsOnly(t *testing.T) {
	ctx := context.Background()
	p, c := newProvider(t)
	if _, err := c.Image(ctx, strings.Replace(p.URL, "localhost", "127.0.0.1", 1)+"/t/p/w780/p.jpg"); !errors.Is(err, ErrHost) {
		t.Errorf("another host: %v, want ErrHost", err)
	}
	elsewhere := p.URL // 127.0.0.1: not in Hosts
	p.override["/release-group/rg-x/front-500"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere+"/archive/x.jpg", http.StatusTemporaryRedirect)
	}
	if _, err := c.Image(ctx, c.CoverURL("rg-x")); !errors.Is(err, ErrHost) {
		t.Errorf("a redirect to another host: %v, want ErrHost", err)
	}
	if b, err := c.Image(ctx, c.CoverURL("rg-1")); err != nil || string(b) != "cover:rg-1.jpg" {
		t.Errorf("the archive's redirect: %q %v", b, err)
	}
	if !reflect.DeepEqual(New("", "v").Hosts, []string{"api.themoviedb.org", "image.tmdb.org", "musicbrainz.org", "coverartarchive.org", "archive.org", ".archive.org", "thumbnails.libretro.com"}) {
		t.Error("the default hosts are not the providers, the archive and the thumbnail set")
	}
}

// A 429 is retried after its Retry-After at most three times; a body past its bound is
// refused.
// Invariant: I-302
func TestClient_429Retries_BoundedBody(t *testing.T) {
	ctx := context.Background()
	p, c := newProvider(t)
	calls := 0
	p.override["/3/search/tv"] = func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	_, err := c.SearchTV(ctx, "x", 0, "en-US")
	if err == nil || calls != MaxRetries+1 || p.clk.total() != time.Duration(MaxRetries)*2*time.Second {
		t.Errorf("err %v, %d calls, waited %v; want an error after %d calls and %v", err, calls, p.clk.total(), MaxRetries+1, time.Duration(MaxRetries)*2*time.Second)
	}
	p.override["/3/search/movie"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"pad":"` + strings.Repeat("x", MaxJSON) + `"}`))
	}
	if _, err := c.SearchMovie(ctx, "x", 0, "en-US"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an oversized body: %v, want ErrTooLarge", err)
	}
	p.override["/3/configuration"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }
	if err := c.CheckKey(ctx); !errors.Is(err, ErrKey) {
		t.Errorf("a refused key: %v, want ErrKey", err)
	}
}

// MusicBrainz is asked at most once a second.
// Invariant: I-302
func TestClient_MusicBrainzOncePerSecond(t *testing.T) {
	ctx := context.Background()
	p, c := newProvider(t)
	for range 4 {
		if _, err := c.SearchAlbum(ctx, "Deftones", "Around the Fur"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(p.mbAt); i++ {
		if d := p.mbAt[i].Sub(p.mbAt[i-1]); d < MBInterval {
			t.Errorf("requests %d and %d %v apart", i-1, i, d)
		}
	}
}

// The TMDB key is needed, and so checked before a run, exactly when a keyed group is
// in the run: every group, or movies, tv or episodes by name.
// Invariant: I-308
func TestNeedsKey_Table(t *testing.T) {
	for _, tc := range []struct {
		only map[string]bool
		want bool
	}{
		{nil, true},
		{map[string]bool{}, true},
		{map[string]bool{OnlyMovies: true}, true},
		{map[string]bool{OnlyMusic: true, OnlyGames: true}, false},
		{map[string]bool{OnlyMusic: true, OnlyTV: true}, true},
		{map[string]bool{OnlyEpisodes: true}, true},
	} {
		if got := NeedsKey(tc.only); got != tc.want {
			t.Errorf("NeedsKey(%v) = %v", tc.only, got)
		}
	}
}

// --- the run (I-299, I-301)

type memWriter struct {
	files  map[string][]byte
	exists map[string]bool
}

func (w *memWriter) Create(_ context.Context, p string, b []byte) error {
	if w.exists[p] || w.files[p] != nil {
		return ErrExist
	}
	w.files[p] = b
	return nil
}

type pickChooser struct {
	pick  int
	asked int
}

func (c *pickChooser) Choose(search.ScrapeItem, []Candidate) Choice {
	c.asked++
	return Choice{Pick: c.pick}
}

func plan() []search.ScrapeItem {
	return []search.ScrapeItem{
		{
			Kind: search.ScrapeMovie, Ino: 10, Dir: "/films/Arrival (2016)", Title: "Arrival", Year: 2016, Season: -1,
			Missing: []search.Slot{{Slot: "nfo", Name: "movie.nfo"}, {Slot: "poster", Name: "poster.jpg"}, {Slot: "fanart", Name: "fanart.jpg"}},
		},
		{Kind: search.ScrapeShow, Ino: 20, Dir: "/tv/The Expanse", Title: "The Expanse", Season: -1},
		{Kind: search.ScrapeSeason, Ino: 21, Show: 20, Dir: "/tv/The Expanse", Season: 1, Missing: []search.Slot{{Slot: "poster", Name: "season01-poster.jpg"}}},
		{
			Kind: search.ScrapeEpisode, Ino: 22, Show: 20, Dir: "/tv/The Expanse/Season 01", Season: 1, Episode: 1,
			Missing: []search.Slot{{Slot: "nfo", Name: "The.Expanse.S01E01.nfo"}, {Slot: "thumb", Name: "The.Expanse.S01E01-thumb.jpg"}},
		},
		{Kind: search.ScrapeEpisode, Ino: 23, Show: 20, Dir: "/tv/The Expanse/Season 01", Season: 1, Episode: 9, Missing: []search.Slot{{Slot: "nfo", Name: "x.nfo"}}},
		{
			Kind: search.ScrapeAlbum, Ino: 30, Dir: "/music/Deftones/Around the Fur", Title: "Around the Fur", Artist: "Deftones", Season: -1,
			Missing: []search.Slot{{Slot: "cover", Name: "cover.jpg"}},
		},
		{
			Kind: search.ScrapeGame, Ino: 40, Dir: "/roms/snes", Title: "Super Mario World", Key: "Super Mario World (USA)", System: "Nintendo - Super Nintendo Entertainment System",
			Regions: []string{"USA"}, Season: -1, Missing: []search.Slot{{Slot: "boxart", Name: "Super Mario World (USA).png"}},
		},
		{
			Kind: search.ScrapeGame, Ino: 41, Dir: "/roms/psx/Final Fantasy VII (USA)", Title: "Final Fantasy VII", Key: "Final Fantasy VII (USA)", System: "Sony - PlayStation",
			Regions: []string{"USA"}, Season: -1, Missing: []search.Slot{{Slot: "boxart", Name: "boxart.png"}},
		},
	}
}

// A run writes each missing slot once, as an exclusive create: a name that exists by
// then counts as present and is not replaced; dry, it writes nothing.
// Invariant: I-299
func TestRun_WritesMissingSlotsOnly(t *testing.T) {
	ctx := context.Background()
	_, c := newProvider(t)
	w := &memWriter{files: map[string][]byte{}, exists: map[string]bool{"/films/Arrival (2016)/fanart.jpg": true}}
	sum, err := Run(ctx, c, plan(), w, nil, Options{Lang: "en-US", Region: "US"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/films/Arrival (2016)/movie.nfo":                        "<movie>",
		"/films/Arrival (2016)/poster.jpg":                       "image:w780/p.jpg",
		"/tv/The Expanse/season01-poster.jpg":                    "image:w780/s1.jpg",
		"/tv/The Expanse/Season 01/The.Expanse.S01E01.nfo":       "<episodedetails>",
		"/tv/The Expanse/Season 01/The.Expanse.S01E01-thumb.jpg": "image:w300/e1.jpg",
		"/music/Deftones/Around the Fur/cover.jpg":               "cover:rg-1.jpg",
		"/roms/snes/Super Mario World (USA).png":                 "boxart:Nintendo - Super Nintendo Entertainment System/Super Mario World (USA).png",
		"/roms/psx/Final Fantasy VII (USA)/boxart.png":           "boxart:Sony - PlayStation/Final Fantasy VII (USA) (Disc 1).png",
	}
	if len(w.files) != len(want) {
		t.Errorf("wrote %d files, want %d: %v", len(w.files), len(want), keys(w.files))
	}
	for p, sub := range want {
		if !strings.Contains(string(w.files[p]), sub) {
			t.Errorf("%s: %.80q, want it to contain %q", p, w.files[p], sub)
		}
	}
	if sum.Present != 1 || sum.Files != len(want) || len(sum.Skipped) != 1 || !strings.Contains(sum.Skipped[0].Why, "no such episode") {
		t.Errorf("summary %+v", sum)
	}
	dry := &memWriter{files: map[string][]byte{}}
	if _, err := Run(ctx, c, plan(), dry, nil, Options{DryRun: true}); err != nil || len(dry.files) != 0 {
		t.Errorf("dry run wrote %v (%v)", keys(dry.files), err)
	}
}

// An unclear search is the user's to choose: skipped without a terminal, the top result
// with --yes, the chosen one from the chooser, the named one with --id.
// Invariant: I-301
func TestRun_UnclearMatch_Table(t *testing.T) {
	ctx := context.Background()
	film := plan()[:1]
	for _, tc := range []struct {
		name    string
		ch      *pickChooser
		o       Options
		written bool
		asked   int
	}{
		{name: "no terminal", written: false},
		{name: "yes", o: Options{Yes: true}, written: true},
		{name: "chooser", ch: &pickChooser{pick: 1}, written: true, asked: 1},
		{name: "id", o: Options{ID: "tmdb:777"}, written: true},
	} {
		p, c := newProvider(t)
		p.movies = []map[string]any{
			{"id": 1, "title": "Arrival", "release_date": "2016-01-01"}, {"id": 2, "title": "Arrival", "release_date": "2016-06-01"},
		}
		w := &memWriter{files: map[string][]byte{}}
		var ch Chooser
		if tc.ch != nil {
			ch = tc.ch
		}
		sum, err := Run(ctx, c, film, w, ch, tc.o)
		if err != nil {
			t.Fatal(err)
		}
		if (len(w.files) > 0) != tc.written || tc.ch != nil && tc.ch.asked != tc.asked {
			t.Errorf("%s: wrote %v, summary %+v", tc.name, keys(w.files), sum)
		}
		if !tc.written && (len(sum.Skipped) != 1 || !strings.Contains(sum.Skipped[0].Why, "no clear match")) {
			t.Errorf("%s: skipped %+v", tc.name, sum.Skipped)
		}
	}
	// --id names one item only
	_, c := newProvider(t)
	if _, err := Run(ctx, c, plan(), &memWriter{files: map[string][]byte{}}, nil, Options{ID: "tmdb:1"}); err == nil {
		t.Error("--id over several items was accepted")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- the boundary (I-298)

// No package but the CLI imports this one: a node never contacts a provider.
// Invariant: I-298
func TestNoNodePackageImportsScrape(t *testing.T) {
	const self = "github.com/zombar/tailraid/internal/scrape"
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "vendor" || d.Name() == "third_party" || strings.HasPrefix(d.Name(), ".")) && p != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if strings.HasPrefix(rel, filepath.Join("internal", "scrape")) || strings.HasPrefix(rel, filepath.Join("cmd", "tailraid")+string(filepath.Separator)) {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == self {
				t.Errorf("%s imports the scrape client", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
