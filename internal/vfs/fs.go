package vfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/helper/chroot"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

// FS is the billy.Filesystem the NFS and S3 gateways serve (docs/04, docs/07). Every
// metadata mutation is a Raft proposal followed by WaitApplied, so a gateway is
// linearizable with itself; reads come from the local FSM copy with the overlay on top.
type FS struct {
	st   *meta.Store
	prop meta.Proposer
	ov   *Overlay
	rd   *Reader
	clk  clock.Clock
	uid  uint32
	gid  uint32
}

// New wires a filesystem. uid/gid are stamped on created inodes.
func New(st *meta.Store, prop meta.Proposer, ov *Overlay, rd *Reader, clk clock.Clock, uid, gid uint32) *FS {
	return &FS{st: st, prop: prop, ov: ov, rd: rd, clk: clk, uid: uid, gid: gid}
}

var (
	_ billy.Filesystem = (*FS)(nil)
	_ billy.Change     = (*FS)(nil)
	_ billy.Capable    = (*FS)(nil)
)

// ErrReadOnly is returned for metadata mutations while the cluster has no leader
// (docs/02 Health Stale/NoLeader → NFS3ERR_ROFS).
var ErrReadOnly = syscall.EROFS

// split cleans a billy path into components relative to the root.
func split(name string) []string {
	name = path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	if name == "/" {
		return nil
	}
	return strings.Split(name[1:], "/")
}

func parentOf(parts []string) ([]string, string) {
	if len(parts) == 0 {
		return nil, ""
	}
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// mapErr converts meta/vfs errors to the os/syscall errors billy callers expect.
func mapErr(op, name string, err error) error {
	if err == nil {
		return nil
	}
	var e error
	switch {
	case errors.Is(err, meta.ErrNotExist):
		e = os.ErrNotExist
	case errors.Is(err, meta.ErrExist):
		e = os.ErrExist
	case errors.Is(err, meta.ErrNotDir):
		e = syscall.ENOTDIR
	case errors.Is(err, meta.ErrIsDir):
		e = syscall.EISDIR
	case errors.Is(err, meta.ErrNotEmpty):
		e = syscall.ENOTEMPTY
	case errors.Is(err, meta.ErrLoop):
		e = syscall.EINVAL
	case errors.Is(err, meta.ErrBadCommand):
		e = syscall.EINVAL
	case errors.Is(err, ErrIO):
		e = syscall.EIO
	case errors.Is(err, ErrWALFull):
		e = syscall.EAGAIN
	default:
		e = err
	}
	return &fs.PathError{Op: op, Path: name, Err: e}
}

// propose applies a metadata mutation and waits for it locally.
func (f *FS) propose(ctx context.Context, cmd meta.Command) (meta.Result, error) {
	res, index, err := f.prop.Propose(ctx, cmd)
	if err != nil {
		return meta.Result{}, ErrReadOnly
	}
	if res.Err != nil {
		return res, res.Err
	}
	if err := f.st.WaitApplied(ctx, index); err != nil {
		return res, err
	}
	return res, nil
}

func (f *FS) now() int64 { return f.clk.Now().UnixNano() }

func (f *FS) resolve(name string) (meta.Inode, error) {
	return f.st.Resolve(split(name))
}

// --- billy.Basic ---------------------------------------------------------------------

func (f *FS) Create(filename string) (billy.File, error) {
	return f.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

func (f *FS) Open(filename string) (billy.File, error) {
	return f.OpenFile(filename, os.O_RDONLY, 0)
}

func (f *FS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	ctx := context.Background()
	parts := split(filename)
	if len(parts) == 0 {
		return nil, mapErr("open", filename, meta.ErrIsDir)
	}
	in, err := f.resolve(filename)
	switch {
	case err == nil:
		if flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
			return nil, mapErr("open", filename, meta.ErrExist)
		}
		if in.Type == meta.TDir {
			return nil, mapErr("open", filename, meta.ErrIsDir)
		}
	case errors.Is(err, meta.ErrNotExist) && flag&os.O_CREATE != 0:
		dirParts, base := parentOf(parts)
		if err := f.mkdirAll(ctx, dirParts, 0o755); err != nil {
			return nil, mapErr("open", filename, err)
		}
		parent, err := f.st.Resolve(dirParts)
		if err != nil {
			return nil, mapErr("open", filename, err)
		}
		res, err := f.propose(ctx, meta.Command{Op: meta.OpCreate, Create: &meta.Create{
			Parent: parent.Ino, Name: base, Type: meta.TFile, Mode: uint32(perm.Perm()), Uid: f.uid, Gid: f.gid, Now: f.now()}})
		if err != nil {
			if errors.Is(err, meta.ErrExist) { // lost a create race; open what is there
				in, err = f.resolve(filename)
				if err != nil {
					return nil, mapErr("open", filename, err)
				}
				break
			}
			return nil, mapErr("open", filename, err)
		}
		in, _ = f.st.GetInode(res.Ino)
	default:
		return nil, mapErr("open", filename, err)
	}
	file := &File{fs: f, ino: in.Ino, name: strings.Join(parts, "/"), flag: flag} // billy names are root-relative
	if flag&os.O_TRUNC != 0 && flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		if err := f.ov.Truncate(in.Ino, in.Size, 0); err != nil {
			return nil, mapErr("open", filename, err)
		}
	}
	if flag&os.O_APPEND != 0 {
		file.pos = int64(file.size())
	}
	return file, nil
}

func (f *FS) Stat(filename string) (os.FileInfo, error) {
	in, err := f.resolve(filename)
	if err != nil {
		return nil, mapErr("stat", filename, err)
	}
	return f.info(in), nil
}

func (f *FS) Lstat(filename string) (os.FileInfo, error) { return f.Stat(filename) }

func (f *FS) info(in meta.Inode) *fileInfo {
	fi := &fileInfo{in: in}
	if in.Type == meta.TFile {
		if sz, ok := f.ov.Size(in.Ino); ok {
			fi.size = sz
			fi.dirty = true
		} else {
			fi.size = in.Size
		}
	} else {
		fi.size = in.Size
	}
	return fi
}

func (f *FS) Rename(oldpath, newpath string) error {
	ctx := context.Background()
	op, on := parentOf(split(oldpath))
	np, nn := parentOf(split(newpath))
	if on == "" || nn == "" {
		return mapErr("rename", oldpath, meta.ErrBadCommand)
	}
	oldDir, err := f.st.Resolve(op)
	if err != nil {
		return mapErr("rename", oldpath, err)
	}
	if err := f.mkdirAll(ctx, np, 0o755); err != nil {
		return mapErr("rename", newpath, err)
	}
	newDir, err := f.st.Resolve(np)
	if err != nil {
		return mapErr("rename", newpath, err)
	}
	var replaced meta.Ino
	if dst, ok := f.st.Lookup(newDir.Ino, nn); ok && dst.Type != meta.TDir {
		replaced = dst.Ino
	}
	if _, err := f.propose(ctx, meta.Command{Op: meta.OpRename, Rename: &meta.Rename{
		OldParent: oldDir.Ino, OldName: on, NewParent: newDir.Ino, NewName: nn, Now: f.now()}}); err != nil {
		return mapErr("rename", oldpath, err)
	}
	if replaced != 0 {
		f.ov.Drop(replaced)
	}
	return nil
}

func (f *FS) Remove(filename string) error {
	ctx := context.Background()
	dp, name := parentOf(split(filename))
	if name == "" {
		return mapErr("remove", filename, meta.ErrBadCommand)
	}
	dir, err := f.st.Resolve(dp)
	if err != nil {
		return mapErr("remove", filename, err)
	}
	in, ok := f.st.Lookup(dir.Ino, name)
	if !ok {
		return mapErr("remove", filename, meta.ErrNotExist)
	}
	var cmd meta.Command
	if in.Type == meta.TDir {
		cmd = meta.Command{Op: meta.OpRmdir, Rmdir: &meta.Rmdir{Parent: dir.Ino, Name: name, Now: f.now()}}
	} else {
		cmd = meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: dir.Ino, Name: name, Now: f.now()}}
	}
	if _, err := f.propose(ctx, cmd); err != nil {
		return mapErr("remove", filename, err)
	}
	f.ov.Drop(in.Ino)
	return nil
}

func (f *FS) Join(elem ...string) string { return path.Join(elem...) }

// --- billy.TempFile ------------------------------------------------------------------

func (f *FS) TempFile(dir, prefix string) (billy.File, error) {
	var b [8]byte
	for i := 0; i < 100; i++ {
		_, _ = rand.Read(b[:])
		name := f.Join(dir, prefix+hex.EncodeToString(b[:]))
		file, err := f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return nil, mapErr("tempfile", dir, meta.ErrExist)
}

// --- billy.Dir -----------------------------------------------------------------------

func (f *FS) ReadDir(p string) ([]os.FileInfo, error) {
	in, err := f.resolve(p)
	if err != nil {
		return nil, mapErr("readdir", p, err)
	}
	ents, err := f.st.ReadDir(in.Ino)
	if err != nil {
		return nil, mapErr("readdir", p, err)
	}
	out := make([]os.FileInfo, len(ents))
	for i := range ents {
		out[i] = f.info(ents[i])
	}
	return out, nil
}

func (f *FS) MkdirAll(filename string, perm os.FileMode) error {
	if err := f.mkdirAll(context.Background(), split(filename), perm); err != nil {
		return mapErr("mkdir", filename, err)
	}
	return nil
}

func (f *FS) mkdirAll(ctx context.Context, parts []string, perm os.FileMode) error {
	cur := meta.RootIno
	for _, name := range parts {
		child, ok := f.st.Lookup(cur, name)
		if ok {
			if child.Type != meta.TDir {
				return meta.ErrNotDir
			}
			cur = child.Ino
			continue
		}
		res, err := f.propose(ctx, meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{
			Parent: cur, Name: name, Mode: uint32(perm.Perm()), Uid: f.uid, Gid: f.gid, Now: f.now()}})
		if err != nil {
			if errors.Is(err, meta.ErrExist) {
				if child, ok := f.st.Lookup(cur, name); ok && child.Type == meta.TDir {
					cur = child.Ino
					continue
				}
				return meta.ErrNotDir
			}
			return err
		}
		cur = res.Ino
	}
	return nil
}

// --- billy.Symlink -------------------------------------------------------------------

func (f *FS) Symlink(target, link string) error {
	ctx := context.Background()
	dp, name := parentOf(split(link))
	if name == "" {
		return mapErr("symlink", link, meta.ErrBadCommand)
	}
	if err := f.mkdirAll(ctx, dp, 0o755); err != nil {
		return mapErr("symlink", link, err)
	}
	dir, err := f.st.Resolve(dp)
	if err != nil {
		return mapErr("symlink", link, err)
	}
	if _, err := f.propose(ctx, meta.Command{Op: meta.OpCreate, Create: &meta.Create{
		Parent: dir.Ino, Name: name, Type: meta.TSymlink, Mode: 0o777, Uid: f.uid, Gid: f.gid, Target: target, Now: f.now()}}); err != nil {
		return mapErr("symlink", link, err)
	}
	return nil
}

func (f *FS) Readlink(link string) (string, error) {
	in, err := f.resolve(link)
	if err != nil {
		return "", mapErr("readlink", link, err)
	}
	if in.Type != meta.TSymlink {
		return "", mapErr("readlink", link, syscall.EINVAL)
	}
	return in.Target, nil
}

// --- billy.Chroot --------------------------------------------------------------------

func (f *FS) Chroot(p string) (billy.Filesystem, error) { return chroot.New(f, p), nil }

func (f *FS) Root() string { return "/" }

// --- billy.Capable -------------------------------------------------------------------

func (f *FS) Capabilities() billy.Capability {
	return billy.AllCapabilities &^ billy.LockCapability
}

// --- billy.Change --------------------------------------------------------------------

func (f *FS) setAttr(name string, sa meta.SetAttr) error {
	in, err := f.resolve(name)
	if err != nil {
		return mapErr("setattr", name, err)
	}
	sa.Ino, sa.Now = in.Ino, f.now()
	if _, err := f.propose(context.Background(), meta.Command{Op: meta.OpSetAttr, SetAttr: &sa}); err != nil {
		return mapErr("setattr", name, err)
	}
	return nil
}

func (f *FS) Chmod(name string, mode os.FileMode) error {
	return f.setAttr(name, meta.SetAttr{Mask: meta.AttrMode, Mode: uint32(mode.Perm())})
}

func (f *FS) Lchown(name string, uid, gid int) error {
	return f.setAttr(name, meta.SetAttr{Mask: meta.AttrUid | meta.AttrGid, Uid: uint32(uid), Gid: uint32(gid)})
}

func (f *FS) Chown(name string, uid, gid int) error { return f.Lchown(name, uid, gid) }

func (f *FS) Chtimes(name string, atime, mtime time.Time) error {
	return f.setAttr(name, meta.SetAttr{Mask: meta.AttrAtime | meta.AttrMtime, Atime: atime.UnixNano(), Mtime: mtime.UnixNano()})
}
