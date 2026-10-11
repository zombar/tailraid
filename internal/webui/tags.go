package webui

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
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

// open opens a file for reading, live or inside a deleted root.
func (s *Server) open(t target) (io.ReadSeekCloser, error) {
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

// coverCache remembers each folder's cover entry by its listing, so a listing costs
// no tag parse twice (docs/13 §5).
type coverCache struct {
	mu      sync.Mutex
	entries map[meta.Ino]coverEntry
}

// coverEntry is keyed by the folder's Version, which the FSM bumps whenever its name
// table changes (docs/01), and its Mtime.
type coverEntry struct {
	version uint64
	mtime   int64
	name    string
}

const coverCacheMax = 1024

// coverCached is the folder's cover when this node has already resolved it for the
// folder's current version; a listing reports only that and never reads a file to find
// one (I-165): GET /api/thumb of the folder resolves it (coverOf).
func (s *Server) coverCached(dir meta.Inode) string {
	if dir.Type != meta.TDir {
		return ""
	}
	s.covers.mu.Lock()
	defer s.covers.mu.Unlock()
	if e, ok := s.covers.entries[dir.Ino]; ok && e.version == dir.Version && e.mtime == dir.Mtime {
		return e.name
	}
	return ""
}

// coverOf is the folder's first audio entry by name that has a picture, "" when none
// (I-165). It is the same on every node: the FSM orders names and the files are the
// same bytes. Resolving it reads each candidate's tags, so only the folder's thumbnail
// request pays for it, once per folder version.
func (s *Server) coverOf(dir meta.Inode) string {
	if dir.Type != meta.TDir {
		return ""
	}
	s.covers.mu.Lock()
	if e, ok := s.covers.entries[dir.Ino]; ok && e.version == dir.Version && e.mtime == dir.Mtime {
		s.covers.mu.Unlock()
		return e.name
	}
	s.covers.mu.Unlock()
	kids, err := s.o.Store.ReadDir(dir.Ino)
	if err != nil {
		return ""
	}
	name := ""
	for i := range kids {
		k := &kids[i]
		if k.Type != meta.TFile || !isAudio(k.Name) || IsOSMetadata(k.Name) {
			continue
		}
		if s.hasPicture(*k) {
			name = k.Name
			break
		}
	}
	s.covers.mu.Lock()
	if s.covers.entries == nil || len(s.covers.entries) >= coverCacheMax {
		s.covers.entries = map[meta.Ino]coverEntry{}
	}
	s.covers.entries[dir.Ino] = coverEntry{version: dir.Version, mtime: dir.Mtime, name: name}
	s.covers.mu.Unlock()
	return name
}

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
