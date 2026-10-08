// Package nfsgw serves the volume over NFSv3 on a loopback listener (docs/07).
package nfsgw

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
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
	fs *vfs.FS
	st *meta.Store
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
	s.FreeFiles = (1 << 32) - uint64(st.Inodes)
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

// HandleLimit is nominal; nothing is cached per handle.
func (h *Handler) HandleLimit() int { return 1 << 20 }

// Listen binds the gateway listener. addr must be a loopback address (docs/08).
func Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("nfsgw: listen address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("nfsgw: refusing to listen on non-loopback address %q", addr)
	}
	return net.Listen("tcp", addr)
}

// Serve runs the NFS server on l until ctx is done.
func Serve(ctx context.Context, l net.Listener, h *Handler) error {
	errc := make(chan error, 1)
	go func() { errc <- nfs.Serve(l, h) }()
	select {
	case <-ctx.Done():
		_ = l.Close()
		<-errc
		return nil
	case err := <-errc:
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}
