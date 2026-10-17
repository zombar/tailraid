package scrape

import (
	"context"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/search"
)

// Titles compare with every word kept, however short, and a Roman numeral from II to XX
// read as its number, the same on both sides (docs/18 §11).
// Invariant: I-395
func TestNorm_ShortWordsAndNumerals(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"Tekken 6", "Tekken 5", false},
		{"Tekken 6", "Tekken", false},
		{"Final Fantasy VII", "final fantasy 7", true},
		{"Rocky II", "Rocky 2", true},
		{"Bad Boys II", "Bad Boys", false},
		{"Star Wars: Episode I", "Star Wars Episode 1", false}, // I alone is a word, not a numeral
		{"Mix", "1009", false},
		{"Malcolm X", "Malcolm 10", true},
		{"XXI", "21", false}, // past XX: a word
		{"Crisis Core - Final Fantasy VII", "Crisis Core: Final Fantasy 7", true},
		{"New York, New York", "New York New York", true},
	} {
		if got := norm(tc.a) == norm(tc.b); got != tc.same {
			t.Errorf("norm(%q)=%q norm(%q)=%q: same %v, want %v", tc.a, norm(tc.a), tc.b, norm(tc.b), got, tc.same)
		}
	}
}

// Of an artist's several albums of one title (the self-titled ones), the one whose first
// release year is the tags' is the clear match; with no year none is.
// Invariant: I-395
func TestClearAlbum_SelfTitledByYear(t *testing.T) {
	cs := []Candidate{{ID: "blue", Title: "Weezer", Artist: "Weezer", Year: 1994}, {ID: "green", Title: "Weezer", Artist: "Weezer", Year: 2001}}
	for _, tc := range []struct {
		year int
		want int
	}{{2001, 1}, {1994, 0}, {0, -1}, {2008, -1}} {
		if i, _ := ClearAlbum("Weezer", "Weezer", tc.year, cs); i != tc.want {
			t.Errorf("year %d: %d, want %d", tc.year, i, tc.want)
		}
	}
}

// A disc dump's short region tag finds the set's name with the region's word, and a
// numbered title finds its own number, not its series' (docs/18 §11).
// Invariant: I-395
func TestClearGame_ShortRegionTags(t *testing.T) {
	names := []string{
		"Tekken 6 (Europe) (En,Fr,De,Es,It)", "Tekken 6 (USA)", "Tekken 5 - Dark Resurrection (USA)", "Tekken (Japan)",
		"Crisis Core - Final Fantasy VII (Europe) (En,Fr,De,Es,It)", "Crisis Core - Final Fantasy VII (USA)",
		"R-Type Tactics (Europe) (En,Fr,De)", "Field Commander (Europe)", "Field Commander (USA)",
	}
	for _, tc := range []struct {
		key, title string
		regions    []string
		want       string
		cands      int
	}{
		{"Tekken 6 (EU)", "Tekken 6", []string{"EU"}, "Tekken 6 (Europe) (En,Fr,De,Es,It)", 2},
		{"Crisis Core - Final Fantasy 7 (EU)", "Crisis Core - Final Fantasy 7", []string{"EU"}, "Crisis Core - Final Fantasy VII (Europe) (En,Fr,De,Es,It)", 2},
		{"R-Type Tactics (EU)", "R-Type Tactics", []string{"EU"}, "R-Type Tactics (Europe) (En,Fr,De)", 1},
		{"Field Commander (EU - AU)", "Field Commander", []string{"EU", "AU"}, "Field Commander (Europe)", 2},
		{"Tekken 6 (US)", "Tekken 6", []string{"US"}, "Tekken 6 (USA)", 2},
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

type answerChooser struct{ c Choice }

func (a answerChooser) Choose(search.ScrapeItem, []Candidate) Choice { return a.c }

// Only an explicit "no match" is remembered, through Options.Remember: a skip, a run
// without a terminal and --yes remember nothing (docs/18 §11).
// Invariant: I-394
func TestRun_NoMatchRemembered(t *testing.T) {
	ctx := context.Background()
	film := plan()[:1]
	for _, tc := range []struct {
		name     string
		ch       Chooser
		o        Options
		remember int
		why      string
	}{
		{name: "no match", ch: answerChooser{Choice{Never: true}}, remember: 1, why: "no match (remembered)"},
		{name: "skip", ch: answerChooser{Choice{Skip: true}}, why: "skipped"},
		{name: "no terminal", why: "no clear match"},
		{name: "yes", o: Options{Yes: true}},
	} {
		p, c := newProvider(t)
		p.movies = []map[string]any{{"id": 1, "title": "Arrival", "release_date": "2016-01-01"}, {"id": 2, "title": "Arrival", "release_date": "2016-06-01"}}
		var remembered []search.ScrapeItem
		tc.o.Remember = func(_ context.Context, it search.ScrapeItem) error {
			remembered = append(remembered, it)
			return nil
		}
		sum, err := Run(ctx, c, film, &memWriter{files: map[string][]byte{}}, tc.ch, tc.o)
		if err != nil {
			t.Fatal(err)
		}
		if len(remembered) != tc.remember || tc.remember == 1 && remembered[0].Ino != film[0].Ino {
			t.Errorf("%s: remembered %+v", tc.name, remembered)
		}
		if tc.why != "" && (len(sum.Skipped) != 1 || !strings.Contains(sum.Skipped[0].Why, tc.why)) {
			t.Errorf("%s: skipped %+v, want %q", tc.name, sum.Skipped, tc.why)
		}
	}
}
