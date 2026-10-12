package webui

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/vfs"
)

// Entry is one directory entry or stat result (docs/13 §3).
type Entry struct {
	Name      string    `json:"name"`
	Type      string    `json:"type"` // file | dir | symlink
	Size      uint64    `json:"size"`
	Mtime     time.Time `json:"mtime"`
	Mode      uint32    `json:"mode"`
	Ino       uint64    `json:"ino"`
	Version   uint64    `json:"version"`              // bumps on every commit: the thumbnail key and the PUT /api/thumb check (§5)
	Dirty     bool      `json:"dirty,omitempty"`      // acknowledged bytes not yet committed (files)
	Upload    bool      `json:"upload,omitempty"`     // an upload's temp, ".<name>.tailraid-upload"
	Target    string    `json:"target,omitempty"`     // symlinks
	Path      string    `json:"path,omitempty"`       // stat only
	Protected bool      `json:"protected,omitempty"`  // the entry's own flag (directories)
	NSFW      bool      `json:"nsfw,omitempty"`       // the entry's own flag (directories)
	Library   string    `json:"library,omitempty"`    // the entry's own library kind (directories; docs/18 §9)
	DeletedAt string    `json:"deleted_at,omitempty"` // a deleted root (RFC 3339 UTC)
	PurgeAt   string    `json:"purge_at,omitempty"`   // when the retention purge takes it; absent with retention 0
	Thumb     bool      `json:"thumb,omitempty"`      // GET /api/thumb answers without a PUT: stored, or an image or audio file
	Cover     string    `json:"cover,omitempty"`      // directories: the audio entry whose picture is the folder's tile (I-165)
	NoCover   bool      `json:"no_cover,omitempty"`   // directories: resolved to no cover (I-165)
}

// DirInfo describes the listed folder itself: the effective flags (its own or an
// ancestor's), whether this node keeps it, its place in the trash, and the snapshot it is
// read as of (at=).
type DirInfo struct {
	Snapshot  *SnapInfo `json:"snapshot,omitempty"`
	Protected bool      `json:"protected"`
	NSFW      bool      `json:"nsfw"`
	Library   string    `json:"library,omitempty"` // effective: the nearest flagged ancestor's kind (docs/18 §9)
	Pinned    bool      `json:"pinned"`
	DeletedAt string    `json:"deleted_at,omitempty"`
	PurgeAt   string    `json:"purge_at,omitempty"`
	Cover     string    `json:"cover,omitempty"`
	NoCover   bool      `json:"no_cover,omitempty"`
}

// cleanPath resolves ".", "..", repeated and backslash separators (meta.SplitPath) and
// returns the components with the canonical "/a/b" form; the root is nil, "/".
func cleanPath(raw string) ([]string, string) {
	parts := meta.SplitPath(raw)
	return parts, "/" + strings.Join(parts, "/")
}

// reserved reports whether parts names /.tailraid or anything under it, which no user
// operation may touch (docs/13 §5, I-160).
func reserved(parts []string) bool {
	return len(parts) > 0 && parts[0] == meta.ReservedDir
}

// target is a resolved request path: live, inside the deleted root `in` (§4), or as of
// the snapshot `at` (docs/16 §6), where in carries the snapshot entry's fields.
type target struct {
	in        meta.Inode
	parts     []string
	canonical string
	root      meta.Inode // the deleted root when addressed with in=
	trashed   bool
	snap      *vfs.SnapshotRef // the snapshot when addressed with at=
	vpath     string           // the entry's /.snapshots path when snap is set
}

// resolveTarget resolves the path (and `in` or `at`) of a read request.
func (s *Server) resolveTarget(r *http.Request, raw string) (target, error) {
	parts, canonical := cleanPath(raw)
	t := target{parts: parts, canonical: canonical}
	if r.URL.Query().Has("at") {
		return s.resolveAt(r, t)
	}
	if q := r.URL.Query().Get("in"); q != "" {
		ino, err := strconv.ParseUint(q, 10, 64)
		if err != nil {
			return t, fmt.Errorf("in=%q: %w", q, meta.ErrBadCommand)
		}
		root, ok := s.o.Store.GetInodeAny(meta.Ino(ino))
		if !ok || root.DeletedAt == 0 {
			return t, fmt.Errorf("in=%d: not a deleted entry: %w", ino, meta.ErrNotExist)
		}
		in, err := s.o.Store.ResolveFrom(root.Ino, parts)
		if err != nil {
			return t, err
		}
		t.in, t.root, t.trashed = in, root, true
		return t, nil
	}
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		return t, err
	}
	t.in = in
	return t, nil
}

func typeName(in meta.Inode) string {
	switch in.Type {
	case meta.TDir:
		return "dir"
	case meta.TSymlink:
		return "symlink"
	default:
		return "file"
	}
}

func rfc3339(ns int64) string {
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

// purgeAt is when the retention purge takes a root deleted at ns ("" with retention 0).
func (s *Server) purgeAt(ns int64) string {
	if ns == 0 || s.o.Retention <= 0 {
		return ""
	}
	return time.Unix(0, ns).Add(s.o.Retention).UTC().Format(time.RFC3339)
}

// entry describes in; canonical is its live path ("" inside the trash, where the
// overlay's view is not consulted).
func (s *Server) entry(in meta.Inode, canonical string) Entry {
	e := Entry{Name: in.Name, Type: typeName(in), Size: in.Size, Mtime: time.Unix(0, in.Mtime).UTC(), Mode: in.Mode & 0o7777, Ino: uint64(in.Ino), Version: in.Version, Target: in.Target}
	e.Protected = in.Flags&meta.FlagProtected != 0
	e.NSFW = in.Flags&meta.FlagNSFW != 0
	if k := in.Library(); k != meta.LibraryNone {
		e.Library = k.String()
	}
	e.DeletedAt = rfc3339(in.DeletedAt)
	e.PurgeAt = s.purgeAt(in.DeletedAt)
	switch in.Type {
	case meta.TFile:
		e.Upload = IsUploadTemp(in.Name)
		if canonical != "" {
			if fi, err := s.o.FS.Stat(canonical); err == nil { // the overlay's size and dirty flag
				e.Size = uint64(fi.Size())
				if d, ok := fi.(interface{ Dirty() bool }); ok {
					e.Dirty = d.Dirty()
				}
			}
		}
		e.Thumb = s.hasThumb(in) || isImage(in.Name) || isAudio(in.Name)
	case meta.TDir:
		cover, known := s.coverCached(in) // known covers only: a listing reads no file (I-165)
		e.Cover, e.NoCover = cover, known && cover == ""
	case meta.TSymlink:
	}
	return e
}

// effective reports the flags in's ancestors and in itself carry (docs/13 §4); the
// library kind is the nearest flagged ancestor's, in's own first (docs/18 §9).
func (s *Server) effective(in meta.Inode) (protected, nsfw bool, library meta.LibraryKind) {
	seen := 0
	for cur := in; ; seen++ {
		protected = protected || cur.Flags&meta.FlagProtected != 0
		nsfw = nsfw || cur.Flags&meta.FlagNSFW != 0
		if library == meta.LibraryNone {
			library = cur.Library()
		}
		if cur.Ino == meta.RootIno || cur.Parent == 0 || seen > 4096 {
			return
		}
		p, ok := s.o.Store.GetInodeAny(cur.Parent)
		if !ok {
			return
		}
		cur = p
	}
}

func (s *Server) ls(w http.ResponseWriter, r *http.Request) {
	t, err := s.resolveTarget(r, r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if t.in.Type != meta.TDir {
		s.fail(w, fmt.Errorf("ls %s: %w", t.canonical, meta.ErrNotDir))
		return
	}
	if t.snap != nil {
		s.lsAt(w, t)
		return
	}
	kids, err := s.o.Store.ReadDir(t.in.Ino)
	if err != nil {
		s.fail(w, err)
		return
	}
	entries := make([]Entry, 0, len(kids))
	for i := range kids {
		if IsOSMetadata(kids[i].Name) { // kept in the volume, reachable by path (I-140)
			continue
		}
		if !t.trashed && len(t.parts) == 0 && kids[i].Name == meta.ReservedDir { // the thumbnails (I-160)
			continue
		}
		live := ""
		if !t.trashed {
			live = path.Join(t.canonical, kids[i].Name)
		}
		entries = append(entries, s.entry(kids[i], live))
	}
	if !t.trashed { // the folder's deleted roots, newest first, after the live entries
		roots := s.o.Store.TrashOf(t.in.Ino)
		for i := range roots {
			entries = append(entries, s.entry(roots[i], ""))
		}
	}
	cover, known := s.coverCached(t.in)
	dir := DirInfo{Cover: cover, NoCover: known && cover == ""}
	var lib meta.LibraryKind
	dir.Protected, dir.NSFW, lib = s.effective(t.in)
	if lib != meta.LibraryNone {
		dir.Library = lib.String()
	}
	if t.trashed {
		dir.DeletedAt, dir.PurgeAt = rfc3339(t.root.DeletedAt), s.purgeAt(t.root.DeletedAt)
	} else {
		dir.Pinned = s.pinned(t.canonical)
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": t.canonical, "dir": dir, "entries": entries})
}

func (s *Server) stat(w http.ResponseWriter, r *http.Request) {
	t, err := s.resolveTarget(r, r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	live := ""
	if !t.trashed {
		live = t.canonical
	}
	if t.snap != nil {
		e := snapEntry(t.in)
		e.Path = t.canonical
		writeJSON(w, http.StatusOK, e)
		return
	}
	e := s.entry(t.in, live)
	e.Path = t.canonical
	var lib meta.LibraryKind
	e.Protected, e.NSFW, lib = s.effective(t.in)
	e.Library = ""
	if lib != meta.LibraryNone {
		e.Library = lib.String()
	}
	if t.trashed {
		e.DeletedAt, e.PurgeAt = rfc3339(t.root.DeletedAt), s.purgeAt(t.root.DeletedAt)
	}
	writeJSON(w, http.StatusOK, e)
}

// pinned reports whether this node keeps the folder at canonical (docs/13 §4).
func (s *Server) pinned(canonical string) bool {
	if s.o.Pins == nil {
		return false
	}
	for _, p := range s.o.Pins.List().Pins {
		if p.Path == canonical {
			return true
		}
	}
	return false
}

// trash lists every deleted root for the CLI and the dashboard (docs/13 §3).
func (s *Server) trash(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": s.trashList()})
}

// get serves a file through http.ServeContent: Range, 206/416, HEAD and If-None-Match
// come from the standard library; the ETag is omitted while the file is dirty (I-69).
// With in= the bytes come from inside a deleted root, with the same headers.
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	parts, canonical := cleanPath(r.PathValue("path"))
	if len(parts) == 0 && r.URL.Query().Get("in") == "" {
		s.fail(w, fmt.Errorf("get %s: %w", canonical, meta.ErrIsDir))
		return
	}
	t, err := s.resolveTarget(r, r.PathValue("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	in := t.in
	switch in.Type {
	case meta.TDir:
		s.fail(w, fmt.Errorf("get %s: %w", canonical, meta.ErrIsDir))
		return
	case meta.TSymlink:
		writeErr(w, http.StatusBadRequest, "get "+canonical+": is a symlink")
		return
	case meta.TFile:
	}
	h := w.Header()
	var f interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
		Close() error
	}
	switch {
	case t.snap != nil: // a snapshot never changes: its ID and the entry name the bytes
		f, err = s.o.FS.Open(t.vpath)
		if err != nil {
			s.fail(w, err)
			return
		}
		h.Set("ETag", fmt.Sprintf(`"s%d.%d.%d"`, t.snap.Snapshot.ID, in.Ino, in.Gen))
	case t.trashed:
		f, err = s.o.FS.OpenTrash(t.root.Ino, t.parts)
		if err != nil {
			s.fail(w, err)
			return
		}
		h.Set("ETag", fmt.Sprintf(`"%d.%d.%d"`, in.Ino, in.Gen, in.Version))
	default:
		f, err = s.o.FS.Open(canonical)
		if err != nil {
			s.fail(w, err)
			return
		}
		fi, err := s.o.FS.Stat(canonical)
		if err != nil {
			_ = f.Close()
			s.fail(w, err)
			return
		}
		if d, ok := fi.(interface{ Dirty() bool }); !ok || !d.Dirty() {
			h.Set("ETag", fmt.Sprintf(`"%d.%d.%d"`, in.Ino, in.Gen, in.Version))
		}
	}
	defer f.Close()
	h.Set("Content-Type", contentType(in.Name))
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") == "1" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": in.Name}))
	}
	http.ServeContent(w, r, in.Name, time.Time{}, f)
}

// contentType is the type by extension, with overrides for what Go's table lacks or gets
// wrong for a media library; unknown types are octet-stream and nothing is sniffed.
func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := typeOverrides[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

var typeOverrides = map[string]string{
	".mkv":  "video/x-matroska",
	".m4v":  "video/mp4",
	".mov":  "video/quicktime",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".opus": "audio/ogg",
	".heic": "image/heic",
	".md":   "text/markdown; charset=utf-8",
	".ts":   "application/octet-stream", // not MPEG-TS, not TypeScript
}

// isImage reports a file the node can decode for a thumbnail (§5).
func isImage(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return true
	}
	return false
}

// isAudio reports a file /api/tags reads (§6).
func isAudio(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp3", ".m4a", ".aac", ".ogg", ".oga", ".opus", ".wav", ".flac":
		return true
	}
	return false
}

// errNotFile is the 400 for a file operation on a directory or symlink.
var errNotFile = errors.New("not a file")
