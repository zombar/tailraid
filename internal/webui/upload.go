package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
)

// Uploads are atomic and resumable, and the vfs is their only state (docs/13 §4, I-81,
// I-82): the body streams into a hidden sibling of the target and one Rename publishes it
// once every announced byte has arrived.
const (
	uploadSuffix = meta.UploadSuffix
	// DefaultUploadSlots bounds concurrent uploads; each holds one chunk-sized buffer.
	DefaultUploadSlots = 4
	// DefaultBodyTimeout is how long an upload body may send nothing before it is dropped.
	DefaultBodyTimeout = 60 * time.Second
	// UploadTTL is how long an upload temp may sit untouched before the sweep removes it.
	UploadTTL = 24 * time.Hour
)

// IsUploadTemp reports whether name is an upload's temp file, ".<name>.tailraid-upload".
func IsUploadTemp(name string) bool { return meta.IsUploadTemp(name) }

// IsOSMetadata reports whether name is a file macOS writes for its own use beside
// the user's: "._<name>" (AppleDouble, a file's extended attributes on a volume
// without them, which NFSv3 is) and ".DS_Store" (Finder's view settings). A listing
// leaves them out (I-140). Both are ephemeral names: a delete unlinks them (docs/06 §1).
func IsOSMetadata(name string) bool { return meta.IsOSMetadata(name) }

func tempName(name string) string { return "." + name + uploadSuffix }

// uploads tracks the temps being written right now (a second upload of the same path is
// refused) and the slots.
type uploads struct {
	mu       sync.Mutex
	inflight map[string]bool
	slots    chan struct{}
}

func (u *uploads) acquire(temp string) (release func(), err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.inflight[temp] {
		return nil, errUploadInProgress
	}
	select {
	case u.slots <- struct{}{}:
	default:
		return nil, errUploadsBusy
	}
	u.inflight[temp] = true
	return func() {
		u.mu.Lock()
		delete(u.inflight, temp)
		<-u.slots
		u.mu.Unlock()
	}, nil
}

var (
	errUploadInProgress = errors.New("an upload of this file is in progress")
	errUploadsBusy      = errors.New("too many uploads in progress")
)

// partRange is one PUT's place in the file: [start, end) of total; whole means no
// Content-Range (the request carries the entire file).
type partRange struct {
	start, end, total uint64
	whole             bool
}

func (p partRange) final() bool { return p.end == p.total }

// parsePart reads Content-Length and Content-Range (docs/13 §3).
func parsePart(r *http.Request) (partRange, int, error) {
	if r.ContentLength < 0 {
		return partRange{}, http.StatusLengthRequired, errors.New("Content-Length is required")
	}
	n := uint64(r.ContentLength)
	cr := r.Header.Get("Content-Range")
	if cr == "" {
		return partRange{end: n, total: n, whole: true}, 0, nil
	}
	var a, b, total uint64
	rest, ok := strings.CutPrefix(cr, "bytes ")
	if !ok {
		return partRange{}, http.StatusBadRequest, fmt.Errorf("Content-Range %q: not a byte range", cr)
	}
	span, tot, ok := strings.Cut(rest, "/")
	if !ok {
		return partRange{}, http.StatusBadRequest, fmt.Errorf("Content-Range %q: no total", cr)
	}
	as, bs, ok := strings.Cut(span, "-")
	if !ok {
		return partRange{}, http.StatusBadRequest, fmt.Errorf("Content-Range %q: no range", cr)
	}
	var err error
	if a, err = strconv.ParseUint(as, 10, 64); err != nil {
		return partRange{}, http.StatusBadRequest, fmt.Errorf("Content-Range %q: bad start", cr)
	}
	if b, err = strconv.ParseUint(bs, 10, 64); err != nil {
		return partRange{}, http.StatusBadRequest, fmt.Errorf("Content-Range %q: bad end", cr)
	}
	if total, err = strconv.ParseUint(tot, 10, 64); err != nil {
		return partRange{}, http.StatusBadRequest, fmt.Errorf("Content-Range %q: the total must be a number", cr)
	}
	if b < a || b >= total {
		return partRange{}, http.StatusBadRequest, fmt.Errorf("Content-Range %q: outside the total", cr)
	}
	if n != b-a+1 {
		return partRange{}, http.StatusBadRequest, fmt.Errorf("Content-Length %d does not match Content-Range %q", n, cr)
	}
	return partRange{start: a, end: b + 1, total: total}, 0, nil
}

// put stores a whole file or one part of it (docs/13 §4): the bytes go through the vfs,
// the NFS write path (overlay, fsynced WAL, flusher), one WAL record per chunk-sized
// read (I-70), into the temp beside the target; the last byte renames it over the target.
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
	case !existed && !errors.Is(err, meta.ErrNotExist):
		s.fail(w, err)
		return
	}
	part, code, err := parsePart(r)
	if err != nil {
		early(w)
		writeErr(w, code, "put "+canonical+": "+err.Error())
		return
	}
	noOverwrite := r.Header.Get("If-None-Match") == "*"
	if existed && noOverwrite && (part.start == 0 || part.final()) {
		early(w)
		writeErr(w, http.StatusPreconditionFailed, "put "+canonical+": exists")
		return
	}
	temp := path.Join(path.Dir(canonical), tempName(parts[len(parts)-1]))
	var cur uint64 // the temp's current size: where the next part must start
	if fi, err := s.o.FS.Stat(temp); err == nil {
		cur = uint64(fi.Size())
	} else if !errors.Is(err, os.ErrNotExist) {
		s.fail(w, err)
		return
	}
	if !part.whole && part.start != cur {
		early(w)
		writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("put %s: the upload is at offset %d, not %d", canonical, cur, part.start), "offset": cur})
		return
	}
	if part.start == 0 && !s.fits(part.total) {
		early(w)
		writeErr(w, http.StatusInsufficientStorage, fmt.Sprintf("put %s: %d bytes do not fit in the volume's free space", canonical, part.total))
		return
	}
	release, err := s.up.acquire(temp)
	if err != nil {
		if errors.Is(err, errUploadsBusy) {
			w.Header().Set("Retry-After", "2")
			early(w)
			writeErr(w, http.StatusServiceUnavailable, "put "+canonical+": "+err.Error())
			return
		}
		early(w)
		writeErr(w, http.StatusConflict, "put "+canonical+": "+err.Error())
		return
	}
	defer release()
	flags := os.O_WRONLY | os.O_CREATE
	if part.whole {
		flags |= os.O_TRUNC
	}
	f, err := s.o.FS.OpenFile(temp, flags, 0o644)
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, err := f.Seek(int64(part.start), io.SeekStart); err != nil {
		_ = f.Close()
		s.fail(w, err)
		return
	}
	// A failure leaves no half part behind: a whole-file upload loses its temp, a part is
	// rolled back to its start so the next attempt resumes there (I-81).
	rollback := func() {
		if part.whole {
			_ = f.Close()
			_ = s.o.FS.Remove(temp)
			return
		}
		_ = f.Truncate(int64(part.start))
		_ = f.Close()
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, s.o.Store.Volume().ChunkSize)
	for left := part.end - part.start; left > 0; {
		_ = rc.SetReadDeadline(time.Now().Add(s.o.BodyTimeout)) // a silent client is dropped (I-82)
		n, rerr := io.ReadFull(r.Body, buf[:min(uint64(len(buf)), left)])
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				rollback()
				s.fail(w, err)
				return
			}
			left -= uint64(n)
		}
		if rerr != nil {
			rollback()
			code := http.StatusBadRequest
			var ne interface{ Timeout() bool }
			if errors.As(rerr, &ne) && ne.Timeout() {
				code = http.StatusRequestTimeout
			}
			writeErr(w, code, fmt.Sprintf("put %s: the body ended after %d of %d bytes: %v", canonical, part.end-part.start-left, part.end-part.start, rerr))
			return
		}
	}
	if err := f.Close(); err != nil {
		s.fail(w, err)
		return
	}
	if !part.final() {
		writeJSON(w, http.StatusAccepted, map[string]any{"offset": part.end})
		return
	}
	if existed && noOverwrite { // raced into existence since part 0
		writeErr(w, http.StatusPreconditionFailed, "put "+canonical+": exists")
		return
	}
	if err := s.o.FS.Rename(temp, canonical); err != nil {
		s.fail(w, err)
		return
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// early prepares a refusal sent before the body has been read: without the read deadline
// the server would first drain the announced body (up to its limit) and the client would
// see the reason only after sending it; with it the reply goes out at once and the
// connection is dropped, which a browser reports as the status (docs/13 §4, I-82).
func early(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	w.Header().Set("Connection", "close")
}

// fits reports whether total bytes of file data have room in the volume at the current
// policy, from the cached status report; without a report it says yes.
func (s *Server) fits(total uint64) bool {
	rep, ok := s.report().(admin.Report)
	if !ok || rep.Volume.K == 0 {
		return true
	}
	return needBytes(total, rep.Volume.K, rep.Volume.M) <= rep.Volume.Free
}

// needBytes is the raw space total bytes of file data take at policy k+m. Each operand
// is widened first: k+m is 256 at most, which overflows a uint8 to 0.
func needBytes(total uint64, k, m uint8) uint64 {
	if k == 0 {
		return total
	}
	return total * (uint64(k) + uint64(m)) / uint64(k)
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
