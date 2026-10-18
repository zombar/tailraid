package search

import (
	"reflect"
	"testing"
)

// Titles and years from names: `Title (2019)`, scene names cut at a year, an episode
// number or a release token, brackets and dots handled.
// Invariant: I-211
func TestCatalog_CleanName_Table(t *testing.T) {
	for _, tc := range []struct {
		in    string
		title string
		year  int
	}{
		{"Arrival (2016)", "Arrival", 2016},
		{"Arrival [2016]", "Arrival", 2016},
		{"Blade.Runner.2049.2017.1080p.BluRay.x264-GROUP", "Blade Runner 2049", 2017},
		{"The.Matrix.1999.REMUX.2160p", "The Matrix", 1999},
		{"Stalker 1080p x265", "Stalker", 0},
		{"[Group] Spirited Away [ABCD1234]", "Spirited Away", 0},
		{"some_movie_name", "some movie name", 0},
		{"Mr. Robot", "Mr. Robot", 0},
		{"1917", "1917", 0},
		{"2001 A Space Odyssey (1968)", "2001 A Space Odyssey", 1968},
		{"Show.Name.S01E02.720p.HDTV", "Show Name", 0},
		// season packs as release folders name them (the real tailnet, 2026-10-12)
		{"Invasion.2021.S01.PROPER.WEBRip.x264-ION10", "Invasion", 2021},
		{"Pluribus.S01.1080p.x265-ELiTE", "Pluribus", 0},
		{"Severance 2022 Seasons 1 and 2 Complete 1080p WEB x264 [i_c]", "Severance", 2022},
		{"Star Trek Starfleet Academy S01e01-10 (1080p Ita Eng Spa h265 10bit SubS) byMe7alh", "Star Trek Starfleet Academy", 0},
		{"Star.Trek.Strange.New.Worlds.S01.ITA.ENG.1080p.AMZN.WEBRip.AAC.x265-Pir8", "Star Trek Strange New Worlds", 0},
		{"Show Name Season 2", "Show Name", 0},
		{"Dune Part Two 2024 Extended Cut", "Dune Part Two", 2024},
		// a season word only cuts before its number, a year only after the first token
		{"The Four Seasons 1990 1080p", "The Four Seasons", 1990},
		{"Season of the Witch", "Season of the Witch", 0},
		{"2012 2009 1080p", "2012", 2009},
		{"", "", 0},
	} {
		title, year := CleanName(tc.in)
		if title != tc.title || year != tc.year {
			t.Errorf("CleanName(%q) = %q, %d; want %q, %d", tc.in, title, year, tc.title, tc.year)
		}
	}
	if SortTitle("The Matrix") != "matrix" || SortTitle("A") != "a" || SortTitle("Arrival") != "arrival" {
		t.Fatal("sort titles drop a leading article and fold")
	}
}

// Episode numbers: SnnEnn, a double episode, nnxnn, Episode nn, none; season folders.
// Invariant: I-211
func TestCatalog_Episode_Table(t *testing.T) {
	for _, tc := range []struct {
		in                  string
		season, episode, to int
		ok                  bool
	}{
		{"Show.S01E02.720p", 1, 2, 2, true},
		{"show s1e9", 1, 9, 9, true},
		{"Show - S02E01E02 - Pilot", 2, 1, 2, true},
		{"Show S03E05-E06", 3, 5, 6, true},
		{"Show 4x07 Title", 4, 7, 7, true},
		{"Episode 12", -1, 12, 12, true},
		{"Show 1080p", 0, 0, 0, false},
		{"Show 2019", 0, 0, 0, false},
	} {
		s, e, to, ok := ParseEpisode(tc.in)
		if s != tc.season || e != tc.episode || to != tc.to || ok != tc.ok {
			t.Errorf("ParseEpisode(%q) = %d %d %d %v", tc.in, s, e, to, ok)
		}
	}
	for name, want := range map[string]int{"Season 1": 1, "season 02": 2, "Series 3": 3, "Specials": 0, "S01": -1, "Extras": -1} {
		n, ok := SeasonOf(name)
		if (want < 0) == ok || (ok && n != want) {
			t.Errorf("SeasonOf(%q) = %d %v", name, n, ok)
		}
	}
}

// Kodi sidecars: movie, show, episode and artist fields; a bare URL, truncated XML and
// an unknown root contribute nothing.
// Invariant: I-211
func TestCatalog_NFO_Table(t *testing.T) {
	movie := `<?xml version="1.0" encoding="UTF-8"?>
<movie><title>Arrival</title><year>2016</year><plot>Linguist meets heptapods.</plot><runtime>116</runtime>
<genre>Drama</genre><genre>Sci-Fi / Mystery</genre><ratings><rating name="imdb" default="true"><value>7.9</value></rating></ratings>
<mpaa>PG-13</mpaa><director>Denis Villeneuve</director>
<actor><name>Amy Adams</name></actor><actor><name>Jeremy Renner</name></actor></movie>
https://www.themoviedb.org/movie/329865`
	n, ok := DecodeNFO([]byte(movie))
	want := NFO{
		Root: "movie", Title: "Arrival", Year: 2016, Plot: "Linguist meets heptapods.", Runtime: 116, Genres: []string{"Drama", "Sci-Fi", "Mystery"},
		Rating: "7.9", MPAA: "PG-13", People: []string{"Denis Villeneuve", "Amy Adams", "Jeremy Renner"}, Directors: 1, Season: -1, Episode: -1,
	}
	if !ok || !reflect.DeepEqual(n, want) {
		t.Fatalf("movie\n got %+v\nwant %+v", n, want)
	}
	n, ok = DecodeNFO([]byte(`<tvshow><title>Dark</title><premiered>2017-12-01</premiered><rating>8.7</rating><status>Ended</status><outline>Time travel.</outline></tvshow>`))
	if !ok || n.Title != "Dark" || n.Year != 2017 || n.Rating != "8.7" || n.Status != "Ended" || n.Plot != "Time travel." {
		t.Fatalf("tvshow %+v", n)
	}
	n, ok = DecodeNFO([]byte(`<episodedetails><title>Secrets</title><season>1</season><episode>1</episode><aired>2017-12-01</aired></episodedetails>`))
	if !ok || n.Season != 1 || n.Episode != 1 || n.Aired != "2017-12-01" {
		t.Fatalf("episode %+v", n)
	}
	n, ok = DecodeNFO([]byte(`<artist><name>Agnes Obel</name><biography>Danish singer.</biography></artist>`))
	if !ok || n.Biography != "Danish singer." {
		t.Fatalf("artist %+v", n)
	}
	for _, bad := range []string{"https://www.themoviedb.org/movie/329865", "<movie><title>Arr", "<html><title>x</title></html>", "", "\xff\xfe"} {
		if _, ok := DecodeNFO([]byte(bad)); ok {
			t.Fatalf("%q decoded", bad)
		}
	}
}

// Invariant: I-211
func FuzzNFODecode(f *testing.F) {
	f.Add([]byte(`<movie><title>A</title><year>2001</year></movie>`))
	f.Add([]byte(`<tvshow><ratings><rating default="true"><value>9</value></rating></ratings></tvshow>`))
	f.Fuzz(func(t *testing.T, b []byte) {
		n, ok := DecodeNFO(b)
		if !ok {
			return
		}
		if n.Year < 0 || n.Runtime < 0 || len(n.People) > 64+6 && n.Root == "" {
			t.Fatalf("%+v", n)
		}
	})
}
