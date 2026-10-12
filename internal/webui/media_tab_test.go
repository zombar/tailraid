package webui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/webui"
)

// lazyCatalog lets a test build the search service after the UI exists.
type lazyCatalog struct{ svc *search.Service }

func (l *lazyCatalog) Catalog(ctx context.Context) ([]search.Entry, error) { return l.svc.Catalog(ctx) }

// mediaUI is a UI with a real index over its fabric; pass runs the leader's pass.
func mediaUI(t *testing.T) (*ui, func()) {
	t.Helper()
	lc := &lazyCatalog{}
	u := newUI(t, func(o *webui.Options) { o.Catalog = lc })
	u.fs.SetFlusher(u.f.Flusher())
	lc.svc = &search.Service{Src: u.f.FSM, Clk: u.f.Clk, Cfg: search.DefaultConfig(), Vol: search.VFS{FS: u.fs, St: u.f.FSM, Rd: u.f.ReaderNoCache()}}
	lc.svc.Cfg.MaxFile = 64 << 10
	return u, func() {
		t.Helper()
		u.flush()
		if _, err := lc.svc.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

type mediaLib struct {
	Lib   webui.MediaLib    `json:"lib"`
	Items []webui.MediaItem `json:"items"`
}

func (u *ui) mediaJSON(query string, out any) int {
	u.t.Helper()
	code, _, body := u.get("/api/media?" + query)
	if code == 200 && out != nil {
		must(u.t, json.Unmarshal(body, out))
	}
	return code
}

func titles(items []webui.MediaItem) []string {
	out := []string{}
	for i := range items {
		out = append(out, items[i].Title)
	}
	return out
}

// A library lists its items from the live tree, named from the tree until the
// catalogue has an entry; sort and offset are bounded; a folder that is not a library
// is 404; a page holds at most MediaPage items.
// Invariant: I-212
func TestWebUI_Media_Library_Table(t *testing.T) {
	u, pass := mediaUI(t)
	u.write("/films/Arrival (2016)/arrival.mkv", 0, []byte("video"))
	u.write("/films/Arrival (2016)/movie.nfo", 0, []byte(`<movie><title>Arrival</title><year>2016</year><genre>Drama</genre><runtime>116</runtime></movie>`))
	u.write("/films/Arrival (2016)/poster.jpg", 0, []byte{0xff, 0xd8})
	u.f.Clk.Advance(1e9)
	u.write("/films/Moon.2009.1080p.BluRay.x264-GROUP.mkv", 0, []byte("video"))
	u.write("/films/Extras/readme.txt", 0, []byte("no video"))
	u.write("/films/Shorts/Tiny (2020)/tiny.mp4", 0, []byte("v"))
	u.flush()
	for _, p := range []string{"/films", "/films/Shorts"} {
		if code, _ := u.setFlags(p, `"library":"movies"`); code != 200 {
			t.Fatalf("flag %s: %d", p, code)
		}
	}
	var libs struct{ Libs []webui.MediaLib }
	if code := u.mediaJSON("", &libs); code != 200 || len(libs.Libs) != 2 || libs.Libs[0].Path != "/films" || libs.Libs[1].Path != "/films/Shorts" {
		t.Fatalf("libraries: %d %+v", code, libs)
	}
	var l mediaLib
	if code := u.mediaJSON("lib=/films", &l); code != 200 || !reflect.DeepEqual(titles(l.Items), []string{"Arrival", "Moon"}) || l.Items[0].Catalogued {
		t.Fatalf("before the pass: %d %+v", code, l)
	}
	if l.Items[1].Year != 2009 || l.Items[1].Video != "/films/Moon.2009.1080p.BluRay.x264-GROUP.mkv" || l.Lib.Count != 2 || l.Lib.Kind != "movies" {
		t.Fatalf("a bare file by its cleaned name: %+v", l)
	}
	pass()
	l = mediaLib{}
	u.mediaJSON("lib=/films", &l)
	if a := l.Items[0]; !a.Catalogued || a.Runtime != 116 || a.Poster != "/films/Arrival (2016)/poster.jpg" || !reflect.DeepEqual(l.Lib.Genres, []string{"drama"}) {
		t.Fatalf("after the pass: %+v %+v", a, l.Lib)
	}
	for _, tc := range []struct {
		q    string
		code int
		want []string
	}{
		{"sort=title", 200, []string{"Arrival", "Moon"}},
		{"sort=year", 200, []string{"Arrival", "Moon"}},
		{"sort=added", 200, []string{"Moon", "Arrival"}},
		{"sort=size", 400, nil},
		{"offset=1", 200, []string{"Moon"}},
		{"offset=2", 200, []string{}},
		{"offset=3", 200, []string{}},
		{"offset=-1", 400, nil},
		{"offset=x", 400, nil},
	} {
		var got mediaLib
		code := u.mediaJSON("lib=/films&"+tc.q, &got)
		if code != tc.code || (code == 200 && !reflect.DeepEqual(titles(got.Items), tc.want)) {
			t.Errorf("%s: %d %v, want %d %v", tc.q, code, titles(got.Items), tc.code, tc.want)
		}
	}
	for _, p := range []string{"/films/Extras", "/films/Moon.2009.1080p.BluRay.x264-GROUP.mkv", "/nowhere"} {
		if code := u.mediaJSON("lib="+url.QueryEscape(p), nil); code != 404 {
			t.Errorf("lib=%s: %d, want 404", p, code)
		}
	}

	for i := range webui.MediaPage + 1 {
		must(t, u.fs.MkdirAll(fmt.Sprintf("/shows/show %03d", i), 0o755))
	}
	if code, _ := u.setFlags("/shows", `"library":"tv"`); code != 200 {
		t.Fatal("flag /shows")
	}
	for _, tc := range []struct{ offset, want int }{{0, webui.MediaPage}, {1, webui.MediaPage}, {webui.MediaPage - 1, 2}, {webui.MediaPage, 1}, {webui.MediaPage + 1, 0}} {
		var got mediaLib
		u.mediaJSON(fmt.Sprintf("lib=/shows&offset=%d", tc.offset), &got)
		if len(got.Items) != tc.want || got.Lib.Count != webui.MediaPage+1 {
			t.Errorf("offset %d: %d items of %d, want %d", tc.offset, len(got.Items), got.Lib.Count, tc.want)
		}
	}
}

// A show's seasons and episodes come from its folders live, numbered from the names
// until the catalogue has them, specials last, the season poster from the show's folder.
// Invariant: I-212
func TestWebUI_Media_Item_ShowSeasonsEpisodes(t *testing.T) {
	u, pass := mediaUI(t)
	u.write("/shows/Dark/tvshow.nfo", 0, []byte(`<tvshow><title>Dark</title><year>2017</year><plot>Time travel.</plot></tvshow>`))
	u.write("/shows/Dark/Season 1/Dark.S01E01.mkv", 0, []byte("v"))
	u.write("/shows/Dark/Season 1/Dark.S01E02E03.mkv", 0, []byte("v"))
	u.write("/shows/Dark/Dark.S02E01.mkv", 0, []byte("v"))
	u.write("/shows/Dark/Specials/Dark.S00E01.mkv", 0, []byte("v"))
	u.write("/shows/Dark/season01-poster.jpg", 0, []byte{0xff, 0xd8})
	u.write("/shows/Dark/notes.mkv", 0, []byte("v")) // no episode number: not an episode
	u.flush()
	if code, _ := u.setFlags("/shows", `"library":"tv"`); code != 200 {
		t.Fatal("flag")
	}
	type ep struct{ s, e, to int }
	shape := func(it webui.MediaItem) map[int][]ep {
		out := map[int][]ep{}
		for _, s := range it.Seasons {
			for _, e := range s.Episodes {
				out[s.N] = append(out[s.N], ep{e.Season, e.Episode, e.EpisodeTo})
			}
		}
		return out
	}
	want := map[int][]ep{1: {{1, 1, 1}, {1, 2, 3}}, 2: {{2, 1, 1}}, 0: {{0, 1, 1}}}
	for _, stage := range []string{"before the pass", "after the pass"} {
		var it webui.MediaItem
		if code := u.mediaJSON("item=/shows/Dark", &it); code != 200 {
			t.Fatalf("%s: %d", stage, code)
		}
		if got := shape(it); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v", stage, got)
		}
		if n := []int{it.Seasons[0].N, it.Seasons[1].N, it.Seasons[2].N}; !reflect.DeepEqual(n, []int{1, 2, 0}) || it.Children != 3 {
			t.Fatalf("%s: season order %v", stage, n)
		}
		if stage == "after the pass" && (it.Title != "Dark" || it.Plot != "Time travel." || it.Year != 2017 || it.Seasons[0].Poster != "/shows/Dark/season01-poster.jpg") {
			t.Fatalf("catalogued show %+v", it)
		}
		if stage == "before the pass" {
			pass()
		}
	}
	if code := u.mediaJSON("item=/shows", nil); code != 404 {
		t.Fatalf("the library itself: %d", code)
	}
}

// Nothing in the trash or under /.tailraid is listed or opened; a library in the trash
// leaves the list.
// Invariant: I-212
func TestWebUI_Media_TrashedAndReservedHidden(t *testing.T) {
	u, pass := mediaUI(t)
	u.write("/films/Gone (2001)/gone.mkv", 0, []byte("v"))
	u.write("/films/Kept (2002)/kept.mkv", 0, []byte("v"))
	u.write("/old/Old (1990)/old.mkv", 0, []byte("v"))
	u.flush()
	for _, p := range []string{"/films", "/old"} {
		if code, _ := u.setFlags(p, `"library":"movies"`); code != 200 {
			t.Fatal("flag")
		}
	}
	pass()
	gone := u.inoOf("/films/Gone (2001)")
	u.del("/films/Gone (2001)", "/old")
	var l mediaLib
	u.mediaJSON("lib=/films", &l)
	if !reflect.DeepEqual(titles(l.Items), []string{"Kept"}) {
		t.Fatalf("after the delete: %v", titles(l.Items))
	}
	var libs struct{ Libs []webui.MediaLib }
	u.mediaJSON("", &libs)
	if len(libs.Libs) != 1 || libs.Libs[0].Path != "/films" {
		t.Fatalf("a deleted library is listed: %+v", libs)
	}
	if code := u.mediaJSON(fmt.Sprintf("item=/&in=%d", gone), nil); code != 404 {
		t.Fatalf("an item in the trash: %d", code)
	}
	if code := u.mediaJSON("lib=/old", nil); code != 404 {
		t.Fatalf("a library in the trash: %d", code)
	}
	if code := u.mediaJSON("item=/.tailraid/index", nil); code != 404 {
		t.Fatalf("the reserved folder: %d", code)
	}
}

// An item under an nsfw folder carries the flag, in the list and the item view.
// Invariant: I-212
func TestWebUI_Media_NSFWFlagCarried(t *testing.T) {
	u, _ := mediaUI(t)
	u.write("/films/late/Night (2001)/night.mkv", 0, []byte("v"))
	u.write("/films/Day (2002)/day.mkv", 0, []byte("v"))
	u.flush()
	if code, _ := u.setFlags("/films", `"library":"movies"`); code != 200 {
		t.Fatal("flag")
	}
	if code, _ := u.setFlags("/films/Day (2002)", `"nsfw":true`); code != 200 {
		t.Fatal("nsfw")
	}
	var l mediaLib
	u.mediaJSON("lib=/films", &l)
	if len(l.Items) != 1 || !l.Items[0].NSFW {
		t.Fatalf("list %+v", l.Items)
	}
	var it webui.MediaItem
	u.mediaJSON("item="+url.QueryEscape("/films/Day (2002)"), &it)
	if !it.NSFW || it.Video != "/films/Day (2002)/day.mkv" {
		t.Fatalf("item %+v", it)
	}
}

// Two nodes with the same tree and chain answer with identical bytes.
// Invariant: I-212
func TestWebUI_Media_DeterministicAcrossNodes(t *testing.T) {
	read := func() [][]byte {
		u, pass := mediaUI(t)
		u.write("/films/Arrival (2016)/arrival.mkv", 0, []byte("video"))
		u.write("/films/Arrival (2016)/movie.nfo", 0, []byte(`<movie><title>Arrival</title><plot>Heptapods.</plot></movie>`))
		u.write("/films/Moon.2009.mkv", 0, []byte("video"))
		u.write("/shows/Dark/Season 1/Dark.S01E01.mkv", 0, []byte("v"))
		u.flush()
		u.setFlags("/films", `"library":"movies"`)
		u.setFlags("/shows", `"library":"tv"`)
		pass()
		var out [][]byte
		for _, q := range []string{"", "lib=/films", "lib=/films&sort=added", "lib=/shows", "item=" + url.QueryEscape("/films/Arrival (2016)"), "item=/shows/Dark"} {
			code, _, body := u.get("/api/media?" + q)
			if code != 200 {
				t.Fatalf("%s: %d %s", q, code, body)
			}
			out = append(out, body)
		}
		return out
	}
	a, b := read(), read()
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("answer %d differs:\n%s\n%s", i, a[i], b[i])
		}
	}
}

// A film's or an episode's video carries what the page needs to grab its frame as the
// file browser does: the video's inode and version, and whether a frame is stored. Once
// the page PUTs one, the listing says so.
//
// Invariant: I-212
func TestWebUI_Media_VideoFrameFields(t *testing.T) {
	u, _ := mediaUI(t)
	u.write("/films/Moon.2009.mkv", 0, []byte("video"))
	u.write("/films/Arrival/arrival.mkv", 0, []byte("video"))
	u.write("/shows/Dark/Dark.S01E01.mkv", 0, []byte("v"))
	u.flush()
	for _, p := range []struct{ path, kind string }{{"/films", "movies"}, {"/shows", "tv"}} {
		if code, _ := u.setFlags(p.path, `"library":"`+p.kind+`"`); code != 200 {
			t.Fatalf("flag %s: %d", p.path, code)
		}
	}
	check := func(stage string, it webui.MediaItem, video string, thumb bool) {
		t.Helper()
		st := u.statOf(video)
		if it.Video != video || uint64(it.VideoIno) != st.Ino || it.VideoVersion != st.Version || it.VideoThumb != thumb {
			t.Fatalf("%s: %s: video %q ino %d version %d thumb %v; want %q %d %d %v", stage, it.Path, it.Video, it.VideoIno, it.VideoVersion, it.VideoThumb, video, st.Ino, st.Version, thumb)
		}
	}
	var l mediaLib
	u.mediaJSON("lib=/films", &l)
	check("list", l.Items[1], "/films/Moon.2009.mkv", false)
	var film webui.MediaItem
	u.mediaJSON("item=/films/Arrival", &film)
	check("film", film, "/films/Arrival/arrival.mkv", false)
	var show webui.MediaItem
	u.mediaJSON("item=/shows/Dark", &show)
	check("episode", show.Seasons[0].Episodes[0], "/shows/Dark/Dark.S01E01.mkv", false)

	resp := u.do(http.MethodPut, fmt.Sprintf("/api/thumb?path=%s&version=%d", url.QueryEscape("/films/Moon.2009.mkv"), u.statOf("/films/Moon.2009.mkv").Version), bytes.NewReader(jpegBytes(16, 16, green)), webui.HeaderUI, "1")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("frame PUT: %d", resp.StatusCode)
	}
	l = mediaLib{}
	u.mediaJSON("lib=/films", &l)
	check("after the frame", l.Items[1], "/films/Moon.2009.mkv", true)
}
