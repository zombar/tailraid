package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/dhowden/tag"

	"github.com/zombar/tailraid/internal/mediatag"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/search"
)

// Tag reads are bounded (docs/13 §6, I-164): internal/mediatag reads the first TagHead
// bytes and the last TagTail, nothing between.
const (
	TagHead = mediatag.Head
	TagTail = mediatag.Tail
)

// Tags is /api/tags (docs/13 §3).
type Tags = mediatag.Tags

// readTags parses an audio file's tags; a file without any, or with tags the parser
// rejects, is empty tags and no error.
func readTags(f io.ReadSeeker, size int64) (Tags, *tag.Picture) { return mediatag.Read(f, size) }

// open opens a file for reading, live, inside a deleted root or as of a snapshot.
func (s *Server) open(t target) (io.ReadSeekCloser, error) {
	if t.snap != nil {
		return t.fs.Open(t.vpath)
	}
	if t.trashed {
		return t.fs.OpenTrash(t.root.Ino, t.parts)
	}
	return t.fs.Open(t.canonical)
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	t, err := s.resolveTarget(r, r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if t.in.Type != meta.TFile || !isAudio(t.in.Name) {
		writeErr(w, http.StatusBadRequest, "tags "+t.canonical+": not an audio file")
		return
	}
	f, err := s.open(t)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	tags, _ := readTags(f, int64(t.in.Size))
	writeJSON(w, http.StatusOK, tags)
}

// coverCache remembers each folder's cover, so a listing costs no tag parse twice
// (docs/13 §5).
type coverCache struct {
	mu      sync.Mutex
	entries map[meta.Ino]coverEntry
}

// coverEntry holds while every inode it read is unchanged and, for a "none", while none
// of the videos it waits for has a frame (I-275): the folder itself first, then each
// subfolder it looked into and each track whose tags it read. The FSM bumps a folder's
// Version whenever its name table changes, and a file's when its bytes do (docs/01).
type coverEntry struct {
	deps []coverDep
	wait []meta.Ino // a "none"'s frameless videos, the one to grab first
	name string
}

type coverDep struct {
	ino     meta.Ino
	version uint64
	mtime   int64
}

// coverRecord is a folder's resolved cover as stored in the volume beside the thumbnails
// (docs/13 §5, I-256): the choice, "" for none, every inode it read and the videos a none
// waits for, so any node serves it while they are unchanged and resolves it again once
// one changed or one of those videos got its frame.
type coverRecord struct {
	V    int              `json:"v,omitempty"`
	Name string           `json:"name"`
	Deps []coverRecordDep `json:"deps"`
	Wait []meta.Ino       `json:"wait,omitempty"`
}

// coverRecordVersion 2 lists a none's waits (I-275); a none of an older record cannot
// know a frame arrived and is resolved again.
const coverRecordVersion = 2

type coverRecordDep struct {
	Ino     meta.Ino `json:"ino"`
	Version uint64   `json:"version"`
	Mtime   int64    `json:"mtime"`
}

// coverKey is the name of a folder's cover record in ThumbDir.
func coverKey(dir meta.Inode) string { return fmt.Sprintf("%d-%d.cover", dir.Ino, dir.Gen) }

// parseCoverKey reads a cover record's name back; ok is false for any other name.
func parseCoverKey(name string) (ino meta.Ino, gen uint32, ok bool) {
	base, found := strings.CutSuffix(name, ".cover")
	if !found {
		return 0, 0, false
	}
	var i uint64
	var g uint32
	if n, err := fmt.Sscanf(base, "%d-%d", &i, &g); err != nil || n != 2 || fmt.Sprintf("%d-%d", i, g) != base {
		return 0, 0, false
	}
	return meta.Ino(i), g, true
}

// current reports whether every inode deps names is as it was when it was read.
func (s *Server) current(deps []coverDep) bool {
	for _, d := range deps {
		if in, ok := s.o.Store.GetInodeAny(d.ino); !ok || depOf(in) != d {
			return false
		}
	}
	return true
}

// valid reports whether e still holds for dir (I-256, I-275).
func (s *Server) valid(dir meta.Inode, e coverEntry) bool {
	if len(e.deps) == 0 || e.deps[0] != depOf(dir) || !s.current(e.deps[1:]) {
		return false
	}
	for _, ino := range e.wait {
		if in, ok := s.o.Store.GetInodeAny(ino); !ok || s.hasThumb(in) {
			return false
		}
	}
	return true
}

// storedCover is the folder's cover from its record in the volume, when the record exists
// and still holds: one small read where resolving reads the tags of the folder's tracks
// over the network.
func (s *Server) storedCover(dir meta.Inode) (coverEntry, bool) {
	f, err := s.o.FS.Open(path.Join(ThumbDir, coverKey(dir))) // the node's own tree (docs/21 §3)
	if err != nil {
		return coverEntry{}, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return coverEntry{}, false
	}
	var rec coverRecord
	if json.Unmarshal(b, &rec) != nil || len(rec.Deps) == 0 || (rec.Name == "" && rec.V < coverRecordVersion) {
		return coverEntry{}, false
	}
	e := coverEntry{name: rec.Name, wait: rec.Wait}
	for _, d := range rec.Deps {
		e.deps = append(e.deps, coverDep{ino: d.Ino, version: d.Version, mtime: d.Mtime})
	}
	if !s.valid(dir, e) {
		return coverEntry{}, false
	}
	return e, true
}

// storeCover writes the folder's resolved cover to the volume for every node; a failure
// only costs another node the resolve.
func (s *Server) storeCover(dir meta.Inode, e coverEntry) {
	rec := coverRecord{V: coverRecordVersion, Name: e.name, Wait: e.wait}
	for _, d := range e.deps {
		rec.Deps = append(rec.Deps, coverRecordDep{Ino: d.ino, Version: d.version, Mtime: d.mtime})
	}
	b, err := json.Marshal(rec)
	if err == nil {
		err = s.storeThumb(coverKey(dir), b)
	}
	if err != nil {
		s.o.Logger.Warn("folder cover not stored", "dir", dir.Ino, "err", err)
	}
}

// remember keeps a folder's cover in this node's memory for listings.
func (s *Server) remember(dir meta.Inode, e coverEntry) {
	s.covers.mu.Lock()
	if s.covers.entries == nil || len(s.covers.entries) >= coverCacheMax {
		s.covers.entries = map[meta.Ino]coverEntry{}
	}
	s.covers.entries[dir.Ino] = e
	s.covers.mu.Unlock()
}

const coverCacheMax = 1024

// CoverSubdirs is how many subfolders a folder without a cover of its own looks into on
// each level (docs/13 §5, I-165).
const CoverSubdirs = 16

// coverWaitMax bounds the frameless videos a "none" waits for (I-275).
const coverWaitMax = 8

func depOf(in meta.Inode) coverDep {
	return coverDep{ino: in.Ino, version: in.Version, mtime: in.Mtime}
}

// cached is the remembered cover of dir, if one is remembered that still holds.
func (s *Server) cached(dir meta.Inode) (coverEntry, bool) {
	s.covers.mu.Lock()
	e, ok := s.covers.entries[dir.Ino]
	s.covers.mu.Unlock()
	if !ok || !s.valid(dir, e) {
		return coverEntry{}, false
	}
	return e, true
}

// coverKnown reports whether this node holds the folder's cover resolved, so a tile asks
// no background job for it (I-374).
func (s *Server) coverKnown(dir meta.Inode) bool {
	_, ok := s.cached(dir)
	return ok
}

// coverCached is the folder's cover and whether it is settled: resolved by this node for
// the current versions and, for a none, waiting for no video's frame. A listing reports
// only that and never reads a file to find one (I-165): GET /api/thumb of the folder
// resolves it (coverOf).
func (s *Server) coverCached(dir meta.Inode) (cover string, settled bool) {
	if dir.Type != meta.TDir {
		return "", false
	}
	e, ok := s.cached(dir)
	return e.name, ok && (e.name != "" || len(e.wait) == 0)
}

// coverOf is the folder's cover (I-165, I-257, I-275): its own art, else
// "<subfolder>/…/<entry>" for the first subfolder whose art resolves, "" when none, with
// the frameless videos a none waits for. It is the same on every node: the FSM orders
// names and the files are the same bytes. Resolving it reads candidates' tags, so only
// the folder's thumbnail request pays for it, once per version of what it read.
func (s *Server) coverOf(dir meta.Inode) coverEntry {
	if dir.Type != meta.TDir {
		return coverEntry{}
	}
	if e, ok := s.cached(dir); ok {
		return e
	}
	if e, ok := s.storedCover(dir); ok { // another node, or this one before a restart, resolved it (I-256)
		s.remember(dir, e)
		return e
	}
	var e coverEntry
	e.name = s.coverIn(dir, CoverDepth, &e.deps, &e.wait)
	if len(e.deps) == 0 {
		return coverEntry{} // the folder could not be listed
	}
	if e.name != "" {
		e.wait = nil
	} else {
		slices.SortStableFunc(e.wait, func(a, b meta.Ino) int { return s.grabRank(a) - s.grabRank(b) })
	}
	s.remember(dir, e)
	s.storeCover(dir, e)
	return e
}

// grabRank orders a none's videos for the browser to frame: those every browser decodes
// first, so the hint is not an mkv another file could replace.
func (s *Server) grabRank(ino meta.Ino) int {
	in, _ := s.o.Store.GetInodeAny(ino)
	switch strings.ToLower(path.Ext(in.Name)) {
	case ".mp4", ".m4v", ".webm", ".mov":
		return 0
	}
	return 1
}

// CoverDepth is how many folder levels below itself a folder without art of its own
// looks for some (docs/13 §5, I-257, I-275): Music/Artist/Studio/Album/CD1 takes four.
const CoverDepth = 4

// coverIn is dir's cover looking at most levels folders down, first wins: its own art,
// else "<sub>/<cover>" for the first of at most CoverSubdirs subfolders by name whose own
// cover resolves. Every inode it read is appended to deps, dir first, and every video
// without a frame to wait.
func (s *Server) coverIn(dir meta.Inode, levels int, deps *[]coverDep, wait *[]meta.Ino) string {
	kids, err := s.o.Store.ReadDir(dir.Ino)
	if err != nil {
		return ""
	}
	*deps = append(*deps, depOf(dir))
	if name := s.directCover(kids, deps, wait); name != "" || levels == 0 {
		return name
	}
	n := 0
	for i := range kids {
		k := &kids[i]
		if k.Type != meta.TDir || strings.HasPrefix(k.Name, ".") || IsOSMetadata(k.Name) || notArt(k.Name) {
			continue
		}
		if n == CoverSubdirs {
			break
		}
		n++
		if c := s.coverIn(*k, levels-1, deps, wait); c != "" {
			return k.Name + "/" + c
		}
	}
	return ""
}

// notArt names the folders a cover is never looked for in: subtitles and samples.
func notArt(dir string) bool {
	switch strings.ToLower(dir) {
	case "subs", "subtitles", "sample", "samples":
		return true
	}
	return false
}

// advert matches the images a release ships to advertise its tracker (docs/13 §5,
// I-275): `www.YTS.MX.jpg`, `YTS.BZ - Official site.jpg`. They are never art.
var advert = regexp.MustCompile(`(?i)(^|[^a-z])(www\.|yts|yify|rarbg|eztv|torrent)|official site`)

// directCover is a folder's own art among kids (I-165, I-257, I-275): the first audio
// entry by name that has a thumbnail (any, when they hold a cover.* or folder.* image,
// since every track's thumbnail is then that image; else the first with an embedded
// picture), else an artwork image (cover.*, folder.*, poster.*, in that order), else the
// first other image by name that is not an advert, else the first other file by name
// whose thumbnail is stored (a video whose frame the browser grabbed). Each track whose
// tags it reads is appended to deps (its bytes may land after its create), and each video
// without a frame to wait.
func (s *Server) directCover(kids []meta.Inode, deps *[]coverDep, wait *[]meta.Ino) string {
	first, sibling := "", false
	for i := range kids {
		k := &kids[i]
		if k.Type != meta.TFile || IsOSMetadata(k.Name) {
			continue
		}
		if isAudio(k.Name) && first == "" {
			first = k.Name
		}
		if isCoverImage(k.Name) {
			sibling = true
		}
	}
	if first != "" && sibling {
		return first
	}
	if first != "" {
		for i := range kids {
			k := &kids[i]
			if k.Type == meta.TFile && isAudio(k.Name) && !IsOSMetadata(k.Name) {
				*deps = append(*deps, depOf(*k))
				if s.hasPicture(*k) {
					return k.Name
				}
			}
		}
	}
	for _, art := range artNames {
		for i := range kids {
			if k := &kids[i]; k.Type == meta.TFile && k.Name == art {
				return art
			}
		}
	}
	for i := range kids {
		k := &kids[i]
		if k.Type == meta.TFile && isImage(k.Name) && !IsOSMetadata(k.Name) && !IsUploadTemp(k.Name) && !advert.MatchString(k.Name) {
			return k.Name
		}
	}
	for i := range kids {
		k := &kids[i]
		if k.Type != meta.TFile || isAudio(k.Name) || isImage(k.Name) || IsOSMetadata(k.Name) {
			continue
		}
		if s.hasThumb(*k) {
			return k.Name
		}
		if search.IsVideo(k.Name) && !IsUploadTemp(k.Name) && len(*wait) < coverWaitMax {
			*wait = append(*wait, k.Ino)
		}
	}
	return ""
}

// coverNames are the sibling images an audio file's thumbnail falls back to, in order.
var coverNames = []string{"cover.jpg", "cover.jpeg", "cover.png", "cover.webp", "folder.jpg", "folder.jpeg", "folder.png", "folder.webp"}

func isCoverImage(name string) bool { return slices.Contains(coverNames, name) }

// artNames are the images that are a folder's own art when no track gives one, in order
// (I-257): an album's cover, a show's or a movie's poster.
var artNames = append(slices.Clone(coverNames), "poster.jpg", "poster.jpeg", "poster.png", "poster.webp")

// hasPicture reports whether the audio file in carries an embedded picture.
func (s *Server) hasPicture(in meta.Inode) bool {
	parts, err := s.o.Store.PathOf(in.Ino)
	var f io.ReadSeekCloser
	if err == nil {
		f, err = s.o.FS.Open("/" + path.Join(parts...)) // the node resolves covers for everyone
	}
	if err != nil { // inside the trash: read through the root
		root, ok := s.trashRootOf(in)
		if !ok {
			return false
		}
		rel, err := s.o.Store.PathOf(in.Ino)
		if err != nil {
			return false
		}
		rootParts, err := s.o.Store.PathOf(root.Ino)
		if err != nil || len(rel) < len(rootParts) {
			return false
		}
		f, err = s.o.FS.OpenTrash(root.Ino, rel[len(rootParts):])
		if err != nil {
			return false
		}
	}
	defer f.Close()
	_, pic := readTags(f, int64(in.Size))
	return pic != nil
}

// trashRootOf finds the deleted root above in, if any.
func (s *Server) trashRootOf(in meta.Inode) (meta.Inode, bool) {
	cur := in
	for range 4096 {
		if cur.DeletedAt != 0 {
			return cur, true
		}
		if cur.Ino == meta.RootIno || cur.Parent == 0 {
			return meta.Inode{}, false
		}
		p, ok := s.o.Store.GetInodeAny(cur.Parent)
		if !ok {
			return meta.Inode{}, false
		}
		cur = p
	}
	return meta.Inode{}, false
}

var errNoThumb = errors.New("no thumbnail")

func noThumb(what string) error { return fmt.Errorf("%s: %w", what, errNoThumb) }
