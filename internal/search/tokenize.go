// Package search is the index kept in the volume (docs/18): the tokenizer, the segment
// codec, the leader's pass, the media catalogue and the query.
package search

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Word is a folded word and the byte offset in the source where it starts.
type Word struct {
	Text string
	Off  int
}

// Fold is the one folding of the index, the names and the queries (docs/18 §3):
// NFKC, lowercase, combining marks dropped, so Café and cafe are the same word.
func Fold(s string) string {
	s = norm.NFD.String(strings.ToLower(norm.NFKC.String(s)))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Words splits s into its words: runs of letters and digits, folded, at least two
// characters long; a CJK character is a word of its own (docs/18 §3). The offset is
// the run's first byte in s.
func Words(s string) []Word {
	var out []Word
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		if w := Fold(s[start:end]); utf8.RuneCountInString(w) >= 2 {
			out = append(out, Word{Text: w, Off: start})
		}
		start = -1
	}
	for i, r := range s {
		switch {
		case isCJK(r):
			flush(i)
			out = append(out, Word{Text: Fold(string(r)), Off: i})
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r):
			if start < 0 {
				start = i
			}
		default:
			flush(i)
		}
	}
	flush(len(s))
	return out
}

// Terms is the words of s as strings, for a query.
func Terms(s string) []string {
	ws := Words(s)
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Text
	}
	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}
