package webui_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

const minute = 60 * 1000 // ms

// as makes the loopback caller another Tailscale login and forgets the cached answer.
func (u *ui) as(login string, tags ...string) {
	u.ts.SetUser(loop, login, tags...)
	u.auth.Purge()
}

func (u *ui) report(body string) int {
	u.t.Helper()
	return u.post("/api/playback", body, nil)
}

func (u *ui) pos(p string, pos, dur int, final bool) int {
	u.t.Helper()
	return u.report(fmt.Sprintf(`{"path":%q,"pos":%d,"dur":%d,"final":%v}`, p, pos, dur, final))
}

func (u *ui) row(viewer, p string) (meta.PlayRow, bool) {
	u.t.Helper()
	in, err := u.f.FSM.Resolve(meta.SplitPath(p))
	must(u.t, err)
	return u.f.FSM.PlaybackRow(viewer, in.Ino)
}

func (u *ui) progress(p string) (int, webui.Progress) {
	u.t.Helper()
	code, _, body := u.get("/api/playback?path=" + url.QueryEscape(p))
	var pr webui.Progress
	if code == 200 {
		must(u.t, json.Unmarshal(body, &pr))
	}
	return code, pr
}

// The viewer is the request's: the Tailscale login here (no users), never the body's;
// another login reads none of it; a device with no login has no viewer, reads nothing and
// writes nothing.
// Invariant: I-407
func TestWebUI_Playback_ViewerFromRequest_Table(t *testing.T) {
	u := newUI(t, func(o *webui.Options) { o.NodeName = "pike" })
	u.write("/films/a.mkv", 0, []byte("video"))
	if code := u.report(`{"path":"/films/a.mkv","pos":90000,"dur":7200000,"viewer":"t:mallory@example.com"}`); code != 204 {
		t.Fatalf("report: %d", code)
	}
	r, ok := u.row("t:"+login, "/films/a.mkv")
	if !ok || r.Pos != 90000 || r.Dur != 7200000 || r.On != "pike" {
		t.Fatalf("row %+v %v", r, ok)
	}
	if _, ok := u.row("t:mallory@example.com", "/films/a.mkv"); ok {
		t.Fatal("the body named the viewer")
	}
	if code, pr := u.progress("/films/a.mkv"); code != 200 || pr.Pos != 90000 || pr.On != "pike" {
		t.Fatalf("GET: %d %+v", code, pr)
	}
	u.as("pat@example.com") // another person's browser
	if code, _ := u.progress("/films/a.mkv"); code != 404 {
		t.Fatalf("another login read the row: %d", code)
	}
	u.as("", "tag:server") // a tagged device: no login
	if code, _ := u.progress("/films/a.mkv"); code != 404 {
		t.Fatalf("no viewer read: %d", code)
	}
	if code := u.pos("/films/a.mkv", 1, 2, true); code != 403 {
		t.Fatalf("no viewer wrote: %d", code)
	}
	var cont struct{ Items []webui.MediaItem }
	if code := u.mediaJSON("continue=1", &cont); code != 200 || len(cont.Items) != 0 {
		t.Fatalf("no viewer's row: %d %+v", code, cont)
	}
}

// With users, the viewer is the signed-in user; a file they may not read is 403 and
// takes no row.
// Invariant: I-407
func TestWebUI_Playback_UserNeedsRead(t *testing.T) {
	u, _, lee := usersUI(t)
	b := u.signIn(lee)
	if code, body := u.postAs(b, "/api/playback", `{"path":"/r/f","pos":1000,"dur":5000,"final":true}`); code != 204 {
		t.Fatalf("readable: %d %s", code, body)
	}
	viewer := meta.ViewerUserPrefix + strconv.Itoa(int(lee))
	if _, ok := u.row(viewer, "/r/f"); !ok {
		t.Fatal("no row for the user")
	}
	if code, _ := u.postAs(b, "/api/playback", `{"path":"/other/f","pos":1000,"dur":5000,"final":true}`); code != 403 {
		t.Fatalf("unreadable: %d", code)
	}
	if _, ok := u.row(viewer, "/other/f"); ok {
		t.Fatal("a row of a file the user may not read")
	}
	if code, _ := u.getAs(b, "/api/playback?path=/other/f"); code != 403 {
		t.Fatalf("GET unreadable: %d", code)
	}
}

// A viewer's GET and POST reach only its own rows: two logins over one file keep two rows.
// Invariant: I-407
func TestWebUI_Playback_RowsAreTheViewers(t *testing.T) {
	u := newUI(t)
	u.write("/a.mkv", 0, []byte("video"))
	u.pos("/a.mkv", 5*minute, 100*minute, true)
	u.as("pat@example.com")
	u.pos("/a.mkv", 7*minute, 100*minute, true)
	u.report(`{"paths":["/a.mkv"],"action":"watched"}`)
	if r, _ := u.row("t:pat@example.com", "/a.mkv"); r.Pos != 7*minute || !r.Watched {
		t.Fatalf("pat: %+v", r)
	}
	if r, _ := u.row("t:"+login, "/a.mkv"); r.Pos != 5*minute || r.Watched {
		t.Fatalf("jim: %+v", r)
	}
}

// One position per (viewer, file) per PlaybackEvery: a sooner report is dropped unless
// final.
// Invariant: I-408
func TestWebUI_Playback_Coalesce_Table(t *testing.T) {
	u := newUI(t)
	u.write("/a.mkv", 0, []byte("video"))
	for i, c := range []struct {
		advance time.Duration
		pos     int
		final   bool
		want    uint64
	}{
		{0, 1000, false, 1000},
		{5 * time.Second, 6000, false, 1000},                         // within the window: dropped
		{webui.PlaybackEvery - 5*time.Second - 1, 7000, false, 1000}, // a hair short of it
		{1, 8000, false, 8000},                                       // the window closed
		{time.Second, 9000, true, 9000},                              // final: at once
		{time.Second, 3000, false, 9000},                             // within the window after the final
	} {
		u.f.Clk.Advance(c.advance)
		if code := u.pos("/a.mkv", c.pos, 100*minute, c.final); code != 204 {
			t.Fatalf("%d: %d", i, code)
		}
		if r, _ := u.row("t:"+login, "/a.mkv"); r.Pos != c.want {
			t.Fatalf("%d: pos %d, want %d", i, r.Pos, c.want)
		}
	}
}

// A played report and every action go at once, whatever the window.
// Invariant: I-408
func TestWebUI_Playback_FinalAndActionsAtOnce(t *testing.T) {
	u := newUI(t)
	u.write("/t.mp3", 0, []byte("audio"))
	u.pos("/t.mp3", 1000, 60000, false)
	for range 3 {
		if code := u.report(`{"path":"/t.mp3","played":true}`); code != 204 {
			t.Fatal(code)
		}
	}
	if code := u.report(`{"path":"/t.mp3","action":"hide"}`); code != 204 {
		t.Fatal(code)
	}
	r, _ := u.row("t:"+login, "/t.mp3")
	if r.Plays != 3 || !r.Hidden {
		t.Fatalf("row %+v", r)
	}
	var res struct{ Failed []webui.Failure }
	if code := u.post("/api/playback", `{"paths":["/t.mp3","/nope.mkv"],"action":"watched"}`, &res); code != 200 || len(res.Failed) != 1 || res.Failed[0].Status != 404 {
		t.Fatalf("batch: %d %+v", code, res)
	}
	if r, _ := u.row("t:"+login, "/t.mp3"); !r.Watched {
		t.Fatal("the batch's good path was not marked")
	}
}

// A refused report is answered with its status, never 204.
// Invariant: I-408
func TestWebUI_Playback_RefusalIsStatus(t *testing.T) {
	u := newUI(t)
	u.write("/a.mkv", 0, []byte("video"))
	must(t, u.fs.MkdirAll("/dir", 0o755))
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"path":"/a.mkv","pos":1}`, 400},               // no duration
		{`{"path":"/dir","pos":1,"dur":2}`, 404},         // not a file
		{`{"path":"/nope.mkv","pos":1,"dur":2}`, 404},    // no such file
		{`{"path":"/a.mkv","action":"rewind"}`, 400},     // unknown action
		{`{"paths":[],"action":"watched"}`, 400},         // an empty batch
		{`{"path":"/.tailraid/x","pos":1,"dur":2}`, 404}, // the reserved folder
		{`{"path":"/a.mkv","action":"hide"}`, 404},       // nothing to hide
	} {
		if code := u.report(c.body); code != c.code {
			t.Errorf("%s: %d, want %d", c.body, code, c.code)
		}
	}
	u.fs.MinCodec = func() uint8 { return meta.CodecAttrs } // a member reads codec 16
	if code := u.pos("/a.mkv", 1, 2, true); code != 503 {
		t.Fatalf("codec: %d", code)
	}
	if code := u.post("/api/playback", `{"path":"/a.mkv","pos":1,"dur":2}`, nil); code != 503 {
		t.Fatalf("codec, within the window: %d", code)
	}
	u.fs.MinCodec = nil
	big := bytes.Repeat([]byte(`"/a.mkv",`), webui.MaxBatch)
	if code := u.report(`{"paths":[` + string(big) + `"/a.mkv"],"action":"watched"}`); code != 400 {
		t.Fatalf("a batch past MaxBatch: %d", code)
	}
}

// watchUI is a UI with a films, a shows and a music library, flagged.
func watchUI(t *testing.T) *ui {
	t.Helper()
	u, _ := mediaUI(t)
	u.write("/films/Arrival (2016)/arrival.mkv", 0, []byte("video"))
	u.write("/films/Heat (1995)/heat.mkv", 0, []byte("video"))
	u.write("/films/Moon.2009.mkv", 0, []byte("video"))
	for _, e := range []string{"Season 1/Show.S01E01.mkv", "Season 1/Show.S01E02.mkv", "Season 2/Show.S02E01.mkv", "Specials/Show.S00E01.mkv"} {
		u.write("/shows/Show/"+e, 0, []byte("v"))
	}
	for i := range 3 {
		u.write(fmt.Sprintf("/music/Bonobo/Mix/%02d.mp3", i+1), 0, []byte("audio"))
		u.write(fmt.Sprintf("/music/Agnes/Aventine/%02d.mp3", i+1), 0, []byte("audio"))
	}
	u.flush()
	for p, k := range map[string]string{"/films": "movies", "/shows": "tv", "/music": "music"} {
		if code, _ := u.setFlags(p, `"library":"`+k+`"`); code != 200 {
			t.Fatalf("flag %s: %d", p, code)
		}
	}
	return u
}

func (u *ui) continueRow() []webui.MediaItem {
	u.t.Helper()
	var out struct{ Items []webui.MediaItem }
	if code := u.mediaJSON("continue=1", &out); code != 200 {
		u.t.Fatalf("continue: %d", code)
	}
	return out.Items
}

func paths(items []webui.MediaItem) []string {
	out := []string{}
	for i := range items {
		out = append(out, items[i].Kind+":"+items[i].Path)
	}
	return out
}

// Continue watching: started and unfinished films, episodes and long tracks, newest
// first; nothing watched, hidden, under a minute or a short track.
// Invariant: I-409
func TestWebUI_Continue_Table(t *testing.T) {
	u := watchUI(t)
	step := func() { u.f.Clk.Advance(time.Minute) }
	u.pos("/films/Arrival (2016)/arrival.mkv", 30*minute, 116*minute, true)
	step()
	u.pos("/films/Heat (1995)/heat.mkv", 30*1000, 170*minute, true) // under a minute in
	step()
	u.pos("/films/Moon.2009.mkv", 95*minute, 97*minute, true) // watched
	step()
	u.pos("/shows/Show/Season 1/Show.S01E01.mkv", 20*minute, 45*minute, true)
	step()
	u.pos("/music/Bonobo/Mix/01.mp3", 41*minute, 72*minute, true) // a long track
	step()
	u.pos("/music/Agnes/Aventine/01.mp3", 2*minute, 4*minute, true) // a short one
	got := u.continueRow()
	want := []string{"track:/music/Bonobo/Mix/01.mp3", "episode:/shows/Show/Season 1/Show.S01E01.mkv", "movie:/films/Arrival (2016)"}
	if !reflect.DeepEqual(paths(got), want) {
		t.Fatalf("row %v, want %v", paths(got), want)
	}
	if got[1].Show != "Show" || got[1].Progress == nil || got[1].Progress.Pos != 20*minute {
		t.Fatalf("episode %+v", got[1])
	}
	if got[2].Video != "/films/Arrival (2016)/arrival.mkv" || got[2].Progress.Dur != 116*minute {
		t.Fatalf("film %+v", got[2])
	}
	u.report(`{"path":"/films/Arrival (2016)/arrival.mkv","action":"hide"}`)
	if got := paths(u.continueRow()); len(got) != 2 {
		t.Fatalf("after hide: %v", got)
	}
	step()
	u.pos("/films/Arrival (2016)/arrival.mkv", 31*minute, 116*minute, true) // played again: back
	if got := paths(u.continueRow()); got[0] != "movie:/films/Arrival (2016)" {
		t.Fatalf("after playing again: %v", got)
	}
}

// The episode after one watched is next up, across a season's end, never into Specials,
// and only while it has no row of its own.
// Invariant: I-409
func TestWebUI_Continue_NextUpAcrossSeason(t *testing.T) {
	u := watchUI(t)
	u.pos("/shows/Show/Season 1/Show.S01E02.mkv", 45*minute, 45*minute, true)
	got := u.continueRow()
	if len(got) != 1 || got[0].Path != "/shows/Show/Season 2/Show.S02E01.mkv" || !got[0].NextUp || got[0].Show != "Show" {
		t.Fatalf("next up: %+v", got)
	}
	u.f.Clk.Advance(time.Minute)
	u.pos("/shows/Show/Season 2/Show.S02E01.mkv", 10*1000, 45*minute, true) // started, under a minute
	if got := u.continueRow(); len(got) != 0 {
		t.Fatalf("a next episode with a row of its own: %v", paths(got))
	}
	u.f.Clk.Advance(time.Minute)
	u.pos("/shows/Show/Season 2/Show.S02E01.mkv", 45*minute, 45*minute, true) // the last regular one watched
	if got := u.continueRow(); len(got) != 0 {
		t.Fatalf("after the last episode: %v", paths(got))
	}
}

// A file in the trash, or under a folder no longer a library, drops out of the row.
// Invariant: I-409
func TestWebUI_Continue_TrashAndUnflaggedDrop(t *testing.T) {
	u := watchUI(t)
	u.pos("/films/Arrival (2016)/arrival.mkv", 30*minute, 116*minute, true)
	u.pos("/music/Bonobo/Mix/01.mp3", 41*minute, 72*minute, true)
	if got := u.continueRow(); len(got) != 2 {
		t.Fatalf("before: %v", paths(got))
	}
	if code := u.post("/api/delete", `{"paths":["/films/Arrival (2016)"]}`, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := u.setFlags("/music", `"library":"none"`); code != 200 {
		t.Fatalf("unflag: %d", code)
	}
	if got := u.continueRow(); len(got) != 0 {
		t.Fatalf("after: %v", paths(got))
	}
}

// Two nodes over one FSM answer one viewer's row and a library byte-identically.
// Invariant: I-409, I-410
func TestWebUI_Continue_DeterministicAcrossNodes(t *testing.T) {
	u := watchUI(t)
	u.pos("/films/Arrival (2016)/arrival.mkv", 30*minute, 116*minute, true)
	u.pos("/shows/Show/Season 1/Show.S01E01.mkv", 45*minute, 45*minute, true)
	u.report(`{"path":"/music/Agnes/Aventine/02.mp3","played":true}`)
	other := newUIOn(t, u.f)
	for _, q := range []string{"continue=1", "lib=/films", "lib=/shows", "lib=/music&sort=played", "item=/shows/Show"} {
		a, _, ab := u.get("/api/media?" + q)
		b, _, bb := other.get("/api/media?" + q)
		if a != 200 || b != 200 || !bytes.Equal(ab, bb) {
			t.Fatalf("%s:\n%s\n%s", q, ab, bb)
		}
	}
}

// Every item carries the viewer's state: a film's and an episode's progress, a show's
// watched and next, an album's plays and its tracks'.
// Invariant: I-410
func TestWebUI_Media_ProgressFields_Table(t *testing.T) {
	u := watchUI(t)
	u.pos("/films/Arrival (2016)/arrival.mkv", 30*minute, 116*minute, true)
	var lib mediaLib
	if code := u.mediaJSON("lib=/films", &lib); code != 200 {
		t.Fatal(code)
	}
	for _, it := range lib.Items {
		got := it.Progress != nil
		if want := it.Path == "/films/Arrival (2016)"; got != want {
			t.Fatalf("%s progress %+v", it.Path, it.Progress)
		}
	}
	var film webui.MediaItem
	u.mediaJSON("item="+url.QueryEscape("/films/Arrival (2016)"), &film)
	if film.Progress == nil || film.Progress.Pos != 30*minute {
		t.Fatalf("item: %+v", film.Progress)
	}
	u.pos("/shows/Show/Season 1/Show.S01E01.mkv", 45*minute, 45*minute, true)
	var show webui.MediaItem
	u.mediaJSON("item=/shows/Show", &show)
	var ep *webui.MediaItem
	for i := range show.Seasons {
		for j := range show.Seasons[i].Episodes {
			if show.Seasons[i].Episodes[j].Path == "/shows/Show/Season 1/Show.S01E01.mkv" {
				ep = &show.Seasons[i].Episodes[j]
			}
		}
	}
	if ep == nil || ep.Progress == nil || !ep.Progress.Watched {
		t.Fatalf("episode %+v", ep)
	}
	// a viewer with no rows sees what one who never played anything sees
	u.as("pat@example.com")
	var fresh mediaLib
	u.mediaJSON("lib=/films", &fresh)
	for _, it := range fresh.Items {
		if it.Progress != nil {
			t.Fatalf("pat sees jim's progress on %s", it.Path)
		}
	}
}

// A show is watched once every episode of its numbered seasons is; its next is the
// episode in progress, else the one after the furthest watched, else the first.
// Invariant: I-410
func TestWebUI_Media_ShowWatchedAndNext(t *testing.T) {
	u := watchUI(t)
	show := func() webui.MediaItem {
		t.Helper()
		var lib mediaLib
		if code := u.mediaJSON("lib=/shows", &lib); code != 200 || len(lib.Items) != 1 {
			t.Fatalf("%d %+v", code, lib)
		}
		return lib.Items[0]
	}
	if s := show(); s.Watched || s.Next != "" {
		t.Fatalf("no rows: %+v", s)
	}
	u.pos("/shows/Show/Season 1/Show.S01E01.mkv", 45*minute, 45*minute, true)
	if s := show(); s.Watched || s.Next != "/shows/Show/Season 1/Show.S01E02.mkv" {
		t.Fatalf("after E01: watched %v next %q", s.Watched, s.Next)
	}
	u.f.Clk.Advance(time.Minute)
	u.pos("/shows/Show/Season 2/Show.S02E01.mkv", 5*minute, 45*minute, true)
	if s := show(); s.Next != "/shows/Show/Season 2/Show.S02E01.mkv" {
		t.Fatalf("in progress: %q", s.Next)
	}
	u.report(`{"paths":["/shows/Show/Season 1/Show.S01E02.mkv","/shows/Show/Season 2/Show.S02E01.mkv"],"action":"watched"}`)
	if s := show(); !s.Watched || s.Next != "/shows/Show/Season 1/Show.S01E01.mkv" {
		t.Fatalf("all watched: watched %v next %q", s.Watched, s.Next)
	}
}

// sort=played orders a music library by the viewer's plays, then title; another library
// refuses it.
// Invariant: I-410
func TestWebUI_Media_SortPlayed(t *testing.T) {
	u := watchUI(t)
	for range 2 {
		u.report(`{"path":"/music/Bonobo/Mix/02.mp3","played":true}`)
	}
	u.report(`{"path":"/music/Agnes/Aventine/01.mp3","played":true}`)
	var lib mediaLib
	if code := u.mediaJSON("lib=/music&sort=played", &lib); code != 200 {
		t.Fatal(code)
	}
	if got := paths(lib.Items); !reflect.DeepEqual(got, []string{"album:/music/Bonobo/Mix", "album:/music/Agnes/Aventine"}) || lib.Items[0].Plays != 2 {
		t.Fatalf("played: %v %+v", got, lib.Items)
	}
	if code := u.mediaJSON("lib=/films&sort=played", nil); code != 400 {
		t.Fatalf("films by plays: %d", code)
	}
}

// An album's item lists its tracks' plays and a long track's position.
// Invariant: I-410
func TestWebUI_Media_AlbumTrackPlays(t *testing.T) {
	u := watchUI(t)
	u.report(`{"path":"/music/Bonobo/Mix/01.mp3","played":true}`)
	u.pos("/music/Bonobo/Mix/01.mp3", 41*minute, 72*minute, true)
	u.pos("/music/Bonobo/Mix/02.mp3", 1*minute, 3*minute, true)
	var album webui.MediaItem
	if code := u.mediaJSON("item=/music/Bonobo/Mix", &album); code != 200 {
		t.Fatal(code)
	}
	if len(album.TrackPlay) != 2 || album.TrackPlay[0].Name != "01.mp3" || album.TrackPlay[0].Plays != 1 || album.TrackPlay[0].Progress == nil ||
		album.TrackPlay[1].Progress != nil || album.Plays != 1 {
		t.Fatalf("tracks %+v plays %d", album.TrackPlay, album.Plays)
	}
	_ = testharness.Pattern
}
