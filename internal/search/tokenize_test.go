package search

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// The index, the names and the queries fold alike: letters and digits, NFKC, lowercase,
// marks dropped, at least two characters, CJK one word per character, offsets in bytes.
// Invariant: I-207
func TestTokenize_Table(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []Word
	}{
		{"", nil},
		{"a", nil},
		{"ab", []Word{{"ab", 0}}},
		{"Hello, World!", []Word{{"hello", 0}, {"world", 7}}},
		{"Agnès Obel", []Word{{"agnes", 0}, {"obel", 7}}},
		{"Café", []Word{{"cafe", 0}}},
		{"ﬁle", []Word{{"file", 0}}}, // NFKC: the ligature is two letters
		{"track 07 x 2019", []Word{{"track", 0}, {"07", 6}, {"2019", 11}}},
		{"snake_case-and.dots", []Word{{"snake", 0}, {"case", 6}, {"and", 11}, {"dots", 15}}},
		{"東京 tower", []Word{{"東", 0}, {"京", 3}, {"tower", 7}}},
		{"ÉCOLE", []Word{{"ecole", 0}}},
		{"bad\xffbytes", []Word{{"bad", 0}, {"bytes", 4}}},
	} {
		if got := Words(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Words(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if Fold("Agnès") != Fold("AGNES") {
		t.Fatal("a query folds as the index does")
	}
}

// Invariant: I-207
func FuzzTokenize(f *testing.F) {
	for _, s := range []string{"Agnès Obel", "東京", "a\xffb", "ﬁ", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, w := range Words(s) {
			if w.Off < 0 || w.Off >= len(s) || !utf8.ValidString(w.Text) || w.Text == "" {
				t.Fatalf("%q: bad word %+v", s, w)
			}
			if w.Text != Fold(w.Text) {
				t.Fatalf("%q: %q is not folded", s, w.Text)
			}
			if strings.ContainsAny(w.Text, " \t\n.,") {
				t.Fatalf("%q: separator inside %q", s, w.Text)
			}
		}
	})
}
