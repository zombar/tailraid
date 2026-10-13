package search

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Context is the kind of library a name is read in: the same words mean different things
// to a film, a track and a game (docs/18 §8a).
type Context uint8

// Contexts.
const (
	CtxAny Context = iota
	CtxMovies
	CtxTV
	CtxMusic
	CtxRoms
)

// Role is what a folder is to the catalogue before it is a title (docs/18 §8a).
type Role uint8

// Folder roles.
const (
	RoleNone     Role = iota
	RoleDisc          // CD1, Disc 2, D1: part of the album or game above it
	RoleSeason        // Season 1, Specials
	RoleCategory      // Albums, 1 Studio, Hacks, Games: walked through
	RoleFormat        // MP3 CBR Original: walked through
	RoleRegion        // USA, Japan: walked through, a game's region
	RoleExtras        // Subs, Sample, Keys-…: never an item
)

var roleWords = [...]string{"none", "disc", "season", "category", "format", "region", "extras"}

func (r Role) String() string { return roleWords[r] }

// Name is what a name says (docs/18 §8a, I-276). Zero values are absent; Season is -1
// when no season is named.
type Name struct {
	Title, EpisodeTitle string
	Year                int
	Date                string // YYYY-MM-DD
	Season              int
	Episode, EpisodeTo  int
	Seasons             []int // a pack folder's seasons
	Part, Parts         int
	Absolute            int
	Disc, Discs         int
	Track               int
	Group               string
	Edition             string
	Quality             []string
	Languages           []string
	Regions             []string
	Revision            string
	Flags               []string
	TitleID             string
	Role                Role
}

var (
	siteRe     = regexp.MustCompile(`(?i)^www\.\S+\s*-\s*`)
	copyRe     = regexp.MustCompile(`(?i)\s+copy(\s+\d+)?$`)
	dateRe     = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d\d)[.\- ](0[1-9]|1[0-2])[.\- ](0[1-9]|[12]\d|3[01])(?:[^0-9]|$)`)
	dateDMYRe  = regexp.MustCompile(`(?:^|[^0-9])(0[1-9]|[12]\d|3[01])[.\-](0[1-9]|1[0-2])[.\-]((?:19|20)\d\d)(?:[^0-9]|$)`)
	partRe     = regexp.MustCompile(`(?i)[\s.\-]*\bpart\s*0*(\d{1,2})\s*of\s*0*(\d{1,2})\b[\s,.\-]*`)
	packRe     = regexp.MustCompile(`(?i)\bs(\d{1,2})\s?e\d{1,3}\s?-\s?\d{1,3}\b`)
	partListRe = regexp.MustCompile(`(?i)\s+-\s+part\s+\d.*$`)
	parenRe    = regexp.MustCompile(`\(([^)]*)\)`)
	anyYearRe  = regexp.MustCompile(`(?:^|\D)((?:19|20)\d\d)(?:\D|$)`)
	seasonsRe  = regexp.MustCompile(`(?i)\bseasons?[ ._]?(\d{1,2})(?:\s*(?:and|&|-|to)\s*(\d{1,2}))?\b`)
	sTokRe     = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})(?:[^a-z0-9]|$)`)
	absRe      = regexp.MustCompile(`\s-\s(\d{1,4})(?:v\d)?(?:\s|$|\(|\[)`)
	groupRe    = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*`)
	groupTagRe = regexp.MustCompile(`[(\[]([^)\]]*)[)\]]`)
	trackRe    = regexp.MustCompile(`^(?:(\d)-)?(\d{1,3})(?:\s*[-._]+\s*|\s+)(\S.*)$`)
	leadYearRe = regexp.MustCompile(`^[(\[]?((?:19|20)\d\d)[)\]]?(?:\s*[-_]+\s*|\s+)(.+)$`)
	discsRe    = regexp.MustCompile(`(?i)\b(\d)\s*(?:cd|disc|discs|disk)s?\b`)
	discRe     = regexp.MustCompile(`(?i)\b(?:disc|disk|cd)\s*0*(\d{1,2})\b`)
	monthYear  = regexp.MustCompile(`(?i)^(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+((?:19|20)\d\d)$`)
	titleIDRe  = regexp.MustCompile(`^[0-9A-Fa-f]{16}$`)
	revRe      = regexp.MustCompile(`(?i)^(rev\s*[0-9a-z.]+|v\d+(?:\.\d+)*)$`)
	langsRe    = regexp.MustCompile(`^[A-Z][a-z](?:[,+][A-Z][a-z])+$|^(?:En|Ja|Fr|De|Es|It|Nl|Pt|Sv|No|Da|Fi|Zh|Ko|Ru|Pl)$`)
	chanRe     = regexp.MustCompile(`(\d)\.(\d)(?:\D|$)`)
)

// quality is the release vocabulary: a title ends at the first of these (docs/18 §8),
// and the analyser reports them in name order.
var quality = map[string]bool{
	"480p": true, "576p": true, "720p": true, "1080p": true, "1080i": true, "2160p": true, "4k": true, "uhd": true,
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true, "avc": true, "xvid": true, "divx": true, "av1": true,
	"bluray": true, "blu-ray": true, "brrip": true, "bdrip": true, "webrip": true, "web-dl": true, "webdl": true, "web": true,
	"hdtv": true, "dvdrip": true, "dvd": true, "remux": true, "hdts": true, "hdcam": true, "cam": true, "ts": true,
	"aac": true, "aac2.0": true, "aac5.1": true, "ac3": true, "dts": true, "dd5.1": true, "ddp5.1": true, "ddp5": true, "ddpa5.1": true,
	"eac3": true, "truehd": true, "atmos": true, "flac": true, "5.1": true, "7.1": true, "2.0": true,
	"10bit": true, "hdr": true, "hdr10": true, "dv": true, "sdr": true,
}

// junk ends a title like quality but is not reported: release flags and stores.
var junk = map[string]bool{
	"proper": true, "repack": true, "internal": true, "limited": true, "complete": true, "full": true, "multi": true,
	"amzn": true, "atvp": true, "nf": true, "dsnp": true, "hmax": true, "itunes": true, "ds4k": true, "esub": true, "esubs": true,
	"subs": true, "dual": true, "yify": true, "soundtrack": true, "ost": true, "320": true, "@320": true, "mp3": true,
}

// cuts reports whether a lower-cased token ends a title: a release, store or language
// token (docs/18 §8).
func cuts(t string) bool { return quality[t] || junk[t] || langs[t] != "" }

// langs are the language tokens of scene names.
var langs = map[string]string{
	"ita": "ITA", "eng": "ENG", "spa": "SPA", "esp": "SPA", "fre": "FRE", "fra": "FRE", "ger": "GER", "deu": "GER",
	"jpn": "JPN", "jap": "JPN", "kor": "KOR", "rus": "RUS", "hin": "HIN", "hindi": "HIN", "por": "POR", "pol": "POL",
}

// editions, longest first so "Extended Cut" wins over "Extended".
var editions = []string{
	"Remastered and Expanded", "Director's Cut", "Directors Cut", "Extended Cut", "Extended Edition", "Theatrical Cut",
	"Deluxe Edition", "Japanese Edition", "Special Edition", "Limited Edition", "Collector's Edition", "Anniversary Edition",
	"Super Deluxe", "Remastered", "Extended", "Unrated", "Theatrical", "Uncut",
}

// regions are No-Intro, GoodTools and Switch region words.
var regions = map[string]string{
	"usa": "USA", "us": "USA", "u": "USA", "europe": "Europe", "eu": "EU", "e": "Europe", "japan": "Japan", "jp": "Japan", "j": "Japan",
	"world": "World", "w": "World", "uk": "UK", "au": "AU", "australia": "Australia", "asia": "Asia", "korea": "Korea", "k": "Korea",
	"brazil": "Brazil", "canada": "Canada", "china": "China", "france": "France", "germany": "Germany", "italy": "Italy",
	"spain": "Spain", "sweden": "Sweden", "netherlands": "Netherlands", "ue": "USA, Europe", "ju": "Japan, USA",
}

// romFlags are the tags No-Intro and GoodTools give a dump, by their first word.
var romFlags = map[string]string{
	"proto": "proto", "prototype": "proto", "beta": "beta", "demo": "demo", "sample": "sample", "unl": "unl", "pirate": "pirate",
	"hack": "hack", "kiosk": "kiosk", "bonus": "bonus", "!": "verified", "h": "hack", "b": "bad", "dlc": "dlc", "upd": "update", "update": "update",
}

// categoryFlags are the flags a ROM's category folder gives it.
var categoryFlags = map[string]string{"hacks": "hack", "hack": "hack", "translations": "translation", "translation": "translation", "unreleased": "unreleased", "prototypes": "proto", "homebrew": "homebrew", "dlc": "dlc", "updates": "update"}

var (
	discRoleRe     = regexp.MustCompile(`(?i)^(?:(?:cd|disc|disk)\s*(\d{1,2}|[ivx]{1,4}|one|two|three|four|five|six)|d(\d{1,2}))(?:\b|$)`)
	categoryRoleRe = regexp.MustCompile(`(?i)^(?:\d{1,2}\s*-?\s*)?(studio albums?|albums|singles|eps|compilations?|live( albums)?|bootlegs|studio|remixes|soundtracks|hacks?|translations?|unreleased|prototypes|homebrew|games|dlc|updates)$`)
	formatWordRe   = regexp.MustCompile(`(?i)\b(mp3|flac|cbr|vbr|320|256|192|v0|v2|aac|alac|m4a|kbps|original|lossless|(?:19|20)\d\d)\b|[\[\]()\-_@]`)
	extrasRoleRe   = regexp.MustCompile(`(?i)^(subs|subtitles|sample|samples|extras|featurettes|bonus features|keys(?:[- ].*)?)$`)
)

var numberWords = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6}

// RoleOf is a folder's role by its name alone (docs/18 §8a).
func RoleOf(dir string) Role {
	d := strings.TrimSpace(dir)
	switch {
	case d == "":
		return RoleNone
	case extrasRoleRe.MatchString(d):
		return RoleExtras
	case discRoleRe.MatchString(d):
		return RoleDisc
	case categoryRoleRe.MatchString(d):
		return RoleCategory
	case regions[strings.ToLower(d)] != "" && len(d) > 1:
		return RoleRegion
	}
	if _, ok := SeasonOf(d); ok {
		return RoleSeason
	}
	if strings.TrimSpace(formatWordRe.ReplaceAllString(d, " ")) == "" && formatWordRe.MatchString(d) && hasFormatWord(d) {
		return RoleFormat
	}
	return RoleNone
}

func hasFormatWord(d string) bool {
	l := strings.ToLower(d)
	for _, w := range []string{"mp3", "flac", "cbr", "vbr", "aac", "alac", "lossless", "kbps"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// discNumber reads a disc folder's number: digits, a roman numeral or a word.
func discNumber(dir string) int {
	m := discRoleRe.FindStringSubmatch(strings.TrimSpace(dir))
	if m == nil {
		return 0
	}
	s := strings.ToLower(m[1] + m[2])
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if n, ok := numberWords[s]; ok {
		return n
	}
	return roman(s)
}

func roman(s string) int {
	vals := map[byte]int{'i': 1, 'v': 5, 'x': 10}
	n := 0
	for i := range len(s) {
		v := vals[s[i]]
		if i+1 < len(s) && vals[s[i+1]] > v {
			n -= v
		} else {
			n += v
		}
	}
	return n
}

// mediaExts are stripped before a name is read: a film's, a track's or a game's.
var romExts = map[string]bool{
	".zip": true, ".7z": true, ".iso": true, ".pbp": true, ".chd": true, ".cue": true, ".bin": true, ".m3u": true, ".cso": true,
	".z64": true, ".n64": true, ".v64": true, ".sfc": true, ".smc": true, ".nes": true, ".gb": true, ".gbc": true, ".gba": true,
	".nds": true, ".3ds": true, ".nsp": true, ".xci": true, ".nsz": true, ".xcz": true, ".rvz": true, ".gcz": true, ".ciso": true,
	".md": true, ".gen": true, ".sms": true, ".gg": true, ".pce": true, ".a26": true, ".lnx": true, ".wbfs": true,
}

func stripExt(name string, ctx Context) (string, bool) {
	e := ext(name)
	switch {
	case videoExt[e] && e != ".ts" || audioExt[e] && ctx != CtxRoms:
		return strings.TrimSuffix(name, path.Ext(name)), audioExt[e]
	case ctx == CtxRoms && romExts[e], e == ".nfo", e == ".ts" && ctx != CtxRoms:
		return strings.TrimSuffix(name, path.Ext(name)), false
	}
	return name, false
}

// Analyze reads a file's or a folder's name in a library of ctx; dirs are the folders
// above it inside the library, nearest first (docs/18 §8a, I-276). It reads nothing but
// its arguments, so every node gives the same answer.
func Analyze(name string, dirs []string, ctx Context) Name {
	n := Name{Season: -1, Role: RoleOf(name)}
	s, audio := stripExt(strings.TrimSpace(name), ctx)
	s = siteRe.ReplaceAllString(s, "")
	s = copyRe.ReplaceAllString(s, "")
	if n.Role == RoleDisc {
		n.Disc = discNumber(name)
	}
	if ctx == CtxRoms {
		analyzeROM(&n, s, dirs)
		return n
	}
	n.Edition = edition(s)
	if m := groupRe.FindStringSubmatch(s); m != nil {
		n.Group = strings.TrimSpace(m[1])
	}
	n.Date = date(s)
	n.Quality = qualityOf(s)
	n.Languages = languagesOf(s)
	if m := discsRe.FindStringSubmatch(s); m != nil {
		n.Discs, _ = strconv.Atoi(m[1])
	} else if m := discRe.FindStringSubmatch(s); m != nil && n.Role != RoleDisc {
		n.Disc, _ = strconv.Atoi(m[1])
	}
	if ctx == CtxMusic || audio {
		analyzeMusic(&n, s, dirs, audio)
		return n
	}
	analyzeVideo(&n, s, dirs, ctx)
	return n
}

// analyzeVideo reads a film's, a show's or an episode's name.
func analyzeVideo(n *Name, s string, dirs []string, ctx Context) {
	body := s
	if n.Group != "" {
		body = groupRe.ReplaceAllString(body, "")
	}
	// a part of a miniseries: "Title - Part 01 of 03, Episode Title (…)"
	if loc := partRe.FindStringSubmatchIndex(body); loc != nil {
		n.Part, _ = strconv.Atoi(body[loc[2]:loc[3]])
		n.Parts, _ = strconv.Atoi(body[loc[4]:loc[5]])
		n.EpisodeTitle = episodeTitle(body[loc[1]:])
		body = body[:loc[0]]
		if ctx == CtxTV {
			n.Season, n.Episode, n.EpisodeTo = 1, n.Part, n.Part
		}
	}
	body = partListRe.ReplaceAllString(body, "")
	body = parenRe.ReplaceAllStringFunc(body, func(g string) string { // "(2019)" stays for CleanName
		if yearTok.MatchString(strings.Trim(g, "()")) {
			return g
		}
		return " "
	})
	switch {
	case packRe.MatchString(body):
		m := packRe.FindStringSubmatch(body)
		k, _ := strconv.Atoi(m[1])
		n.Seasons = []int{k}
	default:
		if loc := episodeLoc(body); loc != nil {
			season, episode, to, _ := ParseEpisode(body[loc[0]:loc[1]])
			n.Season, n.Episode, n.EpisodeTo = season, episode, to
			n.EpisodeTitle = episodeTitle(body[loc[1]:])
			if loc[0] > 0 {
				body = body[:loc[0]]
			}
		} else if n.Date != "" {
			if i := dateIndex(body); i > 0 {
				n.EpisodeTitle = episodeTitle(dateRest(body, i))
				body = body[:i]
			}
		} else if n.Group != "" {
			if m := absRe.FindStringSubmatchIndex(body); m != nil {
				n.Absolute, _ = strconv.Atoi(body[m[2]:m[3]])
				body = body[:m[0]]
			}
		}
		n.Seasons = seasonsOf(body)
	}
	if n.Season < 0 && n.Episode > 0 {
		for _, d := range dirs {
			if k, ok := SeasonOf(d); ok {
				n.Season = k
				break
			}
		}
	}
	n.Title, n.Year = CleanName(body)
	if n.Year == 0 {
		n.Year = yearInGroups(s)
	}
	if n.Year == 0 {
		n.Year = laterYear(s, n.Title)
	}
	if n.Title == "" {
		n.Title = strings.TrimSpace(body)
	}
}

// episodeLoc is where the episode code is in s, or nil.
func episodeLoc(s string) []int {
	for _, re := range episodeRe {
		if loc := re.FindStringIndex(s); loc != nil {
			return loc
		}
	}
	return nil
}

// episodeTitle is the words after an episode code up to the first release, language or
// bracket token: " - Good News About Hell", ".Il.Circolo.Spezzato.ITA.ENG.1080p".
func episodeTitle(rest string) string {
	rest = strings.TrimLeft(rest, " .-_,")
	if i := strings.IndexAny(rest, "(["); i >= 0 {
		rest = rest[:i]
	}
	if dotted(rest) {
		rest = strings.NewReplacer(".", " ", "_", " ").Replace(rest)
	}
	var out []string
	for t := range strings.FieldsSeq(rest) {
		if isReleaseToken(t) {
			break
		}
		out = append(out, t)
	}
	return strings.Trim(strings.Join(out, " "), " -")
}

func isReleaseToken(t string) bool {
	l := strings.ToLower(t)
	if base, _, ok := strings.Cut(l, "-"); ok && (quality[base] || junk[base] || langs[base] != "") {
		return true
	}
	return quality[l] || junk[l] || langs[l] != ""
}

// dotted reports whether a name uses dots for spaces: more dots than spaces.
func dotted(s string) bool {
	return strings.Count(s, ".")+strings.Count(s, "_") > strings.Count(s, " ")
}

func date(s string) string {
	if m := dateRe.FindStringSubmatch(s); m != nil {
		return m[1] + "-" + m[2] + "-" + m[3]
	}
	if m := dateDMYRe.FindStringSubmatch(s); m != nil {
		return m[3] + "-" + m[2] + "-" + m[1]
	}
	return ""
}

func dateIndex(s string) int {
	if loc := dateRe.FindStringSubmatchIndex(s); loc != nil {
		return loc[2]
	}
	if loc := dateDMYRe.FindStringSubmatchIndex(s); loc != nil {
		return loc[2]
	}
	return -1
}

// dateRest is what follows the date that starts at i.
func dateRest(s string, i int) string {
	if len(s) >= i+10 {
		return s[i+10:]
	}
	return ""
}

// seasonsOf reads a pack folder's seasons: "Seasons 1 and 2", "Season.1", "S01".
func seasonsOf(s string) []int {
	if m := seasonsRe.FindStringSubmatch(s); m != nil {
		a, _ := strconv.Atoi(m[1])
		if m[2] == "" {
			return []int{a}
		}
		b, _ := strconv.Atoi(m[2])
		if b < a || b-a > 30 {
			return []int{a, b}
		}
		var out []int
		for k := a; k <= b; k++ {
			out = append(out, k)
		}
		return out
	}
	if m := sTokRe.FindStringSubmatch(s); m != nil && episodeLoc(s) == nil {
		k, _ := strconv.Atoi(m[1])
		return []int{k}
	}
	return nil
}

// undot reads a dotted name's dots and underscores as spaces, but an audio channel
// count's (5.1) as a dot.
func undot(s string) string {
	s = chanRe.ReplaceAllString(s, "$1\x00$2 ")
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	return strings.ReplaceAll(s, "\x00", ".")
}

// tokens splits a name into words: dots and underscores are spaces in a dotted name,
// except inside an audio channel count (5.1).
func tokens(s string) []string {
	if dotted(s) {
		s = undot(s)
	}
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '[' || r == ']' || r == '(' || r == ')' })
}

func qualityOf(s string) []string {
	var out []string
	for _, t := range tokens(s) {
		t = strings.TrimSuffix(t, ",")
		if base, _, ok := strings.Cut(t, "-"); ok && quality[strings.ToLower(base)] && !quality[strings.ToLower(t)] {
			t = base
		}
		if quality[strings.ToLower(t)] && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func languagesOf(s string) []string {
	var out []string
	for _, t := range tokens(s) {
		for p := range strings.SplitSeq(t, "-") {
			if l := langs[strings.ToLower(p)]; l != "" && !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	return out
}

func edition(s string) string {
	l := strings.ToLower(s)
	for _, e := range editions {
		if i := strings.Index(l, strings.ToLower(e)); i >= 0 {
			return s[i : i+len(e)]
		}
	}
	return ""
}

// laterYear is the first year in s after its title: a soundtrack's year behind its
// season (Westworld.Season.1.2016).
func laterYear(s, title string) int {
	rest := s
	if i := strings.Index(strings.NewReplacer(".", " ", "_", " ").Replace(s), title); i >= 0 {
		rest = s[min(len(s), i+len(title)):]
	}
	if m := anyYearRe.FindStringSubmatch(rest); m != nil {
		y, _ := strconv.Atoi(m[1])
		return y
	}
	return 0
}

// yearInGroups is a year named inside a bracket group: "(Complete 2015 TV Miniseries)",
// "(September 1987)".
func yearInGroups(s string) int {
	for _, m := range groupTagRe.FindAllStringSubmatch(s, -1) {
		for t := range strings.FieldsSeq(m[1]) {
			if yearTok.MatchString(t) {
				y, _ := strconv.Atoi(t)
				return y
			}
		}
	}
	return 0
}

// analyzeMusic reads an album folder's or a track's name. An album: a leading or bracketed
// year, the artist's name before a " - " dropped, bracket groups and bitrates dropped. A
// track: its number, a disc before it, the rest as the title.
func analyzeMusic(n *Name, s string, dirs []string, audio bool) {
	if audio {
		t := strings.ReplaceAll(s, "_", " ")
		artist := artistOf(dirs)
		if artist != "" { // "Duran Duran - 08 - UMF"
			if rest, ok := cutPrefixFold(t, artist); ok && strings.HasPrefix(strings.TrimSpace(rest), "-") {
				t = strings.TrimLeft(rest, " -")
			}
		}
		if m := trackRe.FindStringSubmatch(t); m != nil {
			n.Track, _ = strconv.Atoi(m[2])
			if m[1] != "" {
				n.Disc, _ = strconv.Atoi(m[1])
			}
			t = strings.TrimLeft(m[3], " -.")
		}
		if artist != "" { // "There Are Many Dangers - Crash Test Dummies"
			if l := len(t) - len(artist); l > 3 && strings.EqualFold(t[l:], artist) && strings.HasSuffix(strings.TrimSpace(t[:l]), "-") {
				t = strings.TrimRight(t[:l], " -")
			}
		}
		n.Title = strings.Join(strings.Fields(t), " ")
		return
	}
	t := strings.ReplaceAll(s, "_ ", ": ") // Windows writes ':' as '_'
	t = strings.ReplaceAll(t, "_", " ")
	if dotted(t) {
		if !leadYearRe.MatchString(t) {
			n.Title, n.Year = CleanName(t)
			if n.Year == 0 {
				n.Year = laterYear(t, n.Title)
			}
			return
		}
		t = undot(t)
	}
	// a year in brackets anywhere, or a month and year
	for _, m := range groupTagRe.FindAllStringSubmatch(t, -1) {
		g := strings.TrimSpace(m[1])
		if yearTok.MatchString(g) && n.Year == 0 {
			n.Year, _ = strconv.Atoi(g)
		} else if mm := monthYear.FindStringSubmatch(g); mm != nil && n.Year == 0 {
			n.Year, _ = strconv.Atoi(mm[1])
		}
	}
	t = strings.TrimSpace(t)
	// "Artist - 2000 - Album", "Artist - Album", "2016 - Artist - Album": the artist is
	// the folder above
	stripArtist := func() {
		for _, d := range dirs {
			if RoleOf(d) != RoleNone {
				continue
			}
			if rest, ok := cutPrefixFold(t, strings.TrimSpace(d)); ok && strings.HasPrefix(strings.TrimSpace(rest), "-") {
				rest = strings.TrimLeft(rest, " -")
				if m := leadYearRe.FindStringSubmatch(rest); m != nil {
					if n.Year == 0 {
						n.Year, _ = strconv.Atoi(m[1])
					}
					rest = m[2]
				}
				if strings.TrimSpace(rest) != "" {
					t = rest
				}
			}
			return
		}
	}
	stripArtist()
	if m := leadYearRe.FindStringSubmatch(t); len(m) == 3 && !startsWithYear(m[2]) {
		n.Year, _ = strconv.Atoi(m[1])
		t = m[2]
	} else if m := leadYearRe.FindStringSubmatch(groupTagRe.ReplaceAllString(t, "")); m != nil && strings.HasPrefix(t, "(") {
		t = m[2]
	}
	t = strings.TrimSpace(t)
	stripArtist()
	lead := ""
	if strings.HasPrefix(t, "(") { // "(What's The Story) Morning Glory": the group is the title's
		if i := strings.Index(t, ")"); i > 0 && !yearTok.MatchString(t[1:i]) {
			lead, t = t[:i+1], t[i+1:]
		}
	}
	t = lead + groupTagRe.ReplaceAllString(t, " ")
	t = albumSuffixRe.ReplaceAllString(t, "")
	var out []string
	for w := range strings.FieldsSeq(t) {
		lw := strings.ToLower(strings.Trim(w, ".,"))
		if strings.HasPrefix(w, "@") || lw == "320" || lw == "mp3" || lw == "flac" || discsRe.MatchString(lw) && !strings.Contains(lw, " ") && len(lw) <= 4 {
			continue
		}
		if yearTok.MatchString(strings.Trim(w, ".")) && len(out) > 0 { // a reissue's year after the title
			continue
		}
		out = append(out, w)
	}
	n.Title = strings.Trim(strings.Join(out, " "), " -.")
	if n.Title == "" {
		n.Title = strings.TrimSpace(s)
	}
}

// albumSuffixRe is what follows an album's title: " - EP", " - 2 Disc", " - Single".
var albumSuffixRe = regexp.MustCompile(`(?i)\s+-\s+(?:ep|single|lp|\d\s*(?:cd|disc|discs))\s*$`)

// artistOf is the nearest folder above that is a name, not a disc, category or format.
func artistOf(dirs []string) string {
	for _, d := range dirs {
		if RoleOf(d) == RoleNone {
			if len(dirs) > 1 && d == dirs[0] {
				continue // an album folder: its artist is above it
			}
			return strings.TrimSpace(d)
		}
	}
	return ""
}

// startsWithYear reports whether s's first word is a year: "2012 2009" is a title and a
// year, not a year and a title.
func startsWithYear(s string) bool {
	f := strings.Fields(s)
	return len(f) > 0 && yearTok.MatchString(f[0])
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// analyzeROM reads a game's name: the title before its tags, then No-Intro, GoodTools and
// Switch tags; region and category folders above it add regions and flags.
func analyzeROM(n *Name, s string, dirs []string) {
	addFlag := func(f string) {
		if f != "" && !slices.Contains(n.Flags, f) {
			n.Flags = append(n.Flags, f)
		}
	}
	addRegion := func(r string) {
		for p := range strings.SplitSeq(r, ", ") {
			if !slices.Contains(n.Regions, p) {
				n.Regions = append(n.Regions, p)
			}
		}
	}
	// leading tags: "[DLC] TLoZ …"
	for {
		m := groupRe.FindStringSubmatch(s)
		if m == nil {
			break
		}
		if f := romFlags[strings.ToLower(m[1])]; f != "" {
			addFlag(f)
		}
		s = s[len(m[0]):]
	}
	title := s
	if i := strings.IndexAny(s, "(["); i > 0 {
		title = s[:i]
	}
	for _, m := range groupTagRe.FindAllStringSubmatch(s[len(title):], -1) {
		romTag(n, strings.TrimSpace(m[1]), addFlag, addRegion)
	}
	title = strings.TrimSpace(title)
	if i := strings.LastIndex(title, ", "); i > 0 {
		switch a := title[i+2:]; a {
		case "The", "A", "An":
			title = a + " " + title[:i]
		}
	}
	n.Title = title
	for _, d := range dirs {
		switch RoleOf(d) { //nolint:exhaustive // only region and category folders say something of a game
		case RoleRegion:
			addRegion(regions[strings.ToLower(strings.TrimSpace(d))])
		case RoleCategory:
			addFlag(categoryFlags[strings.ToLower(strings.TrimSpace(d))])
		}
	}
}

// romTag reads one bracket group of a game's name.
func romTag(n *Name, g string, addFlag, addRegion func(string)) {
	lg := strings.ToLower(g)
	switch {
	case g == "":
	case titleIDRe.MatchString(g):
		n.TitleID = strings.ToUpper(g)
		switch strings.ToUpper(g[13:]) {
		case "000":
			addFlag("base")
		case "800":
			addFlag("update")
		default:
			addFlag("dlc")
		}
	case strings.HasPrefix(lg, "t+") || strings.HasPrefix(lg, "t-"):
		addFlag("translation")
	case revRe.MatchString(g):
		if n.Revision == "" {
			n.Revision = g
		}
	case langsRe.MatchString(g):
		n.Languages = strings.FieldsFunc(g, func(r rune) bool { return r == ',' || r == '+' })
	case date(g) != "" && len(g) == 10:
		n.Date = date(g)
	case discRe.MatchString(g) && strings.Contains(lg, "disc"):
		m := discRe.FindStringSubmatch(g)
		n.Disc, _ = strconv.Atoi(m[1])
	case strings.HasSuffix(lg, " dlc") || lg == "dlc":
		addFlag("dlc")
	default:
		if r, ok := regionList(g); ok {
			addRegion(r)
			return
		}
		first, _, _ := strings.Cut(lg, " ")
		addFlag(romFlags[first])
	}
}

// regionList reads "USA", "EU - AU", "USA, Europe", "U".
func regionList(g string) (string, bool) {
	parts := strings.FieldsFunc(g, func(r rune) bool { return r == ',' || r == '-' })
	var out []string
	for _, p := range parts {
		r := regions[strings.ToLower(strings.TrimSpace(p))]
		if r == "" {
			return "", false
		}
		out = append(out, r)
	}
	return strings.Join(out, ", "), len(out) > 0
}
