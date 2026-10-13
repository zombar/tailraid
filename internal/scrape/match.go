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

// libretroName is a game's name as the thumbnail set files it: the characters
// &*/:`<>?\| become _ (RetroArch's thumbnail rule).
func libretroName(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("&*/:`<>?\\|", r) {
			return '_'
		}
		return r
	}, s)
}

// baseTitle is a No-Intro name's title: what comes before its first tag.
func baseTitle(s string) string {
	if i := strings.IndexAny(s, "(["); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// ClearGame matches a game against a system's box art names (I-301): the name its file
// already has; else the only name of the same title (the analysed title or the file's
// own); else the only one of those tagged with one of the game's regions. cs are the
// same-title names, best first, for the chooser when nothing is clear.
func ClearGame(key, title string, regions, names []string) (cs []Candidate, idx int, ok bool) {
	exact := libretroName(key)
	t, k := norm(title), norm(baseTitle(key))
	var same []string
	for _, n := range names {
		if n == exact {
			return []Candidate{{ID: n, Title: n}}, 0, true
		}
		if b := norm(baseTitle(n)); b != "" && (b == t || b == k) {
			same = append(same, n)
		}
	}
	tagged := func(n string) bool {
		for _, r := range regions {
			if strings.Contains(n, "("+r) || strings.Contains(n, ", "+r) {
				return true
			}
		}
		return false
	}
	var regional []Candidate
	for _, n := range same {
		if tagged(n) {
			regional = append(regional, Candidate{ID: n, Title: n})
		}
	}
	for _, n := range same {
		if !tagged(n) {
			cs = append(cs, Candidate{ID: n, Title: n})
		}
	}
	cs = append(regional, cs...)
	switch {
	case len(same) == 1:
		return cs, 0, true
	case len(regional) == 1:
		return cs, 0, true
	}
	return cs, -1, false
}
