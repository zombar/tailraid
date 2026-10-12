package search

import (
	"bytes"
	"context"
	"encoding/xml"
	"path"
	"regexp"
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
	People    []string // the director first, then up to six actors
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

// junk is the scene-name vocabulary docs/18 §8 strips: a title ends at the first of
// these, at a year or at a season or episode number.
var junk = map[string]bool{
	"1080p": true, "720p": true, "2160p": true, "480p": true, "4k": true, "x264": true, "x265": true, "h264": true, "h265": true,
	"hevc": true, "bluray": true, "webrip": true, "web-dl": true, "webdl": true, "hdtv": true, "dvdrip": true, "remux": true,
	"aac": true, "dts": true, "atmos": true, "proper": true, "repack": true, "10bit": true, "hdr": true, "brrip": true, "web": true,
}

// CleanName is a file or folder name's title and year (docs/18 §8): `Title (2019)`, or
// a scene name cut at its first year, season or episode number, or release token.
func CleanName(name string) (title string, year int) {
	s := strings.TrimSpace(name)
	if m := parenYear.FindStringSubmatch(s); len(m) == 3 && strings.TrimSpace(m[1]) != "" {
		y, _ := strconv.Atoi(m[2])
		return tidy(brackets.ReplaceAllString(m[1], " "), !strings.Contains(m[1], " ")), y
	}
	s = brackets.ReplaceAllString(s, " ")
	dotted := !strings.Contains(strings.TrimSpace(s), " ")
	if dotted {
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
		if base, _, ok := strings.Cut(lt, "-"); ok && junk[base] {
			lt = base
		}
		if i == 0 {
			continue
		}
		next := ""
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		if junk[lt] || episodeRe[0].MatchString(lt) || seasonTok.MatchString(lt) ||
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
	People    []string
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
	src  Source
	nfo  func(meta.Inode) (NFO, bool)
	tags func(meta.Ino) (Doc, bool)
	out  []Entry
	seen map[meta.Ino]bool
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

func (c *catalogue) add(e Entry) {
	if c.seen[e.Ino] { // a library inside a library is its own section (I-210)
		return
	}
	c.seen[e.Ino] = true
	if e.SortTitle == "" {
		e.SortTitle = SortTitle(e.Title)
	}
	if e.EpisodeTo < e.Episode {
		e.EpisodeTo = e.Episode
	}
	c.out = append(c.out, e)
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
		e.People = n.People
	}
	e.Aired = firstOf(n.Aired, e.Aired)
}

// buildCatalog derives every library's entries, the libraries in kind-then-path order
// and each folder's children in name order, so every node derives the same list.
func buildCatalog(_ context.Context, src Source, nfo func(meta.Inode) (NFO, bool), tags func(meta.Ino) (Doc, bool)) []Entry {
	c := &catalogue{src: src, nfo: nfo, tags: tags, seen: map[meta.Ino]bool{}}
	libs := src.Libraries()
	// a nested library is its own section: claim every library folder first
	for _, l := range libs {
		c.seen[l.Ino] = true
	}
	for _, l := range libs {
		switch l.Kind {
		case meta.LibraryMovies:
			c.movies(l.Ino)
		case meta.LibraryTV:
			c.shows(l.Ino)
		case meta.LibraryMusic:
			c.music(l.Ino)
		case meta.LibraryNone:
		}
	}
	return c.out
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
			e.Title, e.Year = CleanName(k.Name)
			if n, ok := c.sidecar(md, "movie.nfo", vs+".nfo", k.Name+".nfo"); ok {
				applyNFO(&e, n)
			}
			e.Poster = md.art("poster", vs+"-poster", "folder")
			e.Fanart = md.art("fanart", vs+"-fanart")
			e.Thumb = md.art(vs + "-thumb")
			c.add(e)
		case k.Type == meta.TFile && IsVideo(k.Name):
			vs := stem(k.Name)
			e := Entry{Ino: k.Ino, Lib: lib, Kind: EntMovie, Video: k.Ino, Season: -1}
			e.Title, e.Year = CleanName(vs)
			if n, ok := c.sidecar(d, vs+".nfo"); ok {
				applyNFO(&e, n)
			}
			e.Poster = d.art(vs + "-poster")
			e.Fanart = d.art(vs + "-fanart")
			e.Thumb = d.art(vs + "-thumb")
			c.add(e)
		}
	}
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
		e.Title, e.Year = CleanName(show.Name)
		if n, ok := c.sidecar(sd, "tvshow.nfo"); ok {
			applyNFO(&e, n)
		}
		e.Poster = sd.art("poster", "folder")
		e.Fanart = sd.art("fanart")
		e.Banner = sd.art("banner")
		c.add(e)
		c.episodes(lib, show.Ino, sd, e.Title, -1)
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
			c.add(s)
			c.episodes(lib, show.Ino, nd, e.Title, n)
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
func (c *catalogue) episodes(lib, show meta.Ino, d dirIndex, showTitle string, folderSeason int) {
	for i := range d.kids {
		f := &d.kids[i]
		if f.Type != meta.TFile || !IsVideo(f.Name) {
			continue
		}
		vs := stem(f.Name)
		season, ep, to, ok := ParseEpisode(vs)
		if !ok {
			continue
		}
		if season < 0 {
			season = max(folderSeason, 1)
		}
		e := Entry{Ino: f.Ino, Lib: lib, Parent: show, Kind: EntEpisode, Video: f.Ino, Season: season, Episode: ep, EpisodeTo: to}
		e.Title = EpisodeTitle(vs, showTitle, ep)
		if n, ok := c.sidecar(d, vs+".nfo"); ok {
			applyNFO(&e, n)
			if n.Season >= 0 && n.Episode > 0 {
				e.Season, e.Episode = n.Season, n.Episode
				e.EpisodeTo = max(e.EpisodeTo, e.Episode)
			}
		}
		e.Thumb = d.art(vs + "-thumb")
		c.add(e)
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
		albums := 0
		for j := range kd.kids {
			if al := &kd.kids[j]; al.Type == meta.TDir && !c.seen[al.Ino] && c.album(lib, k.Ino, al, c.list(al.Ino)) {
				albums++
			}
		}
		if albums > 0 || a.Plot != "" {
			c.add(a)
		}
	}
}

// album adds dir as an album when its audio files outnumber its other files (13 §6).
func (c *catalogue) album(lib, parent meta.Ino, dir *meta.Inode, d dirIndex) bool {
	var tracks []meta.Inode
	other := 0
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
	if len(tracks) == 0 || len(tracks) <= other {
		return false
	}
	e := Entry{Ino: dir.Ino, Lib: lib, Parent: parent, Kind: EntAlbum, Title: dir.Name, Tracks: len(tracks), Season: -1}
	if doc, ok := c.tags(tracks[0].Ino); ok {
		e.Title = firstOf(doc.Album, dir.Name)
		e.Artist = doc.Artist
		e.Year = doc.Year
		if doc.Genre != "" {
			e.Genres = []string{doc.Genre}
		}
	}
	if n, ok := c.sidecar(d, "album.nfo"); ok {
		applyNFO(&e, n)
	}
	e.Poster = d.art("cover", "folder")
	c.add(e)
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
	}
	return w.b
}

func decodeCatalog(b []byte) []Entry {
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
		for _, p := range []*[]string{&e.Genres, &e.People} {
			m := c.int()
			for j := 0; j < m && c.err == nil; j++ {
				*p = append(*p, c.str())
			}
		}
		out = append(out, e)
	}
	if c.err != nil {
		return nil
	}
	return out
}
