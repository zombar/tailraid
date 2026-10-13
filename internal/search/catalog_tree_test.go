package search

import (
	"context"
	"fmt"
	"reflect"
	"strings"
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

// A music library kept the way a real one is: albums under category folders (`1 Studio`,
// `Studio Albums`), an album of disc folders (`D1`, `CD 2`), an album kept as a format
// edition (`MP3 CBR Original`); every album is listed under its artist with the name
// analyser's title, year and edition, and no disc, category or format folder is an
// album. Before, the album rule took `D1` for the album and missed the albums under
// `1 Studio`.
// Invariant: I-276
func TestCatalog_MusicRoles_Table(t *testing.T) {
	e := newEnv(t)
	e.library("/music", meta.LibraryMusic)
	for _, f := range []string{
		"/music/YES/1 Studio/1983 90125 (Remastered and Expanded) @320/01 Owner.mp3",
		"/music/YES/1 Studio/1983 90125 (Remastered and Expanded) @320/02 Hold On.mp3",
		"/music/Genesis/1974 The Lamb Lies Down On Broadway/D1/01 The Lamb.mp3",
		"/music/Genesis/1974 The Lamb Lies Down On Broadway/D2/01 The Waiting Room.mp3",
		"/music/Marillion/1989 - Seasons End/1989 - MP3 CBR Original/01 The King of Sunset Town.mp3",
		"/music/Simply Red/Studio Albums/[1995] Life/01 Fairground.mp3",
		"/music/Oasis/Oasis - 1995 - (What's The Story) Morning Glory/01 Hello.mp3",
	} {
		e.write(f, []byte("a"))
	}
	e.pass()
	cat := e.catalog()
	type want struct {
		title          string
		year, discs    int
		edition, under string
	}
	for p, w := range map[string]want{
		"/music/YES/1 Studio/1983 90125 (Remastered and Expanded) @320": {"90125", 1983, 0, "Remastered and Expanded", "/music/YES"},
		"/music/Genesis/1974 The Lamb Lies Down On Broadway":            {"The Lamb Lies Down On Broadway", 1974, 2, "", "/music/Genesis"},
		"/music/Marillion/1989 - Seasons End":                           {"Seasons End", 1989, 0, "", "/music/Marillion"},
		"/music/Simply Red/Studio Albums/[1995] Life":                   {"Life", 1995, 0, "", "/music/Simply Red"},
		"/music/Oasis/Oasis - 1995 - (What's The Story) Morning Glory":  {"(What's The Story) Morning Glory", 1995, 0, "", "/music/Oasis"},
	} {
		got, ok := cat[p]
		if !ok || got.Kind != EntAlbum {
			t.Errorf("%s: not an album (%v): %v", p, ok, keys(cat))
			continue
		}
		if g := (want{got.Title, got.Year, got.Discs, got.Edition, ""}); g.title != w.title || g.year != w.year || g.discs != w.discs || g.edition != w.edition || got.Parent != e.ino(w.under) {
			t.Errorf("%s:\n got %+v parent %d\nwant %+v parent %d", p, g, got.Parent, w, e.ino(w.under))
		}
	}
	for _, p := range []string{"/music/Genesis/1974 The Lamb Lies Down On Broadway/D1", "/music/YES/1 Studio", "/music/Simply Red/Studio Albums", "/music/Marillion/1989 - Seasons End/1989 - MP3 CBR Original"} {
		if _, ok := cat[p]; ok {
			t.Errorf("%s is an entry", p)
		}
	}
}

// Shows and films read through the name analyser: a pack folder's title, episode titles
// after the code, a miniseries' parts, a daily show's air dates, an anime's absolute
// numbers; Subs folders hold no episodes.
// Invariant: I-276
func TestCatalog_ExtrasNotItems(t *testing.T) {
	e := newEnv(t)
	e.library("/tv", meta.LibraryTV)
	e.write("/tv/Severance 2022 Seasons 1 and 2 Complete 1080p WEB x264 [i_c]/Season 1/Severance - S01E01 - Good News About Hell.mkv", []byte("v"))
	e.write("/tv/Invasion.2021.S01.PROPER.WEBRip.x264-ION10/Invasion.S01E01.WEBRip.x264-ION10.mkv", []byte("v"))
	e.write("/tv/Invasion.2021.S01.PROPER.WEBRip.x264-ION10/Subs/Invasion.S01E01.WEBRip.x264-ION10/English.srt", []byte("1"))
	e.write("/tv/CHILDHOODS END - Part 1, 2, 3 (Complete 2015 TV Miniseries) - 720p BluRay x264/Childhood's END - Part 02 of 03, The Deceivers (720p BluRay).mp4", []byte("v"))
	e.write("/tv/The Daily Show/The.Daily.Show.2024.03.14.Some.Guest.1080p.WEB.h264-GROUP.mkv", []byte("v"))
	e.write("/tv/Frieren/[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD1234].mkv", []byte("v"))
	e.library("/films", meta.LibraryMovies)
	e.write("/films/Upgrade (2018) [WEBRip] [1080p] [YTS.AM]/Upgrade.2018.1080p.WEBRip.x264-[YTS.AM].mp4", []byte("v"))
	e.pass()
	cat := e.catalog()
	sev := "/tv/Severance 2022 Seasons 1 and 2 Complete 1080p WEB x264 [i_c]"
	ce := "/tv/CHILDHOODS END - Part 1, 2, 3 (Complete 2015 TV Miniseries) - 720p BluRay x264"
	for p, w := range map[string]Entry{
		sev: {Kind: EntShow, Title: "Severance", Year: 2022},
		sev + "/Season 1/Severance - S01E01 - Good News About Hell.mkv": {Kind: EntEpisode, Title: "Good News About Hell", Season: 1, Episode: 1},
		"/tv/Invasion.2021.S01.PROPER.WEBRip.x264-ION10":                {Kind: EntShow, Title: "Invasion", Year: 2021},
		ce: {Kind: EntShow, Title: "CHILDHOODS END", Year: 2015},
		ce + "/Childhood's END - Part 02 of 03, The Deceivers (720p BluRay).mp4":           {Kind: EntEpisode, Title: "The Deceivers", Season: 1, Episode: 2},
		"/tv/The Daily Show/The.Daily.Show.2024.03.14.Some.Guest.1080p.WEB.h264-GROUP.mkv": {Kind: EntEpisode, Title: "Some Guest", Season: -1, Date: "2024-03-14"},
		"/tv/Frieren/[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD1234].mkv":           {Kind: EntEpisode, Title: "Episode 12", Season: -1, Absolute: 12},
		"/films/Upgrade (2018) [WEBRip] [1080p] [YTS.AM]":                                  {Kind: EntMovie, Title: "Upgrade", Year: 2018},
	} {
		got, ok := cat[p]
		if !ok {
			t.Errorf("%s: missing", p)
			continue
		}
		if got.Kind != w.Kind || got.Title != w.Title || got.Year != w.Year || got.Season != w.Season && w.Kind == EntEpisode || got.Episode != w.Episode || got.Date != w.Date || got.Absolute != w.Absolute {
			t.Errorf("%s:\n got %+v\nwant %+v", p, got, w)
		}
	}
	if q := cat["/films/Upgrade (2018) [WEBRip] [1080p] [YTS.AM]"].Quality; !reflect.DeepEqual(q, []string{"1080p", "WEBRip", "x264"}) {
		t.Errorf("film quality %v", q)
	}
	for p := range cat {
		if strings.Contains(p, "/Subs/") || strings.HasSuffix(p, "/Subs") {
			t.Errorf("%s from a Subs folder is an entry", p)
		}
	}
}

// A roms library laid out as a real one is (platform folders with region, category and
// Switch "Games" folders, keys and drivers beside the games): one platform per folder
// with its display name and game count, one game per file a platform takes, the discs
// of one title as one game, a cue folder as one game, Switch updates and DLC and every
// non-game left out; region and category folders add regions and flags. No ROM is read.
// Invariant: I-278
func TestCatalog_Roms_Table(t *testing.T) {
	e := newEnv(t)
	e.library("/roms", meta.LibraryRoms)
	rom := []byte("rom")
	for _, f := range []string{
		"/roms/SNES/USA/Donkey Kong Country (USA) (Rev 2).zip",
		"/roms/SNES/USA/Donkey Kong Country (USA).zip",
		"/roms/SNES/USA/Batman - Revenge of the Joker (USA) (Proto).zip",
		"/roms/SNES/the password is skwbr.txt",
		"/roms/N64/Hacks/Super Mario 64 (Splitscreen Multiplayer).zip",
		"/roms/N64/Japan/Bakuretsu Muteki Bangai-O.zip",
		"/roms/N64/GoldenEye 007 (All Unlockables).zip",
		"/roms/Gamecube/Tom Clancy's Splinter Cell - Chaos Theory (Disc 1).iso",
		"/roms/Gamecube/Tom Clancy's Splinter Cell - Chaos Theory (Disc 2).iso",
		"/roms/Gamecube/Mario Kart - Double Dash!! (Bonus Disc).iso",
		"/roms/PS1/Final Fantasy VII/Final Fantasy VII.cue",
		"/roms/PS1/Final Fantasy VII/Final Fantasy VII (Track 1).bin",
		"/roms/PS1/Alien Trilogy.pbp",
		"/roms/Switch/Games/Borderlands 3 [01009970122E4000][USA][v0].xci",
		"/roms/Switch/Games/Borderlands 3 [01009970122E4800][USA][v196608].nsp",
		"/roms/Switch/Games/Borderlands 3 [Bounty of Blood DLC][01009970122E5004][USA][v0].nsp",
		"/roms/Switch/a8xx-turnip-gen8-V26.zip",
		"/roms/Switch/emulator.apk",
		"/roms/Switch/Keys-22.1.0/prod.keys",
		"/roms/Atari Lynx Stuff/Chips Challenge.lnx",
	} {
		e.write(f, rom)
	}
	e.pass()
	cat := e.catalog()
	type want struct {
		kind            uint8
		title, platform string
		regions, flags  string
		rev             string
		discs, tracks   int
	}
	j := func(l []string) string { return strings.Join(l, ",") }
	wants := map[string]want{
		"/roms/SNES":             {EntPlatform, "Super Nintendo", "Super Nintendo", "", "", "", 0, 3},
		"/roms/N64":              {EntPlatform, "Nintendo 64", "Nintendo 64", "", "", "", 0, 3},
		"/roms/Gamecube":         {EntPlatform, "GameCube", "GameCube", "", "", "", 0, 2},
		"/roms/PS1":              {EntPlatform, "PlayStation", "PlayStation", "", "", "", 0, 2},
		"/roms/Switch":           {EntPlatform, "Nintendo Switch", "Nintendo Switch", "", "", "", 0, 1},
		"/roms/Atari Lynx Stuff": {EntPlatform, "Atari Lynx Stuff", "Atari Lynx Stuff", "", "", "", 0, 1},
		"/roms/SNES/USA/Donkey Kong Country (USA) (Rev 2).zip":                  {EntGame, "Donkey Kong Country", "Super Nintendo", "USA", "", "Rev 2", 0, 0},
		"/roms/SNES/USA/Donkey Kong Country (USA).zip":                          {EntGame, "Donkey Kong Country", "Super Nintendo", "USA", "", "", 0, 0},
		"/roms/SNES/USA/Batman - Revenge of the Joker (USA) (Proto).zip":        {EntGame, "Batman - Revenge of the Joker", "Super Nintendo", "USA", "proto", "", 0, 0},
		"/roms/N64/Hacks/Super Mario 64 (Splitscreen Multiplayer).zip":          {EntGame, "Super Mario 64", "Nintendo 64", "", "hack", "", 0, 0},
		"/roms/N64/Japan/Bakuretsu Muteki Bangai-O.zip":                         {EntGame, "Bakuretsu Muteki Bangai-O", "Nintendo 64", "Japan", "", "", 0, 0},
		"/roms/Gamecube/Tom Clancy's Splinter Cell - Chaos Theory (Disc 1).iso": {EntGame, "Tom Clancy's Splinter Cell - Chaos Theory", "GameCube", "", "", "", 2, 0},
		"/roms/Gamecube/Mario Kart - Double Dash!! (Bonus Disc).iso":            {EntGame, "Mario Kart - Double Dash!!", "GameCube", "", "bonus", "", 0, 0},
		"/roms/PS1/Final Fantasy VII":                                           {EntGame, "Final Fantasy VII", "PlayStation", "", "", "", 0, 0},
		"/roms/Switch/Games/Borderlands 3 [01009970122E4000][USA][v0].xci":      {EntGame, "Borderlands 3", "Nintendo Switch", "USA", "base", "v0", 0, 0},
	}
	for p, w := range wants {
		got, ok := cat[p]
		if !ok {
			t.Errorf("%s: missing", p)
			continue
		}
		g := want{got.Kind, got.Title, got.Platform, j(got.Regions), j(got.Flags), got.Revision, got.Discs, got.Tracks}
		if g != w {
			t.Errorf("%s:\n got %+v\nwant %+v", p, g, w)
		}
	}
	for p := range cat {
		if strings.HasPrefix(p, "/roms") {
			if _, ok := wants[p]; !ok && p != "/roms/N64/GoldenEye 007 (All Unlockables).zip" && p != "/roms/PS1/Alien Trilogy.pbp" && p != "/roms/Atari Lynx Stuff/Chips Challenge.lnx" {
				t.Errorf("%s is an entry", p)
			}
		}
	}
	if ff := cat["/roms/PS1/Final Fantasy VII"]; ff.Video != e.ino("/roms/PS1/Final Fantasy VII/Final Fantasy VII.cue") || ff.Parent != e.ino("/roms/PS1") {
		t.Errorf("a cue folder plays its cue: %+v", ff)
	}
}

// A platform's gamelist.xml overrides the name-derived fields of the games it lists and
// names their art; one that does not parse contributes nothing; art falls back to the
// game's own images by name.
// Invariant: I-278
func TestCatalog_Gamelist_Table(t *testing.T) {
	e := newEnv(t)
	e.library("/roms", meta.LibraryRoms)
	e.write("/roms/PSP/Tekken 6 (EU).iso", []byte("rom"))
	e.write("/roms/PSP/Wild Arms XF (EU).iso", []byte("rom"))
	e.write("/roms/PSP/media/images/Tekken 6 (EU).png", img)
	e.write("/roms/PSP/images/Wild Arms XF (EU)-image.jpg", img)
	e.write("/roms/PSP/gamelist.xml", []byte(`<?xml version="1.0"?>
<gameList><game><path>./Tekken 6 (EU).iso</path><name>Tekken 6</name><desc>Fight.</desc><releasedate>20091027T000000</releasedate>
<developer>Namco</developer><publisher>Bandai Namco</publisher><genre>Fighting</genre><players>1-2</players>
<image>./media/images/Tekken 6 (EU).png</image></game>
<game><path>/etc/passwd</path><name>escape</name></game></gameList>`))
	e.write("/roms/SNES/gamelist.xml", []byte(`<gameList><game><path>./x.zip</path><name>X`))
	e.write("/roms/SNES/Some Game (USA).zip", []byte("rom"))
	e.pass()
	cat := e.catalog()
	tk := cat["/roms/PSP/Tekken 6 (EU).iso"]
	if tk.Title != "Tekken 6" || tk.Year != 2009 || tk.Plot != "Fight." || tk.Developer != "Namco" || tk.Publisher != "Bandai Namco" ||
		tk.Players != "1-2" || !reflect.DeepEqual(tk.Genres, []string{"Fighting"}) || tk.Poster != "media/images/Tekken 6 (EU).png" {
		t.Errorf("tekken %+v", tk)
	}
	if wa := cat["/roms/PSP/Wild Arms XF (EU).iso"]; wa.Title != "Wild Arms XF" || wa.Poster != "images/Wild Arms XF (EU)-image.jpg" {
		t.Errorf("wild arms %+v", wa)
	}
	if sg := cat["/roms/SNES/Some Game (USA).zip"]; sg.Title != "Some Game" || sg.Plot != "" {
		t.Errorf("a broken gamelist contributed: %+v", sg)
	}
	g, ok := DecodeGamelist([]byte(`<gameList><game><path>../../up.zip</path></game><game><path>.\dir\a.zip</path><name>A</name></game></gameList>`))
	if !ok || len(g) != 1 || g["dir/a.zip"].Name != "A" {
		t.Errorf("paths: %+v", g)
	}
}

// Invariant: I-278
func FuzzGamelist(f *testing.F) {
	f.Add([]byte(`<gameList><game><path>./a.zip</path><name>A</name><releasedate>1997</releasedate></game></gameList>`))
	f.Add([]byte(`<gameList><game><path>../x</path>`))
	f.Fuzz(func(t *testing.T, b []byte) {
		g, _ := DecodeGamelist(b)
		for p := range g {
			if p == "" || p == ".." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") {
				t.Fatalf("path %q escapes the platform folder", p)
			}
		}
	})
}
