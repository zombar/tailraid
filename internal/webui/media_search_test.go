package webui_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"testing"

	"github.com/zombar/tailraid/internal/search"
	"github.com/zombar/tailraid/internal/webui"
)

// searchFixture is two libraries whose sidecars name people and genres.
func searchFixture(t *testing.T) *ui {
	t.Helper()
	u, pass := mediaUI(t)
	u.write("/films/Arrival (2016)/arrival.mkv", 0, []byte("video"))
	u.write("/films/Arrival (2016)/movie.nfo", 0, []byte(`<movie><title>Arrival</title><year>2016</year><genre>Drama</genre><director>Denis Villeneuve</director><actor><name>Amy Adams</name></actor></movie>`))
	u.write("/films/Cast Away (2000)/cast.mkv", 0, []byte("video"))
	u.write("/films/Cast Away (2000)/movie.nfo", 0, []byte(`<movie><title>Cast Away</title><year>2000</year><genre>Drama</genre><actor><name>Tom Hanks</name></actor></movie>`))
	u.write("/shows/Band of Brothers/tvshow.nfo", 0, []byte(`<tvshow><title>Band of Brothers</title><year>2001</year><genre>War</genre><actor><name>Tom Hanks</name></actor></tvshow>`))
	u.write("/shows/Band of Brothers/Season 1/Band.of.Brothers.S01E01.mkv", 0, []byte("v"))
	u.flush()
	for p, k := range map[string]string{"/films": "movies", "/shows": "tv"} {
		if code, _ := u.setFlags(p, `"library":"`+k+`"`); code != 200 {
			t.Fatalf("flag %s: %d", p, code)
		}
	}
	pass()
	return u
}

type mediaHits struct {
	Query string            `json:"query"`
	Libs  []webui.MediaHits `json:"libs"`
}

// GET /api/search?scope=media answers the catalogue items a field query matches, grouped
// by library in the libraries' order as the Media tab lists them; a malformed query is a
// 400 with the offset of the fault, and an unknown scope a 400.
// Invariant: I-412
func TestWebUI_Search_MediaScope(t *testing.T) {
	u := searchFixture(t)
	get := func(q string) (int, mediaHits, string) {
		t.Helper()
		code, _, body := u.get("/api/search?scope=media&q=" + url.QueryEscape(q))
		var out mediaHits
		if code == 200 {
			must(t, json.Unmarshal(body, &out))
		}
		return code, out, string(body)
	}
	code, res, dump := get(`actor:"tom hanks"`)
	if code != 200 || len(res.Libs) != 2 {
		t.Fatalf("%d %s", code, dump)
	}
	if res.Libs[0].Path != "/films" || res.Libs[0].Kind != "movies" || !reflect.DeepEqual(titles(res.Libs[0].Items), []string{"Cast Away"}) {
		t.Fatalf("films: %+v", res.Libs[0])
	}
	if res.Libs[1].Path != "/shows" || !reflect.DeepEqual(titles(res.Libs[1].Items), []string{"Band of Brothers"}) || res.Libs[1].Items[0].Kind != "show" {
		t.Fatalf("shows: %+v", res.Libs[1])
	}
	if it := res.Libs[0].Items[0]; it.Video == "" || it.Path != "/films/Cast Away (2000)" {
		t.Fatalf("an item as the library lists it: %+v", it)
	}
	if code, res, _ := get(`director:"denis villeneuve"`); code != 200 || len(res.Libs) != 1 || res.Libs[0].Items[0].Directors != 1 {
		t.Fatalf("director: %d %+v", code, res)
	}
	if code, res, _ := get(`genre:western`); code != 200 || len(res.Libs) != 0 {
		t.Fatalf("no hit: %d %+v", code, res)
	}
	code, _, body := u.get("/api/search?scope=media&q=" + url.QueryEscape(`actor:"tom`))
	var qe struct {
		Error string
		At    int
	}
	if code != 400 || json.Unmarshal(body, &qe) != nil || qe.At != 6 {
		t.Fatalf("malformed: %d %s", code, body)
	}
	if code, _, _ := u.get("/api/search?scope=files&q=" + url.QueryEscape("colour:red")); code != 400 {
		t.Fatalf("a malformed files query: %d", code)
	}
	if code, _, _ := u.get("/api/search?scope=everything&q=x"); code != 400 {
		t.Fatalf("unknown scope: %d", code)
	}
	// the files scope lists the same items' files
	code, _, body = u.get("/api/search?q=" + url.QueryEscape(`actor:"tom hanks"`))
	var files search.Result
	must(t, json.Unmarshal(body, &files))
	want := []string{"/films/Cast Away (2000)/cast.mkv", "/shows/Band of Brothers/Season 1/Band.of.Brothers.S01E01.mkv"}
	var got []string
	for _, h := range files.Hits {
		got = append(got, h.Path)
	}
	if code != 200 || !reflect.DeepEqual(got, want) {
		t.Fatalf("files scope: %d %v", code, got)
	}
}

// GET /api/search/suggest answers grouped values with counts, limit 8 by default and
// 400 outside 1..50, scope media only.
// Invariant: I-413
func TestWebUI_Search_Suggest(t *testing.T) {
	u := searchFixture(t)
	code, _, body := u.get("/api/search/suggest?scope=media&q=tom")
	var res struct{ Groups []search.SuggestGroup }
	if code != 200 || json.Unmarshal(body, &res) != nil {
		t.Fatalf("%d %s", code, body)
	}
	if len(res.Groups) != 1 || res.Groups[0].Field != "actor" || res.Groups[0].Values[0].Value != "Tom Hanks" ||
		!reflect.DeepEqual(res.Groups[0].Values[0].Counts, map[string]int{"movie": 1, "show": 1}) {
		t.Fatalf("tom: %s", body)
	}
	for _, c := range []struct {
		q    string
		code int
	}{
		{"&limit=1", 200},
		{fmt.Sprintf("&limit=%d", search.MaxSuggest-1), 200},
		{fmt.Sprintf("&limit=%d", search.MaxSuggest), 200},
		{fmt.Sprintf("&limit=%d", search.MaxSuggest+1), 400},
		{"&limit=0", 400},
		{"&limit=x", 400},
	} {
		if code, _, body := u.get("/api/search/suggest?scope=media&q=tom" + c.q); code != c.code {
			t.Errorf("%s: %d %s", c.q, code, body)
		}
	}
	if code, _, _ := u.get("/api/search/suggest?q=tom"); code != 400 {
		t.Fatalf("no scope: %d", code)
	}
	if code, _, _ := u.get("/api/search/suggest?scope=media&q=" + url.QueryEscape(`year:9`)); code != 400 {
		t.Fatalf("malformed: %d", code)
	}
}
