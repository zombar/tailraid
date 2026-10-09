package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/zombar/tailraid/internal/meta"
)

// put stores the body as the file at path through the vfs: the NFS write path (overlay,
// fsynced WAL, flusher), one WAL record per chunk-sized read (I-70). A write that meets
// the WAL or dirty-byte cap waits for the flusher inside the vfs (docs/04 §3).
func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	parts, canonical := cleanPath(r.PathValue("path"))
	if len(parts) == 0 {
		s.fail(w, fmt.Errorf("put %s: %w", canonical, meta.ErrIsDir))
		return
	}
	in, err := s.o.Store.Resolve(parts)
	existed := err == nil
	switch {
	case existed && in.Type == meta.TDir:
		s.fail(w, fmt.Errorf("put %s: %w", canonical, meta.ErrIsDir))
		return
	case existed && in.Type == meta.TSymlink:
		writeErr(w, http.StatusBadRequest, "put "+canonical+": is a symlink")
		return
	case existed && r.Header.Get("If-None-Match") == "*":
		writeErr(w, http.StatusPreconditionFailed, "put "+canonical+": exists")
		return
	case !existed && !errors.Is(err, meta.ErrNotExist):
		s.fail(w, err)
		return
	}
	f, err := s.o.FS.OpenFile(canonical, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		s.fail(w, err)
		return
	}
	buf := make([]byte, s.o.Store.Volume().ChunkSize)
	for {
		n, rerr := io.ReadFull(r.Body, buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				_ = f.Close()
				s.fail(w, err)
				return
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			_ = f.Close() // what was acknowledged stays, as with a dropped NFS client
			writeErr(w, http.StatusBadRequest, "reading the body: "+rerr.Error())
			return
		}
	}
	if err := f.Close(); err != nil {
		s.fail(w, err)
		return
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// mkdir creates a directory and its parents (I-70).
func (s *Server) mkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "mkdir: "+err.Error())
		return
	}
	parts, canonical := cleanPath(req.Path)
	if len(parts) == 0 {
		s.fail(w, fmt.Errorf("mkdir %s: %w", canonical, meta.ErrIsDir))
		return
	}
	if _, err := s.o.Store.Resolve(parts); err == nil {
		s.fail(w, fmt.Errorf("mkdir %s: %w", canonical, meta.ErrExist))
		return
	} else if !errors.Is(err, meta.ErrNotExist) {
		s.fail(w, err)
		return
	}
	if err := s.o.FS.MkdirAll(canonical, 0o755); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": canonical})
}
