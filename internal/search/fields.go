package search

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/meta"
)

// Field terms (docs/18 §6a): one grammar for every search field and scope.

// Field is what a query term asks of a catalogue item.
type Field uint8

// Fields, in the order suggestions list them after FieldWord.
const (
	FieldWord     Field = iota // a bare word: title, people, genres, artist or plot
	FieldActor                 // the people after the directors
	FieldDirector              // the directors
	FieldPerson                // actors or directors
	FieldGenre
	FieldTitle // title or sort title
	FieldArtist
	FieldYear
	FieldKind
	nFields
)

var fieldByName = map[string]Field{
	"actor": FieldActor, "director": FieldDirector, "person": FieldPerson, "genre": FieldGenre,
	"title": FieldTitle, "artist": FieldArtist, "year": FieldYear, "kind": FieldKind,
}

// String is the field's name in a query.
func (f Field) String() string {
	for n, v := range fieldByName {
		if v == f {
			return n
		}
	}
	return "word"
}

// Query bounds (docs/18 §6a, I-411).
const (
	QueryMax = 1024
	TermsMax = 16
)

// Term is one condition of a query: a field and its folded value.
type Term struct {
	Field  Field
	Value  string // folded words joined by one space
	Quoted bool   // the whole value must equal a field value
	Prefix bool   // the query's last bare word: its last word matches as a prefix
	YearLo int    // FieldYear
	YearHi int
	Kind   uint8 // FieldKind: an Ent* kind
}

// Query is a parsed query: its terms, ANDed.
type Query struct{ Terms []Term }

// QueryError is a malformed query, at a byte offset of it (a 400, I-411).
type QueryError struct {
	At  int
	Msg string
}

func (e *QueryError) Error() string { return fmt.Sprintf("query: %s at %d", e.Msg, e.At) }

var kindByName = map[string]uint8{"movie": EntMovie, "show": EntShow, "episode": EntEpisode, "album": EntAlbum, "game": EntGame}

// ParseQuery parses q by docs/18 §6a's table (I-411). It never panics.
func ParseQuery(q string) (Query, error) {
	var out Query
	if len(q) > QueryMax {
		return out, &QueryError{At: QueryMax, Msg: "longer than QueryMax"}
	}
	add := func(t Term, at int) error {
		if len(out.Terms) >= TermsMax {
			return &QueryError{At: at, Msg: "more than TermsMax terms"}
		}
		out.Terms = append(out.Terms, t)
		return nil
	}
	i := 0
	for i < len(q) {
		if q[i] == ' ' || q[i] == '\t' {
			i++
			continue
		}
		start := i
		field := FieldWord
		if j := strings.IndexByte(q[i:], ':'); j > 0 && isFieldName(q[i:i+j]) {
			f, ok := fieldByName[strings.ToLower(q[i:i+j])]
			if !ok {
				return Query{}, &QueryError{At: start, Msg: "unknown field " + strconv.Quote(q[i:i+j])}
			}
			field = f
			i += j + 1
		}
		quoted := i < len(q) && q[i] == '"'
		var raw string
		if quoted {
			end := strings.IndexByte(q[i+1:], '"')
			if end < 0 {
				return Query{}, &QueryError{At: i, Msg: "unclosed quote"}
			}
			raw = q[i+1 : i+1+end]
			i += end + 2
		} else {
			end := strings.IndexAny(q[i:], " \t")
			if end < 0 {
				end = len(q) - i
			}
			raw = q[i : i+end]
			i += end
		}
		t := Term{Field: field, Quoted: quoted}
		switch field {
		case FieldYear:
			lo, hi, ok := parseYear(raw)
			if !ok {
				return Query{}, &QueryError{At: start, Msg: "malformed year " + strconv.Quote(raw)}
			}
			t.YearLo, t.YearHi, t.Value = lo, hi, raw
		case FieldKind:
			k, ok := kindByName[strings.ToLower(raw)]
			if !ok {
				return Query{}, &QueryError{At: start, Msg: "unknown kind " + strconv.Quote(raw)}
			}
			t.Kind, t.Value = k, strings.ToLower(raw)
		default:
			words := Terms(raw)
			if len(words) == 0 && field == FieldWord && !quoted {
				continue // a bare letter while typing: no word yet
			}
			if len(words) == 0 {
				return Query{}, &QueryError{At: start, Msg: "empty value"}
			}
			if field == FieldWord && !quoted {
				for _, w := range words { // a bare token of several words is several terms
					if err := add(Term{Field: FieldWord, Value: w}, start); err != nil {
						return Query{}, err
					}
				}
				if i >= len(q) {
					out.Terms[len(out.Terms)-1].Prefix = true
				}
				continue
			}
			t.Value = strings.Join(words, " ")
		}
		if err := add(t, start); err != nil {
			return Query{}, err
		}
	}
	return out, nil
}

// isFieldName reports whether s looks like a field name: letters only.
func isFieldName(s string) bool {
	for i := range len(s) {
		if c := s[i] | 0x20; c < 'a' || c > 'z' {
			return false
		}
	}
	return s != ""
}

// parseYear reads n, a decade nnnx or a range a-b.
func parseYear(s string) (lo, hi int, ok bool) {
	num := func(s string) (int, bool) {
		if len(s) != 4 {
			return 0, false
		}
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= 1000
	}
	switch {
	case len(s) == 4 && (s[3] == 'x' || s[3] == 'X'):
		d, ok := num(s[:3] + "0")
		return d, d + 9, ok
	case strings.Contains(s, "-"):
		a, b, _ := strings.Cut(s, "-")
		lo, ok1 := num(a)
		hi, ok2 := num(b)
		return lo, hi, ok1 && ok2 && lo <= hi
	default:
		n, ok := num(s)
		return n, n, ok
	}
}

// fieldTable is the node's field index over the catalogue (docs/18 §6a, I-412): for each
// field its folded values and its words, with the entries that carry them. Built from the
// catalogue, never stored.
type fieldTable struct {
	es     []Entry
	byIno  map[meta.Ino]int
	vals   [nFields]map[string][]int // a whole folded value → entries
	words  [nFields]map[string][]int // a folded word → entries
	sorted [nFields][]string         // the words, sorted, for prefixes
	shown  [nFields]map[string]string
}

// valuesOf lists e's values of a field, as stored (display form).
func valuesOf(e *Entry, f Field) []string {
	dirs := max(e.Directors, 0)
	dirs = min(dirs, len(e.People))
	switch f {
	case FieldActor:
		if e.Directors < 0 {
			return nil
		}
		return e.People[dirs:]
	case FieldDirector:
		if e.Directors < 0 {
			return nil
		}
		return e.People[:dirs]
	case FieldPerson:
		return e.People
	case FieldGenre:
		return e.Genres
	case FieldTitle:
		if e.SortTitle != "" && e.SortTitle != e.Title {
			return []string{e.Title, e.SortTitle}
		}
		return []string{e.Title}
	case FieldArtist:
		if e.Artist == "" {
			return nil
		}
		return []string{e.Artist}
	case FieldWord:
		out := append([]string{e.Title, e.SortTitle, e.Artist, e.Plot}, e.People...)
		return append(out, e.Genres...)
	case FieldYear, FieldKind, nFields:
	}
	return nil
}

func buildFields(es []Entry) *fieldTable {
	t := &fieldTable{es: es, byIno: make(map[meta.Ino]int, len(es))}
	for f := range nFields {
		t.vals[f], t.words[f], t.shown[f] = map[string][]int{}, map[string][]int{}, map[string]string{}
	}
	addOnce := func(m map[string][]int, k string, i int) {
		if l := m[k]; len(l) == 0 || l[len(l)-1] != i {
			m[k] = append(l, i)
		}
	}
	for i := range es {
		t.byIno[es[i].Ino] = i
		for f := range nFields {
			for _, v := range valuesOf(&es[i], f) {
				ws := Terms(v)
				if len(ws) == 0 {
					continue
				}
				key := strings.Join(ws, " ")
				addOnce(t.vals[f], key, i)
				if _, ok := t.shown[f][key]; !ok {
					t.shown[f][key] = v
				}
				for _, w := range ws {
					addOnce(t.words[f], w, i)
				}
			}
		}
	}
	for f := range nFields {
		for w := range t.words[f] {
			t.sorted[f] = append(t.sorted[f], w)
		}
		slices.Sort(t.sorted[f])
	}
	return t
}

// matchTerm is the set of entries one term matches.
func (t *fieldTable) matchTerm(term Term) map[int]bool {
	out := map[int]bool{}
	switch term.Field {
	case FieldYear:
		for i := range t.es {
			if y := t.es[i].Year; y >= term.YearLo && y <= term.YearHi {
				out[i] = true
			}
		}
		return out
	case FieldKind:
		for i := range t.es {
			if t.es[i].Kind == term.Kind || (term.Kind == EntShow && t.es[i].Kind == EntSeason) {
				out[i] = true
			}
		}
		return out
	case FieldActor, FieldDirector, FieldPerson, FieldGenre, FieldTitle, FieldArtist, FieldWord, nFields:
	}
	f := term.Field
	if term.Quoted {
		for _, i := range t.vals[f][term.Value] {
			out[i] = true
		}
		return out
	}
	words := strings.Fields(term.Value)
	for wi, w := range words {
		set := map[int]bool{}
		if term.Prefix && wi == len(words)-1 {
			s := t.sorted[f]
			for j, _ := slices.BinarySearch(s, w); j < len(s) && strings.HasPrefix(s[j], w); j++ {
				for _, i := range t.words[f][s[j]] {
					set[i] = true
				}
			}
		} else {
			for _, i := range t.words[f][w] {
				set[i] = true
			}
		}
		if wi == 0 {
			out = set
			continue
		}
		for i := range out {
			if !set[i] {
				delete(out, i)
			}
		}
	}
	return out
}

// match is the set of items every term matches, each term lifted to its items first, so
// an episode's actor and its show's genre meet in the show; no terms match every item.
func (t *fieldTable) match(terms []Term) map[int]bool {
	var cur map[int]bool
	for _, term := range terms {
		set := map[int]bool{}
		for _, it := range t.items(t.matchTerm(term)) {
			set[it] = true
		}
		if cur == nil {
			cur = set
			continue
		}
		for i := range cur {
			if !set[i] {
				delete(cur, i)
			}
		}
	}
	if cur == nil {
		cur = make(map[int]bool, len(t.es))
		for i := range t.es {
			if it, ok := t.item(i); ok {
				cur[it] = true
			}
		}
	}
	return cur
}

// item lifts an entry to the item the Media tab lists: a season or an episode to its
// show; an artist or a platform is no item.
func (t *fieldTable) item(i int) (int, bool) {
	for range 3 {
		switch t.es[i].Kind {
		case EntMovie, EntShow, EntAlbum, EntGame:
			return i, true
		case EntSeason, EntEpisode:
			p, ok := t.byIno[t.es[i].Parent]
			if !ok {
				return 0, false
			}
			i = p
		default:
			return 0, false
		}
	}
	return 0, false
}

func sortedKeys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// items lifts a set of entries to their items, deduplicated, in entry order.
func (t *fieldTable) items(set map[int]bool) []int {
	seen := map[int]bool{}
	for i := range set {
		if it, ok := t.item(i); ok {
			seen[it] = true
		}
	}
	out := make([]int, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// files lists the files of the given items (docs/18 §6a): a film's or an episode's video,
// every episode's of a show, an album's and a game folder's files, a game file itself.
func (t *fieldTable) files(src Source, items []int) []meta.Ino {
	want := map[int]bool{}
	for _, i := range items {
		want[i] = true
	}
	var out []meta.Ino
	for i := range t.es {
		e := &t.es[i]
		switch e.Kind {
		case EntMovie, EntEpisode:
			if it, ok := t.item(i); ok && want[it] && e.Video != 0 {
				out = append(out, e.Video)
			}
		case EntAlbum, EntGame:
			if !want[i] {
				continue
			}
			in, ok := src.GetInode(e.Ino)
			if ok && in.Type == meta.TFile {
				out = append(out, e.Ino)
				continue
			}
			kids, err := src.ReadDir(e.Ino)
			if err != nil {
				continue
			}
			for j := range kids {
				if kids[j].Type == meta.TFile {
					out = append(out, kids[j].Ino)
				}
			}
		}
	}
	return out
}

// Items answers a query's catalogue items (scope media): sorted by sort title, then
// title, then inode (I-412).
func (t *fieldTable) Items(q Query) []Entry {
	idx := sortedKeys(t.match(q.Terms))
	out := make([]Entry, len(idx))
	for j, i := range idx {
		out[j] = t.es[i]
	}
	slices.SortFunc(out, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(Fold(cmp.Or(a.SortTitle, a.Title)), Fold(cmp.Or(b.SortTitle, b.Title))), cmp.Compare(a.Ino, b.Ino))
	})
	return out
}

// Suggestion is one value of a field with the items it matches by kind.
type Suggestion struct {
	Value  string         `json:"value"`
	Counts map[string]int `json:"counts"`
	total  int
}

// SuggestGroup is one field's suggestions.
type SuggestGroup struct {
	Field  string       `json:"field"`
	Values []Suggestion `json:"values"`
}

// Suggest limits (docs/18 §6a, I-413).
const (
	DefaultSuggest = 8
	MaxSuggest     = 50
)

var suggestFields = []Field{FieldActor, FieldDirector, FieldGenre, FieldTitle, FieldArtist}

var kindName = map[uint8]string{EntMovie: "movie", EntShow: "show", EntAlbum: "album", EntGame: "game"}

// Suggest answers the values whose folded words start with the query's last bare word
// and that match an item with the rest of the query (I-413).
// allow, when not nil, says whether an item may be counted (the web UI's Read check).
func (t *fieldTable) Suggest(q Query, limit int, allow func(*Entry) bool) []SuggestGroup {
	out := []SuggestGroup{}
	if len(q.Terms) == 0 {
		return out
	}
	last := q.Terms[len(q.Terms)-1]
	if last.Field != FieldWord || last.Quoted {
		return out
	}
	base := t.match(q.Terms[:len(q.Terms)-1])
	for _, f := range suggestFields {
		var vals []Suggestion
		for key, entries := range t.vals[f] {
			if !hasWordPrefix(key, last.Value) {
				continue
			}
			set := map[int]bool{}
			for _, i := range entries {
				set[i] = true
			}
			s := Suggestion{Value: t.shown[f][key], Counts: map[string]int{}}
			for _, it := range t.items(set) {
				if !base[it] {
					continue
				}
				if allow != nil && !allow(&t.es[it]) {
					continue
				}
				s.Counts[kindName[t.es[it].Kind]]++
				s.total++
			}
			if s.total > 0 {
				vals = append(vals, s)
			}
		}
		slices.SortFunc(vals, func(a, b Suggestion) int {
			return cmp.Or(cmp.Compare(b.total, a.total), cmp.Compare(Fold(a.Value), Fold(b.Value)), cmp.Compare(a.Value, b.Value))
		})
		if len(vals) > limit {
			vals = vals[:limit]
		}
		if len(vals) > 0 {
			out = append(out, SuggestGroup{Field: f.String(), Values: vals})
		}
	}
	return out
}

// hasWordPrefix reports whether one of the words of the folded value starts with p.
func hasWordPrefix(value, p string) bool {
	for w := range strings.FieldsSeq(value) {
		if strings.HasPrefix(w, p) {
			return true
		}
	}
	return false
}
