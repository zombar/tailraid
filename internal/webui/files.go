package webui

import (
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// Entry is one directory entry or stat result (docs/13 §3).
type Entry struct {
	Name   string    `json:"name"`
	Type   string    `json:"type"` // file | dir | symlink
	Size   uint64    `json:"size"`
	Mtime  time.Time `json:"mtime"`
	Mode   uint32    `json:"mode"`
	Ino    uint64    `json:"ino"`
	Dirty  bool      `json:"dirty,omitempty"`  // acknowledged bytes not yet committed (files)
	Upload bool      `json:"upload,omitempty"` // an upload's temp, ".<name>.tailraid-upload"
	Target string    `json:"target,omitempty"` // symlinks
	Path   string    `json:"path,omitempty"`   // stat only
}

// cleanPath resolves ".", "..", repeated and backslash separators (meta.SplitPath) and
// returns the components with the canonical "/a/b" form; the root is nil, "/".
func cleanPath(raw string) ([]string, string) {
	parts := meta.SplitPath(raw)
	return parts, "/" + strings.Join(parts, "/")
}

func (s *Server) entry(in meta.Inode, canonical string) Entry {
	e := Entry{Name: in.Name, Size: in.Size, Mtime: time.Unix(0, in.Mtime).UTC(), Mode: in.Mode & 0o7777, Ino: uint64(in.Ino), Target: in.Target}
	switch in.Type {
	case meta.TDir:
		e.Type = "dir"
	case meta.TSymlink:
		e.Type = "symlink"
	default:
		e.Type = "file"
		e.Upload = IsUploadTemp(in.Name)
		if fi, err := s.o.FS.Stat(canonical); err == nil { // the overlay's size and dirty flag
			e.Size = uint64(fi.Size())
			if d, ok := fi.(interface{ Dirty() bool }); ok {
				e.Dirty = d.Dirty()
			}
		}
	}
	return e
}

func (s *Server) ls(w http.ResponseWriter, r *http.Request) {
	parts, canonical := cleanPath(r.URL.Query().Get("path"))
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		s.fail(w, err)
		return
	}
	if in.Type != meta.TDir {
		s.fail(w, fmt.Errorf("ls %s: %w", canonical, meta.ErrNotDir))
		return
	}
	kids, err := s.o.Store.ReadDir(in.Ino)
	if err != nil {
		s.fail(w, err)
		return
	}
	entries := make([]Entry, 0, len(kids))
	for i := range kids {
		if IsOSMetadata(kids[i].Name) { // kept in the volume, reachable by path (I-140)
			continue
		}
		if in.Ino == meta.RootIno && kids[i].Name == ReservedDir { // the node's own tree (docs/13 §5, I-195)
			continue
		}
		entries = append(entries, s.entry(kids[i], path.Join(canonical, kids[i].Name)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": canonical, "entries": entries})
}

func (s *Server) stat(w http.ResponseWriter, r *http.Request) {
	parts, canonical := cleanPath(r.URL.Query().Get("path"))
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		s.fail(w, err)
		return
	}
	e := s.entry(in, canonical)
	e.Path = canonical
	writeJSON(w, http.StatusOK, e)
}

// get serves a file through http.ServeContent: Range, 206/416, HEAD and If-None-Match
// come from the standard library; the ETag is omitted while the file is dirty (I-69).
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	parts, canonical := cleanPath(r.PathValue("path"))
	if len(parts) == 0 {
		s.fail(w, fmt.Errorf("get %s: %w", canonical, meta.ErrIsDir))
		return
	}
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch in.Type {
	case meta.TDir:
		s.fail(w, fmt.Errorf("get %s: %w", canonical, meta.ErrIsDir))
		return
	case meta.TSymlink:
		writeErr(w, http.StatusBadRequest, "get "+canonical+": is a symlink")
		return
	case meta.TFile:
	}
	f, err := s.o.FS.Open(canonical)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	fi, err := s.o.FS.Stat(canonical)
	if err != nil {
		s.fail(w, err)
		return
	}
	h := w.Header()
	if d, ok := fi.(interface{ Dirty() bool }); !ok || !d.Dirty() {
		h.Set("ETag", fmt.Sprintf(`"%d.%d.%d"`, in.Ino, in.Gen, in.Version))
	}
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
