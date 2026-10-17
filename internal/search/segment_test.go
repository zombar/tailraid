package search

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

func sampleSegment(nTerms int) ([]Doc, map[string][]Posting, []byte) {
	docs := []Doc{{Ino: 3, Gen: 1, Kind: KindText}, {Ino: 9, Gen: 2, Kind: KindAudio, Title: "Riverside", Artist: "Agnès Obel", Album: "Philharmonics", Genre: "Pop", Year: 2010}}
	docs[0].Chunks[0] = 7
	terms := map[string][]Posting{}
	for i := range nTerms {
		terms[fmt.Sprintf("term%04d", i)] = []Posting{{Doc: i % 2, TF: i + 1, Off: i * 3}}
	}
	cat := []Entry{{Ino: 4, Lib: 2, Kind: EntMovie, Title: "Arrival", SortTitle: "arrival", Year: 2016, Genres: []string{"Drama", "Sci-Fi"}, People: []string{"Denis Villeneuve"}, Season: -1, Poster: "poster.jpg"}}
	return docs, terms, Encode(5, []uint64{4, 2}, 77, docs, terms, cat)
}

// A segment decodes to what was written, and any truncation or flipped byte fails its
// trailer.
// Invariant: I-206
func TestSegment_RoundTrip_TrailerRejectsTruncation(t *testing.T) {
	docs, terms, b := sampleSegment(70)
	r, err := Decode(5, b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Chain, []uint64{4, 2}) || r.Applied != 77 || r.NumDocs() != 2 {
		t.Fatalf("header %+v", r)
	}
	for i, d := range docs {
		if got := r.Doc(i); !reflect.DeepEqual(got, d) {
			t.Fatalf("doc %d: %+v, want %+v", i, got, d)
		}
		if j, ok := r.FindDoc(d.Ino); !ok || j != i {
			t.Fatalf("FindDoc(%d) = %d %v", d.Ino, j, ok)
		}
	}
	if _, ok := r.FindDoc(5); ok {
		t.Fatal("found an absent inode")
	}
	for term, want := range terms {
		off, ok := r.Lookup(term)
		if !ok || !reflect.DeepEqual(r.Postings(off), want) {
			t.Fatalf("term %s: %v %v", term, ok, r.Postings(off))
		}
	}
	if got := r.Prefix("term006"); len(got) != 10 {
		t.Fatalf("prefix term006: %d terms", len(got))
	}
	if cat := r.Catalog(); len(cat) != 1 || cat[0].Title != "Arrival" || cat[0].Season != -1 || !reflect.DeepEqual(cat[0].Genres, []string{"Drama", "Sci-Fi"}) {
		t.Fatalf("catalogue %+v", cat)
	}
	for _, n := range []int{0, 1, len(b) / 2, len(b) - 1} {
		if _, err := Decode(5, b[:n]); !errors.Is(err, ErrSegment) {
			t.Fatalf("truncated to %d: %v", n, err)
		}
	}
	for _, i := range []int{0, 10, len(b) / 2, len(b) - 1} {
		c := append([]byte(nil), b...)
		c[i] ^= 1
		if _, err := Decode(5, c); !errors.Is(err, ErrSegment) {
			t.Fatalf("byte %d flipped: %v", i, err)
		}
	}
}

// The dictionary's blocks at their edges: every term is found, none invented.
// Invariant: I-206
func TestSegment_SizeBoundaries_Table(t *testing.T) {
	for _, n := range []int{0, 1, dictBlock - 1, dictBlock, dictBlock + 1, 3*dictBlock + 5} {
		_, terms, b := sampleSegment(n)
		r, err := Decode(1, b)
		if err != nil {
			t.Fatalf("%d terms: %v", n, err)
		}
		keys := make([]string, 0, len(terms))
		for k := range terms {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := r.Lookup(k); !ok {
				t.Fatalf("%d terms: %s missing", n, k)
			}
		}
		if _, ok := r.Lookup("term"); ok {
			t.Fatalf("%d terms: a prefix found as a term", n)
		}
		if _, ok := r.Lookup("zzz"); ok {
			t.Fatalf("%d terms: past the end found", n)
		}
		if got := len(r.Prefix("term")); got != n {
			t.Fatalf("%d terms: prefix gave %d", n, got)
		}
		if got := len(r.Prefix("")); got != n {
			t.Fatalf("%d terms: empty prefix gave %d", n, got)
		}
	}
}

// Invariant: I-206
func FuzzSegmentDecode(f *testing.F) {
	_, _, b := sampleSegment(40)
	f.Add(b)
	f.Add([]byte("TRSI"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := Decode(1, b)
		if err != nil {
			return
		}
		for i := 0; i < r.NumDocs() && i < 100; i++ {
			_ = r.Doc(i)
			_, _ = r.FindDoc(meta.Ino(i))
		}
		for _, off := range r.Prefix("") {
			_ = r.Postings(off)
		}
		_ = r.Catalog()
	})
}

// A catalogue section written before the name analyser (segment version 1) still reads,
// without the new fields; version 4 round-trips them, a game's and the directors, and a
// version 3 section reads its directors as unknown.
// Invariant: I-276, I-278, I-412
func TestCatalog_SectionVersions(t *testing.T) {
	e := Entry{
		Ino: 7, Lib: 2, Kind: EntEpisode, Title: "Some Guest", Season: -1, Genres: []string{"Talk"}, People: []string{"Host"},
		Date: "2024-03-14", Absolute: 12, Edition: "Extended", Quality: []string{"1080p", "WEB"}, Languages: []string{"ITA"}, Discs: 2,
		Platform: "Super Nintendo", Regions: []string{"USA"}, Revision: "Rev 2", Flags: []string{"proto"}, Developer: "Rare", Publisher: "Nintendo", Players: "1-2",
	}
	e.Directors = 1
	got := decodeCatalog(encodeCatalog([]Entry{e, e}), 4)
	if len(got) != 2 || !reflect.DeepEqual(got[1], e) {
		t.Fatalf("v4 round trip:\n got %+v\nwant %+v", got, e)
	}
	// version 3: the same layout without the directors, which read as unknown
	v4 := encodeCatalog([]Entry{e})
	got = decodeCatalog(v4[:len(v4)-1], 3)
	e3 := e
	e3.Directors = -1
	if len(got) != 1 || !reflect.DeepEqual(got[0], e3) {
		t.Fatalf("v3 section:\n got %+v\nwant %+v", got, e3)
	}
	// version 1: the same layout up to the lists, nothing after them
	var w buf
	w.int(1)
	w.uvarint(7)
	w.uvarint(2)
	w.uvarint(0)
	w.u8(EntEpisode)
	w.uvarint(0)
	for _, s := range []string{"Old", "old", "", "", "", "", "", "", "", "", ""} {
		w.str(s)
	}
	for _, n := range []int{2001, 0, 0, 3, 3, 0} {
		w.int(n)
	}
	w.int(0)
	w.int(0)
	old := decodeCatalog(w.b, 1)
	if len(old) != 1 || old[0].Title != "Old" || old[0].Year != 2001 || old[0].Episode != 3 || old[0].Season != -1 || old[0].Date != "" {
		t.Fatalf("v1 section: %+v", old)
	}
}
