package search

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/meta"
)

// Catalogue entry kinds (docs/18 §8).
const (
	EntMovie   uint8 = 1
	EntShow    uint8 = 2
	EntSeason  uint8 = 3
	EntEpisode uint8 = 4
	EntArtist  uint8 = 5
	EntAlbum   uint8 = 6
	// docs/18 §8b, segment version 3
	EntPlatform uint8 = 7
	EntGame     uint8 = 8
)

// Entry is one media item of the catalogue (docs/18 §8, I-211). Art is a name in the
// item's folder (a loose file's: the library's), "" when there is none: the Media tab
// falls back to the browser-made thumbnail, or for an album to its folder's cover.
type Entry struct {
	Ino       meta.Ino
	Lib       meta.Ino // the library folder
	Parent    meta.Ino // the show of a season or an episode, the artist of an album
	Kind      uint8
	Video     meta.Ino // the file a film or an episode plays
	Title     string
	SortTitle string
	Year      int
	Plot      string
	Genres    []string
	Rating    string // one decimal, as the sidecar gave it
	Runtime   int    // minutes
	MPAA      string
	People    []string // the directors first, then up to six actors
	Directors int      // how many of People are directors; -1 unknown (segment version 3 or older)
	Season    int      // -1: unknown
	Episode   int
	EpisodeTo int // a double episode's last number, else Episode
	Aired     string
	Artist    string
	Tracks    int
	Poster    string
	Fanart    string
	Thumb     string
	Banner    string
	// the name analyser's fields (docs/18 §8a, I-276), segment version 2
	Date      string // an episode's air date, YYYY-MM-DD
	Absolute  int    // an anime episode's absolute number
	Edition   string
	Quality   []string
	Languages []string
	Discs     int
	// a game's (docs/18 §8b), segment version 3; a platform's game count is Tracks
	Platform  string // the platform's display name
	Regions   []string
	Revision  string
	Flags     []string
	Developer string
	Publisher string
	Players   string
}

// --- names

var (
	parenYear = regexp.MustCompile(`^(.*?)[\s.]*[(\[](\d{4})[)\]]`)
	brackets  = regexp.MustCompile(`\[[^\]]*\]`)
	yearTok   = regexp.MustCompile(`^(19|20)\d\d$`)
	episodeRe = []*regexp.Regexp{
		regexp.MustCompile(`(?i)s(\d{1,3})[ ._-]?e(\d{1,4})(?:[ ._-]?-?e(\d{1,4}))?`),
		regexp.MustCompile(`(?i)(?:^|[^0-9a-z])(\d{1,2})x(\d{2,3})(?:\D|$)`),
		regexp.MustCompile(`(?i)(?:^|[^a-z])episode[ ._-]?(\d{1,4})`),
	}
	seasonDir = regexp.MustCompile(`(?i)^(?:season|series)[ ._-]?(\d{1,3})$`)
	seasonTok = regexp.MustCompile(`^s\d{1,2}$`) // a season pack's S01 (lower-cased)
	seasonNum = regexp.MustCompile(`^\d{1,2}$`)
)

// CleanName is a file or folder name's title and year (docs/18 §8): `Title (2019)`, or
// a scene name cut at its first year, season or episode number, or release token.
func CleanName(name string) (title string, year int) {
	s := strings.TrimSpace(name)
	if m := parenYear.FindStringSubmatch(s); len(m) == 3 && strings.TrimSpace(m[1]) != "" {
		y, _ := strconv.Atoi(m[2])
		return tidy(brackets.ReplaceAllString(m[1], " "), !strings.Contains(m[1], " ")), y
	}
	isDotted := dotted(s)
	s = brackets.ReplaceAllString(s, " ")
	if isDotted {
		s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	} else {
		s = strings.ReplaceAll(s, "_", " ")
	}
	toks := strings.Fields(s)
	// the title ends at the first release token, season or episode number, or year
	// (after the first token, and not followed by another year: Blade Runner 2049 2017);
	// a year at or just before the cut is the year
	cut := len(toks)
	for i, t := range toks {
		lt := strings.ToLower(t)
		if base, _, ok := strings.Cut(lt, "-"); ok && cuts(base) {
			lt = base
		}
		if i == 0 {
			continue
		}
		next := ""
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		if cuts(lt) || episodeRe[0].MatchString(lt) || seasonTok.MatchString(lt) ||
			((lt == "season" || lt == "seasons") && seasonNum.MatchString(next)) ||
			(yearTok.MatchString(t) && !yearTok.MatchString(next)) {
			cut = i
			break
		}
	}
	switch {
	case cut < len(toks) && yearTok.MatchString(toks[cut]):
		year, _ = strconv.Atoi(toks[cut])
	case cut > 1 && yearTok.MatchString(toks[cut-1]):
		year, _ = strconv.Atoi(toks[cut-1])
		cut--
	}
	title = tidy(strings.Join(toks[:cut], " "), false)
	if title == "" {
		return strings.TrimSpace(name), 0
	}
	return title, year
}

func tidy(s string, dotted bool) string {
	if dotted {
		s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	}
	return strings.Trim(strings.Join(strings.Fields(s), " "), " -")
}

// SortTitle folds a title and drops a leading article.
func SortTitle(title string) string {
	t := Fold(title)
	for _, a := range []string{"the ", "a ", "an "} {
		if rest, ok := strings.CutPrefix(t, a); ok && rest != "" {
			return rest
		}
	}
	return t
}

// ParseEpisode reads SnnEnn (and SnnEnnEmm), nnxnn or Episode nn from a name; season is
// -1 when the name does not say (docs/18 §8).
func ParseEpisode(name string) (season, episode, to int, ok bool) {
	if m := episodeRe[0].FindStringSubmatch(name); m != nil {
		season, _ = strconv.Atoi(m[1])
		episode, _ = strconv.Atoi(m[2])
		to = episode
		if m[3] != "" {
			to, _ = strconv.Atoi(m[3])
		}
		return season, episode, max(to, episode), true
	}
	if m := episodeRe[1].FindStringSubmatch(name); m != nil {
		season, _ = strconv.Atoi(m[1])
		episode, _ = strconv.Atoi(m[2])
		return season, episode, episode, true
	}
	if m := episodeRe[2].FindStringSubmatch(name); m != nil {
		episode, _ = strconv.Atoi(m[1])
		return -1, episode, episode, true
	}
	return 0, 0, 0, false
}

// EpisodeTitle is an episode's title from its file name: the cleaned name, or
// "Episode N" when that is only the show's name again.
func EpisodeTitle(stem, show string, episode int) string {
	t, _ := CleanName(stem)
	if t == "" || SortTitle(t) == SortTitle(show) {
		return "Episode " + strconv.Itoa(episode)
	}
	return t
}

// SeasonOf is a season folder's number: Season NN, Series NN, Specials (0).
func SeasonOf(name string) (int, bool) {
	if strings.EqualFold(strings.TrimSpace(name), "specials") {
		return 0, true
	}
	if m := seasonDir.FindStringSubmatch(strings.TrimSpace(name)); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	return 0, false
}

// --- sidecars

// NFO is the Kodi fields docs/18 §8 reads.
type NFO struct {
	Root      string
	Title     string
	Original  string
	Year      int
	Plot      string
	Genres    []string
	Rating    string
	Runtime   int
	MPAA      string
	People    []string // the directors first
	Directors int      // how many of People are directors
	Season    int
	Episode   int
	Aired     string
	Status    string
	Biography string
}

type nfoXML struct {
	XMLName   xml.Name
	Title     string   `xml:"title"`
	Original  string   `xml:"originaltitle"`
	Year      string   `xml:"year"`
	Premiered string   `xml:"premiered"`
	Plot      string   `xml:"plot"`
	Outline   string   `xml:"outline"`
	Tagline   string   `xml:"tagline"`
	Runtime   string   `xml:"runtime"`
	Genre     []string `xml:"genre"`
	Rating    string   `xml:"rating"`
	Ratings   []struct {
		Default string `xml:"default,attr"`
		Value   string `xml:"value"`
	} `xml:"ratings>rating"`
	MPAA     string   `xml:"mpaa"`
	Director []string `xml:"director"`
	Actor    []struct {
		Name string `xml:"name"`
	} `xml:"actor"`
	Season    string `xml:"season"`
	Episode   string `xml:"episode"`
	Aired     string `xml:"aired"`
	Status    string `xml:"status"`
	Biography string `xml:"biography"`
}

var nfoRoots = map[string]bool{"movie": true, "tvshow": true, "episodedetails": true, "artist": true, "album": true}

// DecodeNFO parses a Kodi sidecar's first element; anything that is not one of the
// five Kodi roots, including a bare provider URL or truncated XML, is ok == false.
func DecodeNFO(b []byte) (NFO, bool) {
	var x nfoXML
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	if err := d.Decode(&x); err != nil || !nfoRoots[x.XMLName.Local] {
		return NFO{}, false
	}
	n := NFO{Root: x.XMLName.Local, Title: clean(x.Title), Original: clean(x.Original), MPAA: clean(x.MPAA), Aired: clean(x.Aired), Status: clean(x.Status), Biography: clean(x.Biography)}
	n.Plot = firstOf(clean(x.Plot), clean(x.Outline), clean(x.Tagline))
	n.Year = atoi(x.Year)
	if n.Year == 0 && len(strings.TrimSpace(x.Premiered)) >= 4 {
		n.Year = atoi(strings.TrimSpace(x.Premiered)[:4])
	}
	n.Runtime = atoi(x.Runtime)
	for _, g := range x.Genre {
		for part := range strings.SplitSeq(g, "/") {
			if g := clean(part); g != "" {
				n.Genres = append(n.Genres, g)
			}
		}
	}
	n.Rating = rating(x.Rating)
	for _, r := range x.Ratings {
		if n.Rating == "" || r.Default == "true" {
			if v := rating(r.Value); v != "" {
				n.Rating = v
			}
		}
	}
	for _, d := range x.Director {
		if d := clean(d); d != "" {
			n.People = append(n.People, d)
			n.Directors++
		}
	}
	actors := 0
	for _, a := range x.Actor {
		if a := clean(a.Name); a != "" && actors < 6 {
			n.People = append(n.People, a)
			actors++
		}
	}
	n.Season, n.Episode = -1, -1
	if s := strings.TrimSpace(x.Season); s != "" {
		n.Season = atoi(s)
	}
	if e := strings.TrimSpace(x.Episode); e != "" {
		n.Episode = atoi(e)
	}
	return n, true
}

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func rating(s string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 || f > 100 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}

func firstOf(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// --- media types

var (
	videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".webm": true, ".ts": true, ".wmv": true, ".mpg": true, ".mpeg": true}
	audioExt = map[string]bool{".mp3": true, ".m4a": true, ".flac": true, ".ogg": true, ".oga": true, ".opus": true, ".aac": true, ".wav": true}
	imageExt = []string{".jpg", ".jpeg", ".png", ".webp"}
	textExt  = map[string]bool{
		".md": true, ".markdown": true, ".txt": true, ".csv": true, ".tsv": true, ".json": true, ".yaml": true, ".yml": true,
		".toml": true, ".ini": true, ".conf": true, ".log": true, ".srt": true, ".vtt": true, ".html": true, ".htm": true,
		".xml": true, ".go": true, ".ts": true, ".js": true, ".py": true, ".sh": true, ".rs": true, ".c": true, ".h": true,
		".java": true, ".rb": true,
	}
)

func ext(name string) string { return strings.ToLower(path.Ext(name)) }

// IsVideo, IsAudio and IsText are the extensions docs/18 §2 and §8 name; `.ts` is text
// (TypeScript) to the index and video to the catalogue.
func IsVideo(name string) bool { return videoExt[ext(name)] }
func IsAudio(name string) bool { return audioExt[ext(name)] }
func IsText(name string) bool  { return textExt[ext(name)] }

func stem(name string) string { return strings.TrimSuffix(name, path.Ext(name)) }

// --- the catalogue

// catalogue builds the entries of every library (docs/18 §8): the folders from src,
// the sidecars through nfo, an album's tags through tags (the newest doc of a track).
type catalogue struct {
	src   Source
	nfo   func(meta.Inode) (NFO, bool)
	tags  func(meta.Ino) (Doc, bool)
	games func(meta.Inode) (map[string]GameInfo, bool)
	out   []Entry
	seen  map[meta.Ino]bool
	// note, when set, hears of each item the walk adds: the folder its sidecars live in,
	// the stem a file item's sidecars are named after, whether an .nfo was found and an
	// album's first track (the scrape's plan, docs/18 §11)
	note func(e Entry, dir meta.Ino, stem string, nfo bool, track meta.Ino)
}

// dirIndex is a folder's live children by name, and in name order.
type dirIndex struct {
	kids  []meta.Inode
	names map[string]meta.Inode
}

func (c *catalogue) list(dir meta.Ino) dirIndex {
	kids, _ := c.src.ReadDir(dir)
	d := dirIndex{names: map[string]meta.Inode{}}
	for i := range kids {
		if meta.IsEphemeral(kids[i].Name) || strings.HasPrefix(kids[i].Name, ".") {
			continue
		}
		d.kids = append(d.kids, kids[i])
		d.names[strings.ToLower(kids[i].Name)] = kids[i]
	}
	return d
}

// art is the first existing image among the stems, in order.
func (d dirIndex) art(stems ...string) string {
	for _, s := range stems {
		for _, e := range imageExt {
			if in, ok := d.names[strings.ToLower(s+e)]; ok && in.Type == meta.TFile {
				return in.Name
			}
		}
	}
	return ""
}

func (d dirIndex) file(names ...string) (meta.Inode, bool) {
	for _, n := range names {
		if in, ok := d.names[strings.ToLower(n)]; ok && in.Type == meta.TFile {
			return in, true
		}
	}
	return meta.Inode{}, false
}

func (c *catalogue) sidecar(d dirIndex, names ...string) (NFO, bool) {
	if in, ok := d.file(names...); ok {
		return c.nfo(in)
	}
	return NFO{}, false
}

// noted adds e and, when it was new, tells note about it.
func (c *catalogue) noted(e Entry, dir meta.Ino, stem string, nfo bool, track meta.Ino) {
	if c.add(e) && c.note != nil {
		c.note(c.out[len(c.out)-1], dir, stem, nfo, track)
	}
}

func (c *catalogue) add(e Entry) bool {
	if c.seen[e.Ino] { // a library inside a library is its own section (I-210)
		return false
	}
	c.seen[e.Ino] = true
	if e.SortTitle == "" {
		e.SortTitle = SortTitle(e.Title)
	}
	if e.EpisodeTo < e.Episode {
		e.EpisodeTo = e.Episode
	}
	c.out = append(c.out, e)
	return true
}

func applyNFO(e *Entry, n NFO) {
	if n.Title != "" {
		e.Title = n.Title
	}
	if n.Year != 0 {
		e.Year = n.Year
	}
	e.Plot = firstOf(n.Plot, n.Biography, e.Plot)
	if len(n.Genres) > 0 {
		e.Genres = n.Genres
	}
	e.Rating = firstOf(n.Rating, e.Rating)
	if n.Runtime != 0 {
		e.Runtime = n.Runtime
	}
	e.MPAA = firstOf(n.MPAA, e.MPAA)
	if len(n.People) > 0 {
		e.People, e.Directors = n.People, n.Directors
	}
	e.Aired = firstOf(n.Aired, e.Aired)
}

// walk derives every library's entries into c.out.
func (c *catalogue) walk() {
	c.walkLibs(c.src.Libraries(), nil, nil)
}

// walkLibs derives the entries of libs in order: a library rebuild names (every one when
// rebuild is nil) from its folders, every other one by keeping its entries in prev, as
// the pass does for the libraries no change lies under (docs/18 §5, I-387).
func (c *catalogue) walkLibs(libs []meta.Library, rebuild map[meta.Ino]bool, prev []Entry) {
	// a nested library is its own section: claim every library folder first
	for _, l := range libs {
		c.seen[l.Ino] = true
	}
	kept := map[meta.Ino][]int{}
	for i := range prev {
		kept[prev[i].Lib] = append(kept[prev[i].Lib], i)
	}
	for _, l := range libs {
		if rebuild != nil && !rebuild[l.Ino] {
			for _, i := range kept[l.Ino] {
				if !c.seen[prev[i].Ino] {
					c.seen[prev[i].Ino] = true
					c.out = append(c.out, prev[i])
				}
			}
			continue
		}
		switch l.Kind {
		case meta.LibraryMovies:
			c.movies(l.Ino)
		case meta.LibraryTV:
			c.shows(l.Ino)
		case meta.LibraryMusic:
			c.music(l.Ino)
		case meta.LibraryRoms:
			c.roms(l.Ino)
		case meta.LibraryNone:
		}
	}
}

func (c *catalogue) movies(lib meta.Ino) {
	d := c.list(lib)
	for i := range d.kids {
		k := &d.kids[i]
		switch {
		case k.Type == meta.TDir:
			if c.seen[k.Ino] {
				continue
			}
			md := c.list(k.Ino)
			var video meta.Inode
			for j := range md.kids {
				if f := &md.kids[j]; f.Type == meta.TFile && IsVideo(f.Name) {
					video = *f
					break
				}
			}
			if video.Ino == 0 {
				continue // not a film: no video in it
			}
			vs := stem(video.Name)
			e := Entry{Ino: k.Ino, Lib: lib, Kind: EntMovie, Video: video.Ino, Season: -1}
			filmName(&e, Analyze(k.Name, nil, CtxMovies), Analyze(video.Name, []string{k.Name}, CtxMovies))
			_, hasNFO := md.file("movie.nfo", vs+".nfo", k.Name+".nfo")
			if n, ok := c.sidecar(md, "movie.nfo", vs+".nfo", k.Name+".nfo"); ok {
				applyNFO(&e, n)
			}
			e.Poster = md.art("poster", vs+"-poster", "folder")
			e.Fanart = md.art("fanart", vs+"-fanart")
			e.Thumb = md.art(vs + "-thumb")
			c.noted(e, k.Ino, "", hasNFO, 0)
		case k.Type == meta.TFile && IsVideo(k.Name):
			vs := stem(k.Name)
			e := Entry{Ino: k.Ino, Lib: lib, Kind: EntMovie, Video: k.Ino, Season: -1}
			a := Analyze(k.Name, nil, CtxMovies)
			filmName(&e, a, a)
			_, hasNFO := d.file(vs + ".nfo")
			if n, ok := c.sidecar(d, vs+".nfo"); ok {
				applyNFO(&e, n)
			}
			e.Poster = d.art(vs + "-poster")
			e.Fanart = d.art(vs + "-fanart")
			e.Thumb = d.art(vs + "-thumb")
			c.noted(e, lib, vs, hasNFO, 0)
		}
	}
}

// filmName fills a film's name-derived fields from its folder's analysis, the video's
// filling what the folder's name lacks (docs/18 §8a).
func filmName(e *Entry, dir, video Name) {
	e.Title, e.Year = dir.Title, dir.Year
	if e.Year == 0 {
		e.Year = video.Year
	}
	e.Edition = firstOf(dir.Edition, video.Edition)
	e.Quality = video.Quality
	if len(e.Quality) == 0 {
		e.Quality = dir.Quality
	}
	e.Languages = video.Languages
}

func (c *catalogue) shows(lib meta.Ino) {
	shows := c.list(lib).kids
	for i := range shows {
		show := &shows[i]
		if show.Type != meta.TDir || c.seen[show.Ino] {
			continue
		}
		sd := c.list(show.Ino)
		e := Entry{Ino: show.Ino, Lib: lib, Kind: EntShow, Season: -1}
		sn := Analyze(show.Name, nil, CtxTV)
		e.Title, e.Year, e.Quality, e.Languages = sn.Title, sn.Year, sn.Quality, sn.Languages
		_, hasNFO := sd.file("tvshow.nfo")
		if n, ok := c.sidecar(sd, "tvshow.nfo"); ok {
			applyNFO(&e, n)
		}
		e.Poster = sd.art("poster", "folder")
		e.Fanart = sd.art("fanart")
		e.Banner = sd.art("banner")
		c.noted(e, show.Ino, "", hasNFO, 0)
		c.episodes(lib, show.Ino, show.Ino, sd, e.Title, -1, show.Name)
		for j := range sd.kids {
			k := &sd.kids[j]
			n, ok := SeasonOf(k.Name)
			if k.Type != meta.TDir || !ok {
				continue
			}
			nd := c.list(k.Ino)
			s := Entry{Ino: k.Ino, Lib: lib, Parent: show.Ino, Kind: EntSeason, Season: n, Title: k.Name}
			name := "season" + twoDigits(n)
			if n == 0 {
				name = "season-specials"
			}
			s.Poster = firstOf(sd.art(name+"-poster"), nd.art("poster", "folder"))
			c.noted(s, show.Ino, name, false, 0) // a season's poster is named in the show's folder
			c.episodes(lib, show.Ino, k.Ino, nd, e.Title, n, k.Name, show.Name)
		}
	}
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// episodes lists the episode files of one folder: the show's own (season from the
// name) or a season folder's (its number when the name does not say).
func (c *catalogue) episodes(lib, show, dir meta.Ino, d dirIndex, showTitle string, folderSeason int, dirs ...string) {
	for i := range d.kids {
		f := &d.kids[i]
		if f.Type != meta.TFile || !IsVideo(f.Name) {
			continue
		}
		vs := stem(f.Name)
		a := Analyze(f.Name, dirs, CtxTV)
		if a.Episode == 0 && a.Date == "" && a.Absolute == 0 {
			continue // not an episode: no number, part, air date or absolute number
		}
		season := a.Season
		if a.Episode > 0 && season < 0 {
			season = max(folderSeason, 1)
		}
		e := Entry{
			Ino: f.Ino, Lib: lib, Parent: show, Kind: EntEpisode, Video: f.Ino, Season: season, Episode: a.Episode, EpisodeTo: a.EpisodeTo,
			Date: a.Date, Absolute: a.Absolute, Quality: a.Quality, Languages: a.Languages,
		}
		switch {
		case a.EpisodeTitle != "":
			e.Title = a.EpisodeTitle
		case a.Episode > 0:
			e.Title = EpisodeTitle(vs, showTitle, a.Episode)
		case a.Date != "":
			e.Title = a.Date
		default:
			e.Title = "Episode " + strconv.Itoa(a.Absolute)
		}
		_, hasNFO := d.file(vs + ".nfo")
		if n, ok := c.sidecar(d, vs+".nfo"); ok {
			applyNFO(&e, n)
			if n.Season >= 0 && n.Episode > 0 {
				e.Season, e.Episode = n.Season, n.Episode
				e.EpisodeTo = max(e.EpisodeTo, e.Episode)
			}
		}
		e.Thumb = d.art(vs + "-thumb")
		c.noted(e, dir, vs, hasNFO, 0)
	}
}

func (c *catalogue) music(lib meta.Ino) {
	d := c.list(lib)
	for i := range d.kids {
		k := &d.kids[i]
		if k.Type != meta.TDir || c.seen[k.Ino] {
			continue
		}
		kd := c.list(k.Ino)
		if c.album(lib, lib, k, kd) {
			continue
		}
		a := Entry{Ino: k.Ino, Lib: lib, Kind: EntArtist, Title: k.Name, Season: -1}
		if n, ok := c.sidecar(kd, "artist.nfo"); ok {
			applyNFO(&a, n)
		}
		a.Poster = kd.art("artist", "folder")
		a.Fanart = kd.art("fanart")
		albums := c.albums(lib, k.Ino, kd, 2)
		if albums > 0 || a.Plot != "" {
			c.add(a)
		}
	}
}

// albums adds every album in an artist's folder d, walking category and format folders
// (`1 Studio`, `MP3 CBR Original`) at most depth deep, and reports how many it added.
func (c *catalogue) albums(lib, artist meta.Ino, d dirIndex, depth int) int {
	n := 0
	for j := range d.kids {
		al := &d.kids[j]
		if al.Type != meta.TDir || c.seen[al.Ino] {
			continue
		}
		ad := c.list(al.Ino)
		switch RoleOf(al.Name) { //nolint:exhaustive // a name, season or region folder is a candidate album
		case RoleExtras, RoleDisc:
			continue
		case RoleCategory, RoleFormat:
			if depth > 0 {
				n += c.albums(lib, artist, ad, depth-1)
			}
			continue
		}
		if c.album(lib, artist, al, ad) {
			n++
		}
	}
	return n
}

// tracksOf is a folder's audio files and how many other files it holds.
func tracksOf(d dirIndex) (tracks []meta.Inode, other int) {
	for i := range d.kids {
		f := &d.kids[i]
		switch {
		case f.Type != meta.TFile:
		case IsAudio(f.Name):
			tracks = append(tracks, *f)
		case meta.IsOSMetadata(f.Name):
		default:
			other++
		}
	}
	return tracks, other
}

// album adds dir as an album when its audio files outnumber its other files (13 §6), or
// when it holds no audio but disc or format folders that do (`CD1`, `Disc 2`, `MP3 CBR
// Original`): one album, a disc per disc folder.
func (c *catalogue) album(lib, parent meta.Ino, dir *meta.Inode, d dirIndex) bool {
	tracks, other := tracksOf(d)
	discs, folders := 0, 0
	if len(tracks) == 0 {
		for i := range d.kids {
			k := &d.kids[i]
			if r := RoleOf(k.Name); k.Type != meta.TDir || r != RoleDisc && r != RoleFormat {
				continue
			}
			if t, o := tracksOf(c.list(k.Ino)); len(t) > o {
				tracks = append(tracks, t...)
				if RoleOf(k.Name) == RoleDisc {
					discs++
				}
				folders++
			}
		}
	}
	if len(tracks) == 0 || folders == 0 && len(tracks) <= other {
		return false
	}
	var artist string
	if p, ok := c.src.GetInode(parent); ok {
		artist = p.Name
	}
	an := Analyze(dir.Name, []string{artist}, CtxMusic)
	e := Entry{Ino: dir.Ino, Lib: lib, Parent: parent, Kind: EntAlbum, Title: an.Title, Year: an.Year, Edition: an.Edition, Tracks: len(tracks), Discs: max(discs, an.Discs), Season: -1}
	if doc, ok := c.tags(tracks[0].Ino); ok {
		e.Title = firstOf(doc.Album, an.Title)
		e.Artist = doc.Artist
		if doc.Year != 0 {
			e.Year = doc.Year
		}
		if doc.Genre != "" {
			e.Genres = []string{doc.Genre}
		}
	}
	_, hasNFO := d.file("album.nfo")
	if n, ok := c.sidecar(d, "album.nfo"); ok {
		applyNFO(&e, n)
	}
	e.Poster = d.art("cover", "folder")
	c.noted(e, dir.Ino, "", hasNFO, tracks[0].Ino)
	return true
}

// --- encoding

func encodeCatalog(es []Entry) []byte {
	var w buf
	w.int(len(es))
	for i := range es {
		e := &es[i]
		w.uvarint(uint64(e.Ino))
		w.uvarint(uint64(e.Lib))
		w.uvarint(uint64(e.Parent))
		w.u8(e.Kind)
		w.uvarint(uint64(e.Video))
		for _, s := range []string{e.Title, e.SortTitle, e.Plot, e.Rating, e.MPAA, e.Aired, e.Artist, e.Poster, e.Fanart, e.Thumb, e.Banner} {
			w.str(s)
		}
		for _, n := range []int{e.Year, e.Runtime, e.Season + 1, e.Episode, e.EpisodeTo, e.Tracks} {
			w.int(n)
		}
		for _, l := range [][]string{e.Genres, e.People} {
			w.int(len(l))
			for _, s := range l {
				w.str(s)
			}
		}
		w.str(e.Date)
		w.str(e.Edition)
		w.int(e.Absolute)
		w.int(e.Discs)
		for _, l := range [][]string{e.Quality, e.Languages} {
			w.int(len(l))
			for _, s := range l {
				w.str(s)
			}
		}
		for _, s := range []string{e.Platform, e.Revision, e.Developer, e.Publisher, e.Players} {
			w.str(s)
		}
		for _, l := range [][]string{e.Regions, e.Flags} {
			w.int(len(l))
			for _, s := range l {
				w.str(s)
			}
		}
		w.int(e.Directors)
	}
	return w.b
}

func readList(c *cursor, p *[]string) {
	m := c.int()
	for j := 0; j < m && c.err == nil; j++ {
		*p = append(*p, c.str())
	}
}

// decodeCatalog reads a catalogue section of segment version v: 1 lacks the name
// analyser's fields.
func decodeCatalog(b []byte, v uint8) []Entry {
	c := &cursor{b: b}
	n := c.int()
	if n > len(b) {
		return nil
	}
	out := make([]Entry, 0, n)
	for i := 0; i < n && c.err == nil; i++ {
		e := Entry{Ino: meta.Ino(c.uvarint()), Lib: meta.Ino(c.uvarint()), Parent: meta.Ino(c.uvarint()), Kind: c.u8(), Video: meta.Ino(c.uvarint())}
		for _, p := range []*string{&e.Title, &e.SortTitle, &e.Plot, &e.Rating, &e.MPAA, &e.Aired, &e.Artist, &e.Poster, &e.Fanart, &e.Thumb, &e.Banner} {
			*p = c.str()
		}
		for _, p := range []*int{&e.Year, &e.Runtime, &e.Season, &e.Episode, &e.EpisodeTo, &e.Tracks} {
			*p = c.int()
		}
		e.Season--
		lists := []*[]string{&e.Genres, &e.People}
		if v >= 2 {
			for _, p := range lists {
				readList(c, p)
			}
			e.Date, e.Edition = c.str(), c.str()
			e.Absolute, e.Discs = c.int(), c.int()
			lists = []*[]string{&e.Quality, &e.Languages}
		}
		for _, p := range lists {
			readList(c, p)
		}
		if v >= 3 {
			for _, p := range []*string{&e.Platform, &e.Revision, &e.Developer, &e.Publisher, &e.Players} {
				*p = c.str()
			}
			readList(c, &e.Regions)
			readList(c, &e.Flags)
		}
		e.Directors = -1
		if v >= 4 {
			e.Directors = c.int()
		}
		out = append(out, e)
	}
	if c.err != nil {
		return nil
	}
	return out
}

// --- games (docs/18 §8b)

// roms lists a roms library: each child folder a platform, each game below it, walking
// region, category and format folders.
func (c *catalogue) roms(lib meta.Ino) {
	d := c.list(lib)
	for i := range d.kids {
		k := &d.kids[i]
		if k.Type != meta.TDir || c.seen[k.Ino] || RoleOf(k.Name) == RoleExtras {
			continue
		}
		p := platformOf(k.Name)
		pd := c.list(k.Ino)
		var games map[string]GameInfo
		if gl, ok := pd.file("gamelist.xml"); ok && c.games != nil {
			games, _ = c.games(gl)
		}
		g := &gameWalk{c: c, lib: lib, platform: k.Ino, plat: p, games: games}
		g.walk(pd, k.Ino, "", nil, 3)
		g.flush()
		e := Entry{Ino: k.Ino, Lib: lib, Kind: EntPlatform, Title: p.title, Platform: p.title, Tracks: g.count, Season: -1}
		e.Poster = cmp.Or(pd.art("platform", "poster", "folder", "logo"), g.cover)
		c.add(e)
	}
}

// gameWalk gathers one platform's games: candidates first, then one entry per title and
// tag set, the discs of one title merged.
type gameWalk struct {
	c        *catalogue
	lib      meta.Ino
	platform meta.Ino
	plat     platform
	games    map[string]GameInfo
	cands    []gameCand
	count    int
	// cover is the box art of the first game by sort title that has one, below the
	// platform folder: the platform's poster when its folder has none (I-400)
	cover, coverKey string
}

type gameCand struct {
	in   meta.Inode // the game's file, or its folder for a disc set
	file meta.Inode // the file a disc set's folder plays: its cue, m3u or gdi
	rel  string     // its path below the platform folder
	dir  dirIndex   // its folder's
	at   meta.Ino   // the folder its art goes in: its own for a disc set, else its parent
	name Name
}

// walk visits a folder below the platform: rel is its path below the platform folder,
// dirs the folders above it nearest first.
func (g *gameWalk) walk(d dirIndex, at meta.Ino, rel string, dirs []string, depth int) {
	for i := range d.kids {
		k := &d.kids[i]
		r := path.Join(rel, k.Name)
		if k.Type == meta.TDir {
			if g.c.seen[k.Ino] || RoleOf(k.Name) == RoleExtras || depth == 0 {
				continue
			}
			kd := g.c.list(k.Ino)
			if set, ok := discSet(kd); ok { // a folder of one game's disc images
				g.cands = append(g.cands, gameCand{in: *k, file: set, rel: r, dir: kd, at: k.Ino, name: Analyze(set.Name, append([]string{k.Name}, dirs...), CtxRoms)})
				continue
			}
			g.walk(kd, k.Ino, r, append([]string{k.Name}, dirs...), depth-1)
			continue
		}
		if k.Type != meta.TFile || !g.plat.takes(k.Name) || ext(k.Name) == ".bin" && hasCue(d) {
			continue
		}
		n := Analyze(k.Name, dirs, CtxRoms)
		if slices.Contains(n.Flags, "update") || slices.Contains(n.Flags, "dlc") {
			continue // part of a game, not one (docs/18 §8b)
		}
		g.cands = append(g.cands, gameCand{in: *k, file: *k, rel: r, dir: d, at: at, name: n})
	}
}

// discSet reports a folder holding a cue, m3u or gdi: one game, played from that file.
func discSet(d dirIndex) (meta.Inode, bool) {
	for i := range d.kids {
		if k := &d.kids[i]; k.Type == meta.TFile && setExts[ext(k.Name)] {
			return *k, true
		}
	}
	return meta.Inode{}, false
}

func hasCue(d dirIndex) bool {
	for i := range d.kids {
		if k := &d.kids[i]; k.Type == meta.TFile && (ext(k.Name) == ".cue" || ext(k.Name) == ".m3u") {
			return true
		}
	}
	return false
}

// discKey is what two discs of one game share: everything the name says but the disc.
func discKey(rel string, n Name) string {
	return path.Dir(rel) + "\x00" + Fold(n.Title) + "\x00" + strings.Join(n.Regions, ",") + "\x00" + n.Revision + "\x00" + strings.Join(n.Flags, ",")
}

// flush adds one entry per candidate, the discs of one title as one game whose entry is
// its first disc's file.
func (g *gameWalk) flush() {
	byKey := map[string]int{}
	var games []gameCand
	discs := map[int]int{}
	for ci := range g.cands {
		cd := &g.cands[ci]
		if cd.name.Disc > 0 {
			if i, ok := byKey[discKey(cd.rel, cd.name)]; ok {
				discs[i]++
				continue
			}
			byKey[discKey(cd.rel, cd.name)] = len(games)
			discs[len(games)] = 1
		}
		games = append(games, *cd)
	}
	for i := range games {
		cd := &games[i]
		n := cd.name
		e := Entry{
			Ino: cd.in.Ino, Lib: g.lib, Parent: g.platform, Kind: EntGame, Video: cd.file.Ino, Title: n.Title, Season: -1,
			Platform: g.plat.title, Regions: n.Regions, Languages: n.Languages, Revision: n.Revision, Flags: n.Flags, Discs: discs[i],
		}
		if n.Date != "" {
			e.Year = atoi(n.Date[:4])
		}
		dir := path.Dir(cd.rel) // the game's folder below the platform's
		if cd.in.Type == meta.TDir {
			dir = cd.rel
		}
		if gi, ok := g.games[cd.rel]; ok {
			e.Title = firstOf(gi.Name, e.Title)
			e.Plot, e.Developer, e.Publisher, e.Players, e.Rating = gi.Desc, gi.Developer, gi.Publisher, gi.Players, gi.Rating
			if gi.Year != 0 {
				e.Year = gi.Year
			}
			if gi.Genre != "" {
				e.Genres = []string{gi.Genre}
			}
			e.Poster = g.inPlatform(dir, firstOf(gi.Image, gi.Thumbnail))
			e.Banner = g.inPlatform(dir, gi.Marquee)
		}
		if e.Poster == "" {
			e.Poster = g.boxArt(*cd)
		}
		if k := SortTitle(e.Title); e.Poster != "" && (g.cover == "" || k < g.coverKey) {
			g.cover, g.coverKey = path.Join(dir, e.Poster), k
		}
		key := cd.in.Name // a disc set: its folder's name; a file: its stem
		if cd.in.Type != meta.TDir {
			key = stem(cd.in.Name)
		}
		g.c.noted(e, cd.at, key, false, 0)
		g.count++
	}
}

// inPlatform makes a path relative to the platform folder relative to the game's folder
// dir (also relative to the platform's), when the file exists.
func (g *gameWalk) inPlatform(dir, p string) string {
	if p == "" {
		return ""
	}
	parts := append([]string{}, strings.Split(path.Join(g.platformPath(), p), "/")...)
	if in, err := g.c.src.Resolve(parts); err != nil || in.Type != meta.TFile {
		return ""
	}
	rel := p
	if dir != "." && dir != "" {
		rel = strings.Repeat("../", strings.Count(dir, "/")+1) + p
	}
	return rel
}

func (g *gameWalk) platformPath() string {
	parts, err := g.c.src.PathOf(g.platform)
	if err != nil {
		return ""
	}
	return path.Join(parts...)
}

// boxArt is a game's art by name: <stem>.png beside it, images/<stem>* or
// media/boxart/<stem>.* beside it, else boxart.* or cover.* in a game's folder.
func (g *gameWalk) boxArt(cd gameCand) string {
	if cd.in.Type == meta.TDir {
		return cd.dir.art("boxart", "cover", "folder")
	}
	s := stem(cd.in.Name)
	if a := cd.dir.art(s); a != "" {
		return a
	}
	for _, sub := range []string{"images", "media/boxart", "boxart"} {
		parts := strings.Split(sub, "/")
		cur, ok := meta.Inode{}, true
		dirs := cd.dir
		for _, p := range parts {
			cur, ok = dirs.names[strings.ToLower(p)]
			if !ok || cur.Type != meta.TDir {
				ok = false
				break
			}
			dirs = g.c.list(cur.Ino)
		}
		if !ok {
			continue
		}
		if a := dirs.art(s, s+"-image", s+"-boxart"); a != "" {
			return sub + "/" + a
		}
	}
	return ""
}

// RomEntries is a roms library's platforms and games from the tree and the names alone,
// reading no file: what the Media tab lists live before the next pass fills in a
// gamelist's fields (docs/13 §10, I-279).
func RomEntries(src Source, lib meta.Ino) []Entry {
	c := &catalogue{src: src, seen: map[meta.Ino]bool{lib: true}}
	c.roms(lib)
	return c.out
}

// SameGame reports whether two names are discs of one game: the same title and tags but
// the disc (docs/18 §8b).
func SameGame(a, b Name) bool {
	return a.Disc > 0 && b.Disc > 0 && discKey("", a) == discKey("", b)
}
