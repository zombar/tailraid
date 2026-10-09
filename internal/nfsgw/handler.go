// Package nfsgw serves the volume over NFSv3 on a loopback listener (docs/07).
package nfsgw

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	iofs "io/fs"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/vfs"
)

// Handler implements nfs.Handler over a vfs.FS. Handles are 16-byte (magic, gen, ino)
// values encoded by meta.EncodeHandle, so they survive restarts, renames and other
// gateways (I-16) and can never resolve to a different file (I-17).
type Handler struct {
	dirs dirCache // listings reused across the pages of one READDIR(PLUS)
	fs   *vfs.FS
	st   *meta.Store
}

// New wires a handler.
func New(fs *vfs.FS, st *meta.Store) *Handler { return &Handler{fs: fs, st: st} }

var _ nfs.Handler = (*Handler)(nil)

// Mount exports a single root for any dirpath; the loopback listener is the security
// boundary (docs/08).
func (h *Handler) Mount(_ context.Context, _ net.Conn, _ nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	return nfs.MountStatusOk, h.fs, []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

// Change is never nil: chmod, chown and utimes are supported.
// Change returns the attribute hooks. go-nfs passes a change hook's error through to the
// wire unmapped, which the client sees as an RPC-level GARBAGE_ARGS rather than an NFS3
// status; the adapter maps it to the status the other mutations get (docs/07).
func (h *Handler) Change(billy.Filesystem) billy.Change { return changeStatus{h.fs} }

type changeStatus struct{ billy.Change }

func (c changeStatus) Chmod(name string, mode os.FileMode) error {
	return attrStatus(c.Change.Chmod(name, mode))
}

func (c changeStatus) Lchown(name string, uid, gid int) error {
	return attrStatus(c.Change.Lchown(name, uid, gid))
}

func (c changeStatus) Chown(name string, uid, gid int) error {
	return attrStatus(c.Change.Chown(name, uid, gid))
}

func (c changeStatus) Chtimes(name string, atime, mtime time.Time) error {
	return attrStatus(c.Change.Chtimes(name, atime, mtime))
}

// attrStatus maps a SETATTR error to an NFS3 status: ENOENT → NOENT, EROFS/EACCES → ACCES
// (what CREATE and MKDIR get on a stale gateway), anything else → IO.
func attrStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusNoEnt, WrappedErr: err}
	case errors.Is(err, syscall.EROFS), errors.Is(err, os.ErrPermission):
		return &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusAccess, WrappedErr: err}
	}
	return &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusIO, WrappedErr: err}
}

// FSStat reports capacity through meta.Usage (the same formula as `status`).
func (h *Handler) FSStat(_ context.Context, _ billy.Filesystem, s *nfs.FSStat) error {
	st := h.st.Stats()
	total, _, free := meta.Usage(h.st.Volume(), h.st.Nodes(), st)
	s.TotalSize, s.FreeSize, s.AvailableSize = total, free, free
	s.TotalFiles = uint64(st.Inodes)
	s.FreeFiles = uint64(max(int64(1<<32)-int64(st.Inodes), 0)) // never wraps
	s.AvailableFiles = s.FreeFiles
	return nil
}

// ToHandle encodes the inode behind path. Unknown paths yield an empty handle, which
// FromHandle rejects as stale.
func (h *Handler) ToHandle(_ billy.Filesystem, path []string) []byte {
	in, err := h.st.Resolve(path)
	if err != nil {
		return nil
	}
	return meta.EncodeHandle(in.Ino, in.Gen)
}

// FromHandle resolves a handle to the current path of its inode.
func (h *Handler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	ino, gen, ok := meta.DecodeHandle(fh)
	if !ok {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale, WrappedErr: errors.New("malformed handle")}
	}
	in, found := h.st.GetInode(ino)
	if !found || in.Gen != gen {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale, WrappedErr: fmt.Errorf("inode %d gen %d", ino, gen)}
	}
	path, err := h.st.PathOf(ino)
	if err != nil {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale, WrappedErr: err}
	}
	return h.fs, path, nil
}

// InvalidateHandle is a no-op: handles are derived from metadata, not cached.
func (h *Handler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

var _ nfs.CachingHandler = (*Handler)(nil)

// VerifierFor keys a directory listing by the directory inode's identity and its Version
// (which the FSM bumps on every change of the name table, docs/01) and keeps the listing,
// so the following pages of the same READDIR(PLUS) do not list and sort the directory
// again (docs/07).
func (h *Handler) VerifierFor(path string, contents []iofs.FileInfo) uint64 {
	in, err := h.st.Resolve(meta.SplitPath(path))
	if err != nil {
		return hashListing(path, contents)
	}
	key := dirKey{Ino: in.Ino, Gen: in.Gen, Version: in.Version}
	v := key.verifier(path)
	h.dirs.put(v, key, contents)
	return v
}

// DataForVerifier returns the kept listing while the directory is unchanged, else nil
// (go-nfs then lists again and answers NFS3ERR_BAD_COOKIE for the stale cookie, I-47).
func (h *Handler) DataForVerifier(path string, verifier uint64) []iofs.FileInfo {
	key, contents, ok := h.dirs.get(verifier)
	if !ok {
		return nil
	}
	in, err := h.st.Resolve(meta.SplitPath(path))
	if err != nil || in.Ino != key.Ino || in.Gen != key.Gen || in.Version != key.Version {
		return nil
	}
	return contents
}

// hashListing is the verifier for a path that no longer resolves (go-nfs's own scheme).
func hashListing(path string, contents []iofs.FileInfo) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(path))
	for _, fi := range contents {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(fi.Name()))
	}
	return h.Sum64()
}

type dirKey struct {
	Ino     meta.Ino
	Gen     uint32
	Version uint64
}

func (k dirKey) verifier(path string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(path))
	var b [20]byte
	binary.LittleEndian.PutUint64(b[:8], uint64(k.Ino))
	binary.LittleEndian.PutUint32(b[8:12], k.Gen)
	binary.LittleEndian.PutUint64(b[12:], k.Version)
	_, _ = h.Write(b[:])
	v := h.Sum64()
	if v == 0 {
		v = 1 // 0 means "no verifier" on the wire
	}
	return v
}

// dirCache holds the most recent listings (a client pages one or a few directories at a
// time; 64 is generous).
type dirCache struct {
	mu      sync.Mutex
	entries map[uint64]dirEntry
	order   []uint64 // insertion order for eviction
}

type dirEntry struct {
	key      dirKey
	contents []iofs.FileInfo
}

const dirCacheMax = 64

func (c *dirCache) put(v uint64, key dirKey, contents []iofs.FileInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[uint64]dirEntry{}
	}
	if _, ok := c.entries[v]; !ok {
		c.order = append(c.order, v)
		for len(c.order) > dirCacheMax {
			delete(c.entries, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.entries[v] = dirEntry{key, contents}
}

func (c *dirCache) get(v uint64) (dirKey, []iofs.FileInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[v]
	return e.key, e.contents, ok
}

// HandleLimit is nominal; nothing is cached per handle.
func (h *Handler) HandleLimit() int { return 1 << 20 }

// Listen binds the gateway listener. addr must be a loopback address (docs/08).
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("nfsgw: listen address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("nfsgw: refusing to listen on non-loopback address %q", addr)
	}
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

// Serve runs the NFS server on l until ctx is done, then closes the listener and every
// connection it accepted (I-100): go-nfs serves each connection on its own goroutine and
// would otherwise keep answering a mounted client after the gateway's flusher stopped.
func Serve(ctx context.Context, l net.Listener, h *Handler) error {
	tl := &trackedListener{Listener: l, conns: map[net.Conn]struct{}{}}
	errc := make(chan error, 1)
	go func() { errc <- nfs.Serve(tl, h) }()
	select {
	case <-ctx.Done():
		_ = tl.Close()
		tl.closeConns()
		<-errc
		return nil
	case err := <-errc:
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}
