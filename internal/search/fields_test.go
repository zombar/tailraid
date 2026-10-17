package search

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// The grammar of docs/18 §6a, the same for every scope.
// Invariant: I-411
func TestParseQuery_Table(t *testing.T) {
	for _, c := range []struct {
		q    string
		want []Term
		at   int // -1: no error
	}{
		{"", nil, -1},
		{"   ", nil, -1},
		{"tom", []Term{{Field: FieldWord, Value: "tom", Prefix: true}}, -1},
		{"tom ", []Term{{Field: FieldWord, Value: "tom"}}, -1},
		{"Spider-Man far", []Term{{Field: FieldWord, Value: "spider"}, {Field: FieldWord, Value: "man"}, {Field: FieldWord, Value: "far", Prefix: true}}, -1},
		{`actor:"Tom Hanks"`, []Term{{Field: FieldActor, Value: "tom hanks", Quoted: true}}, -1},
		{`ACTOR:hanks genre:drama`, []Term{{Field: FieldActor, Value: "hanks"}, {Field: FieldGenre, Value: "drama"}}, -1},
		{`director:"Denis Villeneuve" arr`, []Term{{Field: FieldDirector, Value: "denis villeneuve", Quoted: true}, {Field: FieldWord, Value: "arr", Prefix: true}}, -1},
		{`"tom hanks"`, []Term{{Field: FieldWord, Value: "tom hanks", Quoted: true}}, -1},
		{"year:1995", []Term{{Field: FieldYear, Value: "1995", YearLo: 1995, YearHi: 1995}}, -1},
		{"year:199x", []Term{{Field: FieldYear, Value: "199x", YearLo: 1990, YearHi: 1999}}, -1},
		{"year:1990-1999", []Term{{Field: FieldYear, Value: "1990-1999", YearLo: 1990, YearHi: 1999}}, -1},
		{"kind:Show", []Term{{Field: FieldKind, Value: "show", Kind: EntShow}}, -1},
		{"person:café", []Term{{Field: FieldPerson, Value: "cafe"}}, -1},
		{"title:x:y", nil, 0},      // the value folds to nothing (one-letter words)
		{"colour:blue", nil, 0},    // unknown field
		{"a b actor:", nil, 4},     // empty value
		{`actor:"tom`, nil, 6},     // unclosed quote
		{"year:95", nil, 0},        // malformed year
		{"year:2000-1990", nil, 0}, // a range backwards
		{"kind:podcast", nil, 0},   // unknown kind
		{"12:30", []Term{{Field: FieldWord, Value: "12"}, {Field: FieldWord, Value: "30", Prefix: true}}, -1}, // digits are no field name
	} {
		got, err := ParseQuery(c.q)
		var qe *QueryError
		switch {
		case c.at >= 0:
			if !errors.As(err, &qe) || qe.At != c.at {
				t.Errorf("ParseQuery(%q) = %v, %v; want an error at %d", c.q, got, err, c.at)
			}
		case err != nil:
			t.Errorf("ParseQuery(%q): %v", c.q, err)
		case !reflect.DeepEqual(got.Terms, c.want):
			t.Errorf("ParseQuery(%q) =\n %+v\nwant\n %+v", c.q, got.Terms, c.want)
		}
	}
}

// The bounds: at most QueryMax bytes and TermsMax terms, {0, 1, max−1, max, max+1}.
// Invariant: I-411
func TestParseQuery_Bounds_Table(t *testing.T) {
	words := func(n int) string {
		w := make([]string, n)
		for i := range w {
			w[i] = fmt.Sprintf("w%d", i)
		}
		return strings.Join(w, " ")
	}
	for _, c := range []struct {
		n  int
		ok bool
	}{{0, true}, {1, true}, {TermsMax - 1, true}, {TermsMax, true}, {TermsMax + 1, false}} {
		if _, err := ParseQuery(words(c.n)); (err == nil) != c.ok {
			t.Errorf("%d terms: %v", c.n, err)
		}
	}
	for _, c := range []struct {
		n  int
		ok bool
	}{{0, true}, {1, true}, {QueryMax - 1, true}, {QueryMax, true}, {QueryMax + 1, false}} {
		q := strings.Repeat(" ", max(c.n-2, 0)) + "ab"[:min(c.n, 2)]
		if _, err := ParseQuery(q); (err == nil) != c.ok {
			t.Errorf("%d bytes: %v", c.n, err)
		}
	}
}

// Invariant: I-411
func FuzzParseQuery(f *testing.F) {
	for _, s := range []string{"", `actor:"tom hanks" genre:drama year:199x arr`, `"`, "a:", "year:1990-", "kind:", "\xff:\"\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		pq, err := ParseQuery(q)
		if err != nil {
			var qe *QueryError
			if !errors.As(err, &qe) || qe.At < 0 || qe.At > len(q) && qe.At != QueryMax {
				t.Fatalf("%q: %v", q, err)
			}
			return
		}
		if len(pq.Terms) > TermsMax {
			t.Fatalf("%q: %d terms", q, len(pq.Terms))
		}
	})
}

// fixture is a movies, a tv and a music library with sidecars naming people and genres.
func fixture(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.library("/films", meta.LibraryMovies)
	e.library("/shows", meta.LibraryTV)
	e.library("/music", meta.LibraryMusic)
	films := []struct{ dir, nfo string }{
		{"Arrival (2016)", `<movie><title>Arrival</title><year>2016</year><genre>Drama</genre><genre>Science Fiction</genre><director>Denis Villeneuve</director><actor><name>Amy Adams</name></actor><actor><name>Jeremy Renner</name></actor></movie>`},
		{"Cast Away (2000)", `<movie><title>Cast Away</title><year>2000</year><genre>Drama</genre><director>Robert Zemeckis</director><actor><name>Tom Hanks</name></actor></movie>`},
		{"Heat (1995)", `<movie><title>Heat</title><year>1995</year><genre>Crime</genre><genre>Thriller</genre><plot>A heist crew.</plot><director>Michael Mann</director><actor><name>Al Pacino</name></actor><actor><name>Tom Hanks Jr</name></actor></movie>`},
		{"Big (1988)", `<movie><title>Big</title><year>1988</year><genre>Comedy</genre><actor><name>Tom Hanks</name></actor><actor><name>Denis Villeneuve</name></actor></movie>`},
	}
	for _, f := range films {
		e.write("/films/"+f.dir+"/film.mkv", []byte("video"))
		e.write("/films/"+f.dir+"/movie.nfo", []byte(f.nfo))
	}
	e.write("/shows/Band of Brothers/tvshow.nfo", []byte(`<tvshow><title>Band of Brothers</title><year>2001</year><genre>Drama</genre><genre>War</genre></tvshow>`))
	e.write("/shows/Band of Brothers/Season 01/Band.of.Brothers.S01E01.mkv", []byte("video"))
	e.write("/shows/Band of Brothers/Season 01/Band.of.Brothers.S01E02.mkv", []byte("video"))
	e.write("/shows/Band of Brothers/Season 01/Band.of.Brothers.S01E01.nfo", []byte(`<episodedetails><title>Currahee</title><season>1</season><episode>1</episode><actor><name>Tom Hanks</name></actor></episodedetails>`))
	for i := range 3 {
		e.write(fmt.Sprintf("/music/Tom Waits/Rain Dogs/%02d.mp3", i+1), id3(0, "TIT2", fmt.Sprintf("Track %d", i), "TPE1", "Tom Waits", "TALB", "Rain Dogs", "TYER", "1985", "TCON", "Rock"))
	}
	e.pass()
	return e
}

func (e *env) table() *fieldTable {
	e.t.Helper()
	x, err := e.svc.index(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return e.svc.fields(x)
}

func (e *env) titles(q string) []string {
	e.t.Helper()
	items, err := e.svc.Media(context.Background(), q, MaxLimit)
	if err != nil {
		e.t.Fatalf("%q: %v", q, err)
	}
	out := []string{}
	for i := range items {
		out = append(out, items[i].Title)
	}
	return out
}

// Field terms match the items whose field holds the value: a word anywhere in it, a
// quoted value whole; actor and director apart; an episode's match lifts to its show.
// Invariant: I-412
func TestFields_Match_Table(t *testing.T) {
	e := fixture(t)
	for _, c := range []struct {
		q    string
		want []string
	}{
		{`actor:"tom hanks"`, []string{"Band of Brothers", "Big", "Cast Away"}}, // never Tom Hanks Jr; the episode lifts to its show
		{`actor:hanks`, []string{"Band of Brothers", "Big", "Cast Away", "Heat"}},
		{`director:"denis villeneuve"`, []string{"Arrival"}},
		{`actor:"denis villeneuve"`, []string{"Big"}},
		{`person:"denis villeneuve"`, []string{"Arrival", "Big"}},
		{`genre:drama`, []string{"Arrival", "Band of Brothers", "Cast Away"}},
		{`genre:"science fiction"`, []string{"Arrival"}},
		{`genre:science`, []string{"Arrival"}},
		{`actor:"tom hanks" genre:drama`, []string{"Band of Brothers", "Cast Away"}},
		{`year:199x`, []string{"Heat"}},
		{`year:1985-2000`, []string{"Big", "Cast Away", "Heat", "Rain Dogs"}},
		{`kind:show`, []string{"Band of Brothers"}},
		{`kind:album`, []string{"Rain Dogs"}},
		{`artist:"tom waits"`, []string{"Rain Dogs"}},
		{`heist`, []string{"Heat"}},                                                    // a bare word reaches the plot
		{`tom`, []string{"Band of Brothers", "Big", "Cast Away", "Heat", "Rain Dogs"}}, // a prefix of people and the artist
		{`title:arr`, []string{}},                                                      // a field value's word is whole, never a prefix
		{`arr`, []string{"Arrival"}},                                                   // the last bare word is
		{`genre:western`, []string{}},
		{``, []string{}},
	} {
		if got := e.titles(c.q); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %v, want %v", c.q, got, c.want)
		}
	}
	if _, err := e.svc.Media(context.Background(), "colour:red", 10); err == nil {
		t.Error("a malformed query answered")
	}
}

// The field table is a pure function of the catalogue: one built twice, or from the
// entries in another order, answers the same, and a new segment rebuilds it.
// Invariant: I-412
func TestFields_TableFromCatalogue_EqualsRebuild(t *testing.T) {
	e := fixture(t)
	cached := e.table()
	es, err := e.svc.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rev := slices.Clone(es)
	slices.Reverse(rev)
	for _, q := range []string{`actor:"tom hanks"`, `genre:drama year:2000-2016`, `tom`, `director:mann`, `kind:movie`} {
		pq, err := ParseQuery(q)
		if err != nil {
			t.Fatal(err)
		}
		a, b, c := cached.Items(pq), buildFields(es).Items(pq), buildFields(rev).Items(pq)
		if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a, c) {
			t.Fatalf("%q: cached %v, rebuilt %v, reversed %v", q, a, b, c)
		}
	}
	e.write("/films/Philadelphia (1993)/film.mkv", []byte("video"))
	e.write("/films/Philadelphia (1993)/movie.nfo", []byte(`<movie><title>Philadelphia</title><year>1993</year><actor><name>Tom Hanks</name></actor></movie>`))
	e.pass()
	if e.table() == cached {
		t.Fatal("a new segment kept the old table")
	}
	if got := e.titles(`actor:"tom hanks" year:199x`); !reflect.DeepEqual(got, []string{"Philadelphia"}) {
		t.Fatalf("after the pass: %v", got)
	}
}

// Both scopes match the same items: the media hits are exactly the items of the files
// hits, for every query of a table.
// Invariant: I-412
func TestSearch_ScopesMatchSameItems(t *testing.T) {
	e := fixture(t)
	ft := e.table()
	itemOfFile := map[meta.Ino]string{}
	for _, it := range sortedKeys(ft.match(nil)) {
		for _, f := range ft.files(e.svc.Src, []int{it}) {
			itemOfFile[f] = ft.es[it].Title
		}
	}
	for _, q := range []string{`actor:"tom hanks"`, `genre:drama`, `year:199x`, `kind:album`, `director:"denis villeneuve"`, `actor:hanks genre:crime`, `genre:western`} {
		media := e.titles(q)
		res := e.svc.Search(context.Background(), q, "", MaxLimit)
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		set := map[string]bool{}
		for _, h := range res.Hits {
			if h.Match != "field" {
				t.Fatalf("%q: a %s hit", q, h.Match)
			}
			set[itemOfFile[h.Ino]] = true
		}
		files := []string{}
		for title := range set {
			files = append(files, title)
		}
		slices.Sort(files)
		got := slices.Clone(media)
		slices.Sort(got)
		if !reflect.DeepEqual(got, files) {
			t.Errorf("%q: media %v, files' items %v", q, got, files)
		}
	}
	// a word beside a field term filters the field's files by name
	res := e.svc.Search(context.Background(), `actor:"tom hanks" s01e02`, "", MaxLimit)
	if got := paths(res); !reflect.DeepEqual(got, []string{"name:/shows/Band of Brothers/Season 01/Band.of.Brothers.S01E02.mkv"}) {
		t.Fatalf("word and field: %v", got)
	}
	if res := e.svc.Search(context.Background(), `actor:"tom`, "", 0); res.Err == nil || res.Err.At != 6 {
		t.Fatalf("a malformed query: %+v", res.Err)
	}
}

// Suggestions: per field in a fixed order the values a prefix starts a word of, counted by
// kind with the rest of the query, ordered by count then value, at most limit.
// Invariant: I-413
func TestSuggest_Table(t *testing.T) {
	e := fixture(t)
	sug := func(q string, limit int) []SuggestGroup {
		t.Helper()
		g, err := e.svc.Suggest(context.Background(), q, limit, nil)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	got := sug("tom", 0)
	want := []SuggestGroup{
		{Field: "actor", Values: []Suggestion{
			{Value: "Tom Hanks", Counts: map[string]int{"movie": 2, "show": 1}, total: 3},
			{Value: "Tom Hanks Jr", Counts: map[string]int{"movie": 1}, total: 1},
		}},
		{Field: "artist", Values: []Suggestion{{Value: "Tom Waits", Counts: map[string]int{"album": 1}, total: 1}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tom:\n %+v\nwant\n %+v", got, want)
	}
	// the rest of the query narrows the counts
	got = sug("genre:drama tom", 0)
	if len(got) != 1 || len(got[0].Values) != 1 || got[0].Values[0].Value != "Tom Hanks" || got[0].Values[0].total != 2 {
		t.Fatalf("genre:drama tom: %+v", got)
	}
	got = sug("vil", 0)
	if len(got) != 2 || got[0].Field != "actor" || got[1].Field != "director" {
		t.Fatalf("vil: %+v", got)
	}
	got = sug("d", 0) // one letter folds to no word: nothing to suggest
	if len(got) != 0 {
		t.Fatalf("d: %+v", got)
	}
	got = sug(`actor:"tom hanks"`, 0) // the last term is no bare word
	if len(got) != 0 {
		t.Fatalf("a field last: %+v", got)
	}
	// allow leaves out what the request may not read
	g, err := e.svc.Suggest(context.Background(), "tom", 0, func(en *Entry) bool { return en.Kind != EntShow })
	if err != nil || g[0].Values[0].total != 2 {
		t.Fatalf("allow: %+v %v", g, err)
	}
}

// Invariant: I-413
func TestSuggest_LimitBoundaries(t *testing.T) {
	es := []Entry{}
	for i := range MaxSuggest + 1 {
		es = append(es, Entry{Ino: meta.Ino(i + 10), Kind: EntMovie, Title: fmt.Sprintf("Film %d", i), People: []string{fmt.Sprintf("Sam %c%c", 'a'+i%26, 'a'+i/26)}})
	}
	ft := buildFields(es)
	pq, err := ParseQuery("sam")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ limit, want int }{{1, 1}, {2, 2}, {MaxSuggest - 1, MaxSuggest - 1}, {MaxSuggest, MaxSuggest}} {
		g := ft.Suggest(pq, c.limit, nil)
		if len(g) != 1 || len(g[0].Values) != c.want {
			t.Errorf("limit %d: %d values", c.limit, len(g[0].Values))
		}
	}
}
