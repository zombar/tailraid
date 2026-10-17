package scrape

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/zombar/tailraid/internal/search"
)

// norm is a title as matching compares it (docs/18 §11, I-395): folded as the index folds
// (NFKC, case-folded, marks dropped, punctuation as spaces, §3), but keeping every word
// however short, since a title's lone digit is its sequel's number, and a Roman numeral
// from II to XX read as its number; the words are joined by one space.
func norm(s string) string {
	ws := strings.FieldsFunc(search.Fold(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.Is(unicode.Mn, r)
	})
	for i, w := range ws {
		if n, ok := roman[w]; ok {
			ws[i] = strconv.Itoa(n)
		}
	}
	return strings.Join(ws, " ")
}

// roman is II..XX as folded words; I is left a word (a pronoun as often as a numeral).
var roman = func() map[string]int {
	m := map[string]int{}
	ones := []string{"", "i", "ii", "iii", "iv", "v", "vi", "vii", "viii", "ix"}
	for n := 2; n <= 20; n++ {
		m[strings.Repeat("x", n/10)+ones[n%10]] = n
	}
	return m
}()

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
// the tags' (I-301), and of several (an artist's self-titled albums) the one whose year
// is the tags' when they have one (I-395).
func ClearAlbum(artist, title string, year int, cs []Candidate) (int, bool) {
	a, t := norm(artist), norm(title)
	var equal []int
	for i, c := range cs {
		if t != "" && norm(c.Title) == t && (a == "" || norm(c.Artist) == a) {
			equal = append(equal, i)
		}
	}
	if len(equal) > 1 && year != 0 {
		var same []int
		for _, i := range equal {
			if cs[i].Year == year {
				same = append(same, i)
			}
		}
		equal = same
	}
	if len(equal) != 1 {
		return -1, false
	}
	return equal[0], true
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
			for _, w := range regionWords(r) {
				if strings.Contains(n, "("+w) || strings.Contains(n, ", "+w) {
					return true
				}
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

// regionNames reads a dump's short region tag as the words No-Intro and Redump names
// carry (docs/18 §11, I-395): a PSP image named "Tekken 6 (EU)" is filed "Tekken 6
// (Europe)".
var regionNames = map[string][]string{
	"EU": {"Europe"}, "EUR": {"Europe"}, "E": {"Europe"},
	"US": {"USA"}, "U": {"USA"}, "NA": {"USA"},
	"JP": {"Japan"}, "JPN": {"Japan"}, "J": {"Japan"},
	"AU": {"Australia"}, "AUS": {"Australia"},
	"UK": {"UK", "United Kingdom"}, "GB": {"UK", "United Kingdom"},
	"FR": {"France"}, "DE": {"Germany"}, "GER": {"Germany"}, "ES": {"Spain"}, "SPA": {"Spain"}, "IT": {"Italy"}, "ITA": {"Italy"},
	"NL": {"Netherlands"}, "SE": {"Sweden"}, "KR": {"Korea"}, "KOR": {"Korea"}, "CN": {"China"}, "CHN": {"China"},
	"BR": {"Brazil"}, "CA": {"Canada"}, "W": {"World"}, "ASIA": {"Asia"},
}

// regionWords is r and what a short tag stands for.
func regionWords(r string) []string {
	return append([]string{r}, regionNames[strings.ToUpper(r)]...)
}
