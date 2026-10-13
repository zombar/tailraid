package search

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var corpus = flag.String("corpus", "", "a file listing of a media volume, one path per line below its library folders (TestNames_Corpus)")

var contexts = map[string]Context{"movies": CtxMovies, "tv": CtxTV, "music": CtxMusic, "roms": CtxRoms}

// field is one field of n by the names testdata/names.tsv uses.
func field(n Name, key string) string {
	ints := func(l []int) string {
		s := make([]string, len(l))
		for i, v := range l {
			s[i] = strconv.Itoa(v)
		}
		return strings.Join(s, ",")
	}
	switch key {
	case "title":
		return n.Title
	case "year":
		return strconv.Itoa(n.Year)
	case "date":
		return n.Date
	case "season":
		return strconv.Itoa(n.Season)
	case "episode":
		return strconv.Itoa(n.Episode)
	case "to":
		return strconv.Itoa(n.EpisodeTo)
	case "seasons":
		return ints(n.Seasons)
	case "eptitle":
		return n.EpisodeTitle
	case "part":
		return strconv.Itoa(n.Part)
	case "parts":
		return strconv.Itoa(n.Parts)
	case "abs":
		return strconv.Itoa(n.Absolute)
	case "disc":
		return strconv.Itoa(n.Disc)
	case "discs":
		return strconv.Itoa(n.Discs)
	case "track":
		return strconv.Itoa(n.Track)
	case "group":
		return n.Group
	case "edition":
		return n.Edition
	case "quality":
		return strings.Join(n.Quality, ",")
	case "langs":
		return strings.Join(n.Languages, ",")
	case "regions":
		return strings.Join(n.Regions, ",")
	case "rev":
		return n.Revision
	case "flags":
		return strings.Join(n.Flags, ",")
	case "titleid":
		return n.TitleID
	case "role":
		return n.Role.String()
	}
	return "?" + key
}

// Every row of testdata/names.tsv, real names from a volume's films, shows, music and
// ROMs, gives the fields it lists; the old helpers agree with Analyze.
// Invariant: I-276
func TestNames_Table(t *testing.T) {
	f, err := os.Open("testdata/names.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, rows := CtxAny, 0
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if c, ok := strings.CutPrefix(text, "## lib="); ok {
			ctx = contexts[c]
			continue
		}
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		cols := strings.Split(text, "\t")
		if len(cols) != 3 {
			t.Fatalf("line %d: %d columns", line, len(cols))
		}
		var dirs []string
		if cols[1] != "-" {
			dirs = strings.Split(cols[1], "|")
		}
		n := Analyze(cols[0], dirs, ctx)
		rows++
		for kv := range strings.SplitSeq(cols[2], ";") {
			k, want, _ := strings.Cut(kv, "=")
			if got := field(n, k); got != want {
				t.Errorf("line %d %q: %s = %q, want %q", line, cols[0], k, got, want)
			}
		}
	}
	if rows < 125 {
		t.Fatalf("only %d rows", rows)
	}
}

// Analyze never panics and gives the same answer twice.
// Invariant: I-276
func FuzzAnalyze(f *testing.F) {
	for _, s := range []string{"Show.S01E02.720p", "[G] A - 012 [1080p].mkv", "Zelda (U) [!].z64", "1-01 x.mp3", "(", "[", "Part 1 of 2", "2024.03.14", "D", "CD ii", ""} {
		f.Add(s, "Season 1", uint8(2))
	}
	f.Fuzz(func(t *testing.T, name, dir string, c uint8) {
		ctx := Context(c % 5)
		a, b := Analyze(name, []string{dir}, ctx), Analyze(name, []string{dir}, ctx)
		if fmt.Sprintf("%+v", a) != fmt.Sprintf("%+v", b) {
			t.Fatalf("%q: two answers", name)
		}
	})
}

// junkLeft reports a release token left in a title.
func junkLeft(title string) string {
	for w := range strings.FieldsSeq(title) {
		l := strings.ToLower(strings.Trim(w, "[]()"))
		if quality[l] && l != "web" && l != "dvd" && l != "ts" || l == "x264" || l == "x265" || strings.HasPrefix(l, "www.") {
			return w
		}
	}
	return ""
}

// The analyser over a whole volume's listing (`-corpus`, never committed): no title is
// empty or keeps a release token, and every video under a TV library has an episode
// number, a part, a date or an absolute number. Skipped without the flag.
// Invariant: I-276
func TestNames_Corpus(t *testing.T) {
	if *corpus == "" {
		t.Skip("no -corpus listing")
	}
	b, err := os.ReadFile(*corpus)
	if err != nil {
		t.Fatal(err)
	}
	libs := map[string]Context{"films": CtxMovies, "tv": CtxTV, "music": CtxMusic, "roms": CtxRoms}
	var bad []string
	for p := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		parts := strings.Split(p, "/")
		ctx, ok := libs[parts[0]]
		if !ok || len(parts) < 2 {
			continue
		}
		name := parts[len(parts)-1]
		dirs := slices.Clone(parts[1 : len(parts)-1])
		slices.Reverse(dirs)
		if IsText(name) || ext(name) == ".jpg" || ext(name) == ".nfo" || strings.HasPrefix(name, ".") {
			continue
		}
		if slices.ContainsFunc(dirs, func(d string) bool { return RoleOf(d) == RoleExtras }) {
			continue
		}
		n := Analyze(name, dirs, ctx)
		switch {
		case n.Role != RoleNone:
		case n.Title == "":
			bad = append(bad, "empty title: "+p)
		case junkLeft(n.Title) != "":
			bad = append(bad, fmt.Sprintf("%q left in the title %q: %s", junkLeft(n.Title), n.Title, p))
		case ctx == CtxTV && IsVideo(name) && n.Episode == 0 && n.Date == "" && n.Absolute == 0:
			bad = append(bad, "no episode: "+p)
		}
	}
	for _, s := range bad {
		t.Error(s)
	}
	t.Logf("%d names, %d flagged", strings.Count(string(b), "\n"), len(bad))
}

// RoleOf on the folder names of a real music library: a disc folder is no album, a
// category or format folder is walked through.
// Invariant: I-276
func TestNames_FolderRoles(t *testing.T) {
	for dir, want := range map[string]Role{
		"CD1": RoleDisc, "Disc 2 (Mars)": RoleDisc, "DISC TWO": RoleDisc, "D1": RoleDisc, "Discovery": RoleNone, "Disco Volante": RoleNone,
		"Dark Side": RoleNone, "1 Studio": RoleCategory, "Studio Albums": RoleCategory, "1983 - MP3 CBR Original": RoleFormat,
		"Moderat - III [Deluxe Edition] (2016) 320": RoleNone, "Subs": RoleExtras, "Keys-22.1.0": RoleExtras, "Season 2": RoleSeason,
		"USA": RoleRegion, "U": RoleNone, "Japan": RoleRegion, "Iron Maiden": RoleNone, path.Base("x/Albums"): RoleCategory,
	} {
		if got := RoleOf(dir); got != want {
			t.Errorf("RoleOf(%q) = %v, want %v", dir, got, want)
		}
	}
}
