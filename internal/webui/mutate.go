package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
)

// Body bounds (docs/08 I-116, docs/13 §3): a single-entry route reads 4 KiB, a batch
// route 256 KiB.
const (
	singleBodyCap = 4 << 10
	batchBodyCap  = 256 << 10
	// MaxBatch bounds the paths of one move or delete (413 above).
	MaxBatch = 1000
)

// decode reads a JSON body of at most limit bytes; a larger one is a 400 that says so.
func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("body too large: at most %d bytes", limit))
		} else {
			writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		}
		return false
	}
	return true
}

// validName refuses what no entry may be called.
func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("name %q: %w", name, meta.ErrBadCommand)
	}
	if len(name) > meta.MaxNameLen {
		return fmt.Errorf("name of %d bytes: %w", len(name), meta.ErrNameTooLong)
	}
	return nil
}

// Failure is one item of a batch that failed (docs/13 §3, I-159).
type Failure struct {
	Path   string `json:"path"`
	Error  string `json:"error"`
	Status int    `json:"status"`
}

// batchPaths reads a batch body and checks its bounds; nil when the response is sent.
func (s *Server) batchPaths(w http.ResponseWriter, r *http.Request, v interface{ paths() []string }) []string {
	if !decode(w, r, batchBodyCap, v) {
		return nil
	}
	ps := v.paths()
	switch {
	case len(ps) == 0:
		writeErr(w, http.StatusBadRequest, "no paths")
		return nil
	case len(ps) > MaxBatch:
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%d paths: at most %d per request", len(ps), MaxBatch))
		return nil
	}
	return ps
}

type pathsBody struct {
	Paths []string `json:"paths"`
	To    string   `json:"to"`
}

func (b *pathsBody) paths() []string { return b.Paths }

// rename gives one entry a new name in its folder.
func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !decode(w, r, singleBodyCap, &req) {
		return
	}
	parts, canonical := cleanPath(req.Path)
	if len(parts) == 0 || reserved(parts) {
		writeErr(w, http.StatusBadRequest, "rename "+canonical+": not a user entry")
		return
	}
	if err := validName(req.Name); err != nil {
		s.fail(w, fmt.Errorf("rename %s: %w", canonical, err))
		return
	}
	dir, err := s.o.Store.Resolve(parts[:len(parts)-1])
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, taken := s.o.Store.Lookup(dir.Ino, req.Name); taken && req.Name != parts[len(parts)-1] {
		s.fail(w, fmt.Errorf("rename %s to %s: %w", canonical, req.Name, meta.ErrExist))
		return
	}
	to := path.Join(path.Dir(canonical), req.Name)
	if err := s.o.FS.Rename(canonical, to); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": to})
}

// move puts each path into the folder `to`, one Rename each, never over an existing
// entry; every item is tried and reported (I-159).
func (s *Server) move(w http.ResponseWriter, r *http.Request) {
	var req pathsBody
	paths := s.batchPaths(w, r, &req)
	if paths == nil {
		return
	}
	toParts, to := cleanPath(req.To)
	if reserved(toParts) {
		writeErr(w, http.StatusBadRequest, "move to "+to+": not a user folder")
		return
	}
	dest, err := s.o.Store.Resolve(toParts)
	if err != nil {
		s.fail(w, err)
		return
	}
	if dest.Type != meta.TDir {
		s.fail(w, fmt.Errorf("move to %s: %w", to, meta.ErrNotDir))
		return
	}
	moved := []string{}
	failed := []Failure{}
	for _, raw := range paths {
		parts, canonical := cleanPath(raw)
		var err error
		switch {
		case len(parts) == 0 || reserved(parts):
			err = fmt.Errorf("%s: not a user entry: %w", canonical, meta.ErrBadCommand)
		default:
			name := parts[len(parts)-1]
			if _, taken := s.o.Store.Lookup(dest.Ino, name); taken {
				err = fmt.Errorf("%s is taken in %s: %w", name, to, meta.ErrExist)
			} else {
				err = s.o.FS.Rename(canonical, path.Join(to, name))
			}
		}
		if err != nil {
			failed = append(failed, Failure{Path: canonical, Error: err.Error(), Status: errStatus(err)})
			continue
		}
		moved = append(moved, path.Join(to, parts[len(parts)-1]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"moved": moved, "failed": failed})
}

type deleteBody struct {
	Paths []string `json:"paths"`
}

func (b *deleteBody) paths() []string { return b.Paths }

// del moves each path into the trash, a folder as one unit (I-158, I-159).
func (s *Server) del(w http.ResponseWriter, r *http.Request) {
	var req deleteBody
	paths := s.batchPaths(w, r, &req)
	if paths == nil {
		return
	}
	deleted := []string{}
	failed := []Failure{}
	for _, raw := range paths {
		parts, canonical := cleanPath(raw)
		var err error
		if len(parts) == 0 || reserved(parts) {
			err = fmt.Errorf("%s: not a user entry: %w", canonical, meta.ErrBadCommand)
		} else {
			err = s.o.FS.Trash(canonical)
		}
		if err != nil {
			failed = append(failed, Failure{Path: canonical, Error: err.Error(), Status: errStatus(err)})
			continue
		}
		deleted = append(deleted, canonical)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "failed": failed})
}

// restore puts a deleted root back (docs/13 §3): in its place, or where `to` and
// `name` say; a taken name and a deleted destination are 409s the page resolves.
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ino  uint64 `json:"ino"`
		To   string `json:"to"`
		Name string `json:"name"`
	}
	if !decode(w, r, singleBodyCap, &req) {
		return
	}
	var parent meta.Ino
	if req.To != "" {
		toParts, to := cleanPath(req.To)
		if reserved(toParts) {
			writeErr(w, http.StatusBadRequest, "restore to "+to+": not a user folder")
			return
		}
		dest, err := s.o.Store.Resolve(toParts)
		if err != nil {
			s.fail(w, err)
			return
		}
		if dest.Type != meta.TDir {
			s.fail(w, fmt.Errorf("restore to %s: %w", to, meta.ErrNotDir))
			return
		}
		parent = dest.Ino
	}
	if req.Name != "" {
		if err := validName(req.Name); err != nil {
			s.fail(w, err)
			return
		}
	}
	err := s.o.FS.Restore(meta.Ino(req.Ino), parent, req.Name)
	switch {
	case errors.Is(err, meta.ErrExist):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "taken": true})
		return
	case errors.Is(err, meta.ErrInTrash):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "in_trash": true})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	parts, err := s.o.Store.PathOf(meta.Ino(req.Ino))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": "/" + strings.Join(parts, "/")})
}

// purge deletes one deleted root for good, or empties the trash.
func (s *Server) purge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ino uint64 `json:"ino"`
		All bool   `json:"all"`
	}
	if !decode(w, r, singleBodyCap, &req) {
		return
	}
	var err error
	switch {
	case req.All:
		err = s.o.FS.PurgeAll()
	case req.Ino != 0:
		err = s.o.FS.Purge(meta.Ino(req.Ino))
	default:
		writeErr(w, http.StatusBadRequest, "purge: an ino or all is required")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// flags sets a folder's protected and nsfw flags (I-163).
func (s *Server) flags(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path      string `json:"path"`
		Protected *bool  `json:"protected"`
		NSFW      *bool  `json:"nsfw"`
	}
	if !decode(w, r, singleBodyCap, &req) {
		return
	}
	parts, canonical := cleanPath(req.Path)
	if len(parts) == 0 || reserved(parts) {
		writeErr(w, http.StatusBadRequest, "flags "+canonical+": the root and /.tailraid carry no flags")
		return
	}
	in, err := s.o.Store.Resolve(parts)
	if err != nil {
		s.fail(w, err)
		return
	}
	if in.Type != meta.TDir {
		s.fail(w, fmt.Errorf("flags %s: %w", canonical, meta.ErrNotDir))
		return
	}
	flags := in.Flags
	set := func(bit uint8, v *bool) {
		if v == nil {
			return
		}
		if *v {
			flags |= bit
		} else {
			flags &^= bit
		}
	}
	set(meta.FlagProtected, req.Protected)
	set(meta.FlagNSFW, req.NSFW)
	if flags != in.Flags {
		if err := s.o.FS.SetFlags(canonical, flags); err != nil {
			s.fail(w, err)
			return
		}
	}
	in, err = s.o.Store.Resolve(parts)
	if err != nil {
		s.fail(w, err)
		return
	}
	e := s.entry(in, canonical)
	e.Path = canonical
	writeJSON(w, http.StatusOK, e)
}

// trashList is admin.TrashList for this node's FSM copy, with the retention's purge_at.
func (s *Server) trashList() []admin.TrashEntry {
	return admin.TrashList(s.o.Store, nil, s.o.Retention)
}
