package webui

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sync"

	"github.com/dhowden/tag"

	"github.com/zombar/tailraid/internal/meta"
)

// Tag reads are bounded (docs/13 §6, I-164): the first TagHead bytes hold an ID3v2 tag
// with its picture, the last TagTail an ID3v1 tag; nothing between is ever streamed.
const (
	TagHead = 10 << 20
	TagTail = 128
)

// Tags is /api/tags (docs/13 §3).
type Tags struct {
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	AlbumArtist string `json:"album_artist"`
	Track       int    `json:"track"`
	Tracks      int    `json:"tracks"`
	Disc        int    `json:"disc"`
	Year        int    `json:"year"`
	Genre       string `json:"genre"`
	HasCover    bool   `json:"has_cover"`
}

// boundedSeeker exposes only the head and the tail of a file: a read anywhere else is
// an EOF, so a parser that walks the audio frames stops at TagHead.
type boundedSeeker struct {
	f    io.ReadSeeker
	size int64
	pos  int64
}

func (b *boundedSeeker) Read(p []byte) (int, error) {
	tail := max(b.size-TagTail, 0)
	switch {
	case b.pos < TagHead && b.pos < tail:
		p = p[:min(int64(len(p)), min(TagHead, tail)-b.pos)]
	case b.pos < tail:
		return 0, io.EOF
	}
	n, err := b.f.Read(p)
	b.pos += int64(n)
	return n, err
}

func (b *boundedSeeker) Seek(off int64, whence int) (int64, error) {
	pos, err := b.f.Seek(off, whence)
	if err == nil {
		b.pos = pos
	}
	return pos, err
}

// readTags parses an audio file's tags; a file without any, or with tags the parser
// rejects, is empty tags and no error.
func readTags(f io.ReadSeeker, size int64) (Tags, *tag.Picture) {
	m, err := tag.ReadFrom(&boundedSeeker{f: f, size: size})
	if err != nil || m == nil {
		return Tags{}, nil
	}
	t := Tags{Title: m.Title(), Artist: m.Artist(), Album: m.Album(), AlbumArtist: m.AlbumArtist(), Year: m.Year(), Genre: m.Genre()}
	t.Track, t.Tracks = m.Track()
	t.Disc, _ = m.Disc()
	pic := m.Picture()
	t.HasCover = pic != nil && len(pic.Data) > 0
	if !t.HasCover {
		pic = nil
	}
	return t, pic
}

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

// coverOf is the folder's first audio entry by name that has a picture, "" when none
// (I-165). It is the same on every node: the FSM orders names and the files are the
// same bytes.
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
