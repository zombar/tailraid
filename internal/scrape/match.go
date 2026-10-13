package scrape

import (
	"strings"

	"github.com/zombar/tailraid/internal/search"
)

// norm is a title as matching compares it: the index's words (NFKC, case-folded, marks
// dropped, punctuation as spaces, docs/18 §3) joined by one space.
func norm(s string) string { return strings.Join(search.Terms(s), " ") }

// Clear is the index of the one candidate whose title or original title equals title's
// and, when year is not 0, whose year equals it too (I-301); ok is false when none or
// more than one does.
func Clear(title string, year int, cs []Candidate) (int, bool) {
	q := norm(title)
	found := -1
	for i, c := range cs {
		if q == "" || norm(c.Title) != q && norm(c.Original) != q || year != 0 && c.Year != year {
			continue
		}
		if found >= 0 {
			return -1, false
		}
		found = i
	}
	return found, found >= 0
}

// ClearAlbum is the index of the one release group whose artist and title both equal
// the tags' (I-301).
func ClearAlbum(artist, title string, cs []Candidate) (int, bool) {
	a, t := norm(artist), norm(title)
	found := -1
	for i, c := range cs {
		if t == "" || norm(c.Title) != t || a != "" && norm(c.Artist) != a {
			continue
		}
		if found >= 0 {
			return -1, false
		}
		found = i
	}
	return found, found >= 0
}
