package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/dhowden/tag"

	"github.com/zombar/tailraid/internal/mediatag"
	"github.com/zombar/tailraid/internal/meta"
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
		return s.o.FS.Open(t.vpath)
	}
	if t.trashed {
		return s.o.FS.OpenTrash(t.root.Ino, t.parts)
	}
	return s.o.FS.Open(t.canonical)
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

// coverEntry holds while every folder it read is unchanged: the folder itself first, then
// each subfolder it looked into. The FSM bumps a folder's Version whenever its name table
// changes (docs/01).
type coverEntry struct {
	deps []coverDep
	name string
}

type coverDep struct {
	ino     meta.Ino
	version uint64
	mtime   int64
}

// coverRecord is a folder's resolved cover as stored in the volume beside the thumbnails
// (docs/13 §5, I-256): the choice, "" for none, and every folder it read, so any node
// serves it while they are unchanged and resolves it again once one changed.
type coverRecord struct {
	Name string           `json:"name"`
	Deps []coverRecordDep `json:"deps"`
}

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

// current reports whether every folder deps names is as it was when they were read.
func (s *Server) current(deps []coverDep) bool {
	for _, d := range deps {
		if in, ok := s.o.Store.GetInodeAny(d.ino); !ok || depOf(in) != d {
			return false
		}
	}
	return true
}

// storedCover is the folder's cover from its record in the volume, when the record exists
// and every folder it read is unchanged: one small read where resolving reads the tags of
// the folder's tracks over the network.
func (s *Server) storedCover(dir meta.Inode) (coverEntry, bool) {
	f, err := s.o.FS.Open(path.Join(ThumbDir, coverKey(dir)))
	if err != nil {
		return coverEntry{}, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return coverEntry{}, false
	}
	var rec coverRecord
	if json.Unmarshal(b, &rec) != nil || len(rec.Deps) == 0 {
		return coverEntry{}, false
	}
	e := coverEntry{name: rec.Name}
	for _, d := range rec.Deps {
		e.deps = append(e.deps, coverDep{ino: d.Ino, version: d.Version, mtime: d.Mtime})
	}
	if e.deps[0] != depOf(dir) || !s.current(e.deps[1:]) {
		return coverEntry{}, false
	}
	return e, true
}

// storeCover writes the folder's resolved cover to the volume for every node; a failure
// only costs another node the resolve.
func (s *Server) storeCover(dir meta.Inode, e coverEntry) {
	rec := coverRecord{Name: e.name}
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

// CoverSubdirs is how many subfolders a folder without a cover of its own looks into,
// one level deep (docs/13 §5, I-165).
const CoverSubdirs = 16

func depOf(in meta.Inode) coverDep {
	return coverDep{ino: in.Ino, version: in.Version, mtime: in.Mtime}
}

// cached is the remembered cover of dir and whether one is remembered for the current
// versions of everything it read.
func (s *Server) cached(dir meta.Inode) (string, bool) {
	s.covers.mu.Lock()
	e, ok := s.covers.entries[dir.Ino]
	s.covers.mu.Unlock()
	if !ok || len(e.deps) == 0 || e.deps[0] != depOf(dir) || !s.current(e.deps[1:]) {
		return "", false
	}
	return e.name, true
}

// coverCached is the folder's cover and whether this node has already resolved it for
// the current versions; a listing reports only that and never reads a file to find one
// (I-165): GET /api/thumb of the folder resolves it (coverOf).
func (s *Server) coverCached(dir meta.Inode) (string, bool) {
	if dir.Type != meta.TDir {
		return "", false
	}
	return s.cached(dir)
}

// coverOf is the folder's first audio entry by name that has a thumbnail, else
// "<subfolder>/<entry>" for the first of at most CoverSubdirs subfolders whose own audio
// gives one, "" when none (I-165). It is the same on every node: the FSM orders names
// and the files are the same bytes. Resolving it reads candidates' tags, so only the
// folder's thumbnail request pays for it, once per version of what it read.
func (s *Server) coverOf(dir meta.Inode) string {
	if dir.Type != meta.TDir {
		return ""
	}
	if name, ok := s.cached(dir); ok {
		return name
	}
	if e, ok := s.storedCover(dir); ok { // another node, or this one before a restart, resolved it (I-256)
		s.remember(dir, e)
		return e.name
	}
	kids, err := s.o.Store.ReadDir(dir.Ino)
	if err != nil {
		return ""
	}
	deps := []coverDep{depOf(dir)}
	name := s.directCover(kids)
	if name == "" {
		n := 0
		for i := range kids {
			k := &kids[i]
			if k.Type != meta.TDir {
				continue
			}
			if n == CoverSubdirs {
				break
			}
			n++
			sub, err := s.o.Store.ReadDir(k.Ino)
			if err != nil {
				continue
			}
			deps = append(deps, depOf(*k))
			if c := s.directCover(sub); c != "" {
				name = k.Name + "/" + c
				break
			}
		}
	}
	e := coverEntry{deps: deps, name: name}
	s.remember(dir, e)
	s.storeCover(dir, e)
	return name
}

// directCover is the first audio entry of kids by name that has a thumbnail: any, when
// they hold a cover.* or folder.* image (every track's thumbnail is then that image),
// else the first with an embedded picture.
func (s *Server) directCover(kids []meta.Inode) string {
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
	if first == "" {
		return ""
	}
	if sibling {
		return first
	}
	for i := range kids {
		k := &kids[i]
		if k.Type == meta.TFile && isAudio(k.Name) && !IsOSMetadata(k.Name) && s.hasPicture(*k) {
			return k.Name
		}
	}
	return ""
}

// coverNames are the sibling images an audio file's thumbnail falls back to, in order.
var coverNames = []string{"cover.jpg", "cover.jpeg", "cover.png", "cover.webp", "folder.jpg", "folder.jpeg", "folder.png", "folder.webp"}

func isCoverImage(name string) bool { return slices.Contains(coverNames, name) }

// hasPicture reports whether the audio file in carries an embedded picture.
func (s *Server) hasPicture(in meta.Inode) bool {
	parts, err := s.o.Store.PathOf(in.Ino)
	var f io.ReadSeekCloser
	if err == nil {
		f, err = s.o.FS.Open("/" + path.Join(parts...))
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
