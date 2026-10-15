package vfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/helper/chroot"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
)

// FS is the billy.Filesystem the NFS and S3 gateways serve (docs/04, docs/07). Every
// metadata mutation is a Raft proposal followed by WaitApplied, so a gateway is
// linearizable with itself; reads come from the local FSM copy with the overlay on top.
type FS struct {
	*fsCore
	// actor is the user every proposal and read of this view acts for (docs/21 §5,
	// I-353): a view from As. The base filesystem New returns has none and is the
	// node's own writer (System), which reaches the reserved tree only once users exist.
	actor  meta.UserIdx
	system bool
}

// fsCore is what every view of one filesystem shares.
type fsCore struct {
	st    *meta.Store
	prop  meta.Proposer
	ov    *Overlay
	rd    *Reader
	fl    *Flusher // for Flush; nil until SetFlusher
	clk   clock.Clock
	uid   uint32
	gid   uint32
	stale func() bool // no leader contact for the Stale threshold (docs/02 §Health); nil = always
	snaps snapState   // /.snapshots: the mirrors and the gateway's caches (docs/16 §6)

	// every byte a client read or wrote through a File (docs/17 §1: bytes_r, bytes_w)
	bytesRead, bytesWritten atomic.Uint64

	// MinCodec, when set, is the oldest codec a live active member reads (the prober's
	// view): a codec-9 field (Trash.Expect, Rename.NoReplace) is sent only once it is
	// 9, since an older leader refuses the entry as malformed (docs/02 §Codec versions).
	MinCodec func() uint8

	// Refused, when set, hears every operation a view refused for want of a role (the
	// audit log, docs/21 §8): the actor, the operation and the path.
	Refused func(actor meta.UserIdx, op, path string)
}

// refuse is EACCES, told to Refused.
func (f *FS) refuse(op, name string) error {
	if f.Refused != nil {
		f.Refused(f.actor, op, name)
	}
	return syscall.EACCES
}

// codecAtLeast reports whether every live member reads codec v (no view: yes).
func (f *FS) codecAtLeast(v uint8) bool { return f.MinCodec == nil || f.MinCodec() >= v }

// New wires a filesystem. uid/gid are stamped on created inodes. The filesystem New
// returns is the node's own (System) writer; a gateway serves a person through As.
func New(st *meta.Store, prop meta.Proposer, ov *Overlay, rd *Reader, clk clock.Clock, uid, gid uint32) *FS {
	return &FS{fsCore: &fsCore{
		st: st, prop: prop, ov: ov, rd: rd, clk: clk, uid: uid, gid: gid,
		snaps: snapState{trees: map[meta.Hash]mirror.Tree{}, index: map[meta.SnapID]map[snapKey][]string{}},
	}, system: true}
}

// As is the view of the filesystem for one user (docs/21 §5, I-353): every proposal
// carries the actor, every open, stat and listing is checked against the actor's role
// on the local replica (once the cluster has users), listings of a folder the actor
// holds no role on show only what they may reach, and the mode bits a client sees
// follow the role. Actor 0 is nobody: everything is refused once users exist.
func (f *FS) As(actor meta.UserIdx) *FS { return &FS{fsCore: f.fsCore, actor: actor} }

// Actor is the user this view acts for (0 for the node's own writer and for nobody).
func (f *FS) Actor() meta.UserIdx { return f.actor }

// System reports whether this is the node's own writer.
func (f *FS) System() bool { return f.system }

// enforced reports whether this view checks roles: the cluster has users and the view
// is a person's (the node's own writer is checked by the FSM alone, I-349).
func (f *FS) enforced() bool { return !f.system && f.st.Enforcing() }

// role is the actor's effective role at ino.
func (f *FS) role(ino meta.Ino) meta.Role { return f.st.Access(f.actor, ino) }

// checkRead refuses a read of ino's contents without Read (I-353).
func (f *FS) checkRead(ino meta.Ino, name string) error {
	if f.enforced() && f.role(ino) < meta.RoleRead {
		return f.refuse("read", name)
	}
	return nil
}

// checkStat refuses a look-up of ino without Read, unless it is on the way to something
// the actor holds a role on (I-348).
func (f *FS) checkStat(ino meta.Ino, name string) error {
	if ino == meta.RootIno {
		return nil // the root is every mount's entry: stat-able by anyone, listed by role (I-350)
	}
	if f.enforced() && f.role(ino) < meta.RoleRead && !f.st.Traverses(f.actor, ino) {
		return f.refuse("stat", name)
	}
	return nil
}

// checkWrite refuses a write to ino's contents without Write (I-353); the FSM decides
// every metadata change itself (I-349).
func (f *FS) checkWrite(ino meta.Ino, name string) error {
	if f.enforced() && f.role(ino) < meta.RoleWrite {
		return f.refuse("write", name)
	}
	return nil
}

// checkAdmin refuses an operation on ino without Admin: a revert or a restore from a
// snapshot into the live tree (docs/21 §3, I-348).
func (f *FS) checkAdmin(ino meta.Ino, name string) error {
	if f.enforced() && f.role(ino) < meta.RoleAdmin {
		return f.refuse("admin", name)
	}
	return nil
}

// SetHealth gives the filesystem the gateway's health view: while stale reports false a
// failed proposal (no leader, leader lost) is retried, because the leader is usually back
// within seconds; once stale (no leader contact for 30 s, docs/02) the mutation fails
// with EROFS (L-187). Without a view every failure is EROFS at once.
func (f *FS) SetHealth(stale func() bool) { f.stale = stale }

// SetFlusher gives the filesystem the flusher Flush waits on.
func (f *FS) SetFlusher(fl *Flusher) { f.fl = fl }

// ClientBytes is how many bytes clients have read and written through this gateway's
// files since it started (cumulative; docs/17 §1).
func (f *FS) ClientBytes() (read, written uint64) { return f.bytesRead.Load(), f.bytesWritten.Load() }

// flushPoll is how often Flush looks again while a flush another path started is still
// outstanding on the inode.
const flushPoll = 50 * time.Millisecond

// Flush commits a file's dirty bytes and returns once the overlay holds nothing for it:
// a Commit has landed for every byte, so another gateway reads them (docs/17 §2, I-192).
// A flush the idle timer started first is waited for rather than doubled.
func (f *FS) Flush(ctx context.Context, name string) error {
	if f.fl == nil {
		return errors.New("flush: no flusher")
	}
	in, err := f.st.Resolve(meta.SplitPath(name))
	if err != nil {
		return mapErr("flush", name, err)
	}
	for {
		if err := f.fl.FlushInode(ctx, in.Ino); err != nil {
			return mapErr("flush", name, err)
		}
		cur, ok := f.st.GetInode(in.Ino)
		if !ok {
			return mapErr("flush", name, meta.ErrNotExist)
		}
		if _, dirty := f.ov.Size(in.Ino, cur.Size); !dirty {
			return nil
		}
		select {
		case <-ctx.Done():
			return mapErr("flush", name, ctx.Err())
		case <-f.clk.After(flushPoll):
		}
	}
}

// Proposal retry backoff while the gateway is not yet stale.
const (
	proposeRetryMin = 200 * time.Millisecond
	proposeRetryMax = 2 * time.Second
)

var (
	_ billy.Filesystem = (*FS)(nil)
	_ billy.Change     = (*FS)(nil)
	_ billy.Capable    = (*FS)(nil)
)

// ErrReadOnly is returned for metadata mutations while the cluster has no leader
// (docs/02 Health Stale/NoLeader → NFS3ERR_ROFS).
var ErrReadOnly = syscall.EROFS

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
	case errors.Is(err, meta.ErrNameTooLong):
		e = syscall.ENAMETOOLONG
	case errors.Is(err, meta.ErrProtected):
		e = syscall.EPERM // os.ErrPermission on the wire: NFS3ERR_ACCES, STATUS_ACCESS_DENIED (I-154)
	case errors.Is(err, meta.ErrAccess):
		e = syscall.EACCES // the actor's role does not cover it (docs/21 §4, I-349)
	case errors.Is(err, meta.ErrInTrash):
		e = os.ErrNotExist // the share never sees the trash (I-155)
	case errors.Is(err, meta.ErrCodec):
		e = syscall.EOPNOTSUPP // a member too old for the command (docs/12 §6)
	case errors.Is(err, meta.ErrBadCommand):
		e = syscall.EINVAL
	case errors.Is(err, ErrIO):
		e = syscall.EIO
	case errors.Is(err, chunk.ErrFull):
		// the cluster has no room for the shards: ENOSPC, "No space left on device",
		// rather than an I/O error (I-79, L-189); the sentinels stay reachable
		e = fmt.Errorf("%w: %w", syscall.ENOSPC, err)
	case errors.Is(err, ErrWALFull), errors.Is(err, ErrDirtyFull):
		// NFS3ERR_IO after the cap wait expired or behind a failed flush (docs/04 §3,
		// I-79). The sentinels stay reachable for callers that tell the two apart.
		e = fmt.Errorf("%w: %w", syscall.EIO, err)
	case errors.Is(err, ErrFileTooLarge):
		e = syscall.EFBIG
	default:
		e = err
	}
	return &fs.PathError{Op: op, Path: name, Err: e}
}

// propose applies a metadata mutation and waits for it locally. A proposal that was not
// applied (no leader, leader changed) is retried with backoff until the gateway is stale.
// A proposal whose outcome is unknown (docs/02 §forwarding, I-95) is proposed once more,
// and the second answer is read in that light: an ErrExist or ErrNotExist after an
// unknown outcome carries meta.ErrUnknownOutcome too, so Create and Mkdir open what is
// there, and Remove and Rename take "already gone" as their own first attempt having
// landed. A second unknown outcome in a row is an error (EIO on the wire).
func (f *FS) propose(ctx context.Context, cmd meta.Command) (meta.Result, error) {
	delay := proposeRetryMin
	afterUnknown := false
	if f.st.Enforcing() && f.codecAtLeast(meta.CodecUsers) {
		// the actor on every proposal once the cluster has users (docs/21 §4, §5): before
		// the first user the wire carries what it did, and a 13-reader refuses nothing
		cmd.Actor, cmd.System = f.actor, f.system
	}
	for {
		res, index, err := f.prop.Propose(ctx, cmd)
		if err == nil {
			if res.Err != nil {
				if index > 0 {
					// a refusal is decided at an index too: once it is applied here, the
					// state that refused (the other gateway's Create, I-222) resolves
					if werr := f.st.WaitApplied(ctx, index); werr != nil {
						return res, werr
					}
				}
				if afterUnknown && (errors.Is(res.Err, meta.ErrExist) || errors.Is(res.Err, meta.ErrNotExist)) {
					return res, fmt.Errorf("%w (%w)", res.Err, meta.ErrUnknownOutcome)
				}
				if errors.Is(res.Err, meta.ErrAccess) && f.Refused != nil {
					f.Refused(f.actor, fmt.Sprintf("op %d", cmd.Op), "")
				}
				return res, res.Err
			}
			if err := f.st.WaitApplied(ctx, index); err != nil {
				return res, err
			}
			return res, nil
		}
		if errors.Is(err, meta.ErrUnknownOutcome) {
			if !afterUnknown {
				afterUnknown = true // the repeat goes at once; its answer is read knowing the first may have landed
				continue
			}
			// unknown again (a leader killed mid-failover leaves a forward in flight and
			// the repeat aimed at its address): proposed again with backoff, every answer
			// read the same way, until the gateway is stale (I-95)
			if f.stale == nil || f.stale() {
				return meta.Result{}, fmt.Errorf("%w: %w", syscall.EIO, err)
			}
			select {
			case <-ctx.Done():
				return meta.Result{}, fmt.Errorf("%w: %w", syscall.EIO, err)
			case <-f.clk.After(delay):
			}
			delay = min(delay*2, proposeRetryMax)
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return meta.Result{}, err
		}
		if f.stale == nil || f.stale() {
			return meta.Result{}, ErrReadOnly
		}
		select {
		case <-ctx.Done():
			return meta.Result{}, ErrReadOnly
		case <-f.clk.After(delay):
		}
		delay = min(delay*2, proposeRetryMax)
	}
}

// landedEarlier reports whether err says the mutation is already in effect: the entry
// is gone (or exists) and the previous attempt's outcome was unknown.
func landedEarlier(err, want error) bool {
	return errors.Is(err, want) && errors.Is(err, meta.ErrUnknownOutcome)
}

func (f *FS) now() int64 { return f.clk.Now().UnixNano() }

// ownCreate reports whether in, found under a name whose exclusive Create answered
// ErrExist after an unknown outcome, is the file that first attempt made: an empty file
// created between asked and now (the FSM never dates a create before its command, and
// clamps it forward only to its own last time). Two clients can both be told "created"
// only inside that window, an unknown outcome racing another create of the name (I-222).
func ownCreate(in meta.Inode, asked, now int64) bool {
	return in.Type == meta.TFile && in.Size == 0 && in.Ctime >= asked && in.Ctime <= now
}

func (f *FS) resolve(name string) (meta.Inode, error) {
	return f.st.Resolve(meta.SplitPath(name))
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
	parts := meta.SplitPath(filename)
	if len(parts) == 0 {
		return nil, mapErr("open", filename, meta.ErrIsDir)
	}
	if isSnapshotPath(parts) {
		return f.snapOpen(filename, parts, flag)
	}
	in, err := f.resolve(filename)
	switch {
	case err == nil:
		// Read first: what the entry is (a directory, a link, exists at all for an
		// O_EXCL) is told to those who may read it, and nothing to anyone else (I-353)
		if aerr := f.checkRead(in.Ino, filename); aerr != nil {
			return nil, mapErr("open", filename, aerr)
		}
		if flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
			return nil, mapErr("open", filename, meta.ErrExist)
		}
		if in.Type == meta.TDir {
			return nil, mapErr("open", filename, meta.ErrIsDir)
		}
		if in.Type != meta.TFile {
			// a symlink has no data of its own; NFS clients READLINK and open the target,
			// so a handle here would only swallow writes (flush ignores non-files)
			return nil, mapErr("open", filename, syscall.ELOOP)
		}
		if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
			if aerr := f.checkWrite(in.Ino, filename); aerr != nil {
				return nil, mapErr("open", filename, aerr)
			}
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
		asked := f.now()
		res, err := f.propose(ctx, meta.Command{Op: meta.OpCreate, Create: &meta.Create{
			Parent: parent.Ino, Name: base, Type: meta.TFile, Mode: uint32(perm.Perm()), Uid: f.uid, Gid: f.gid, Now: asked,
		}})
		if err != nil {
			if !errors.Is(err, meta.ErrExist) {
				return nil, mapErr("open", filename, err)
			}
			// lost a create race, or our own first attempt landed (I-95); the answer was
			// applied here before propose returned, so the name resolves to the winner
			unknown := errors.Is(err, meta.ErrUnknownOutcome)
			in, err = f.resolve(filename)
			if err != nil {
				return nil, mapErr("open", filename, err)
			}
			if flag&os.O_EXCL != 0 && (!unknown || !ownCreate(in, asked, f.now())) {
				return nil, mapErr("open", filename, meta.ErrExist) // another client's file (I-222)
			}
			break
		}
		in, _ = f.st.GetInode(res.Ino)
	default:
		return nil, mapErr("open", filename, err)
	}
	file := &File{fs: f, ino: in.Ino, name: filepath.Join(parts...), flag: flag} // billy names are root-relative
	if flag&os.O_TRUNC != 0 && flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		// nothing to cut on an empty, clean file: no WAL record, no commit (I-1)
		if _, dirty := f.ov.Size(in.Ino, in.Size); dirty || in.Size != 0 {
			if err := f.ov.TruncateAs(in.Ino, in.Size, 0, f.actor); err != nil {
				return nil, mapErr("open", filename, err)
			}
		}
		// a writer that opens the file truncated replaces it: its flushes are one
		// rewrite, so two such copies at once leave one whole (docs/04 §6, I-223)
		f.ov.MarkRewrite(in.Ino)
	}
	if flag&os.O_APPEND != 0 {
		file.pos = int64(file.size())
	}
	return file, nil
}

func (f *FS) Stat(filename string) (os.FileInfo, error) {
	if parts := meta.SplitPath(filename); isSnapshotPath(parts) {
		return f.snapStat(filename, parts)
	}
	in, err := f.resolve(filename)
	if err != nil {
		return nil, mapErr("stat", filename, err)
	}
	if err := f.checkStat(in.Ino, filename); err != nil {
		return nil, mapErr("stat", filename, err)
	}
	return f.info(in), nil
}

func (f *FS) Lstat(filename string) (os.FileInfo, error) { return f.Stat(filename) }

func (f *FS) info(in meta.Inode) *fileInfo {
	fi := &fileInfo{in: in}
	if f.enforced() {
		// the mode bits a client sees follow the role (docs/21 §5, I-351): read-only
		// below Write, owner-only always; the stored mode is kept, never consulted
		mask := uint32(0o500)
		if f.role(in.Ino) >= meta.RoleWrite {
			mask = 0o700
		}
		fi.in.Mode = in.Mode&^0o777 | in.Mode&mask
	}
	if in.Type == meta.TFile {
		if sz, ok := f.ov.Size(in.Ino, in.Size); ok {
			fi.size = sz
			fi.dirty = true
			// the times of its last write here, as its size: the flush stamps its own
			// later, and until then a client must see the file changed (I-261)
			if wrote, user, hasUser, ok := f.ov.Changed(in.Ino); ok {
				w := wrote.UnixNano()
				fi.in.Ctime = max(in.Ctime, w)
				if hasUser {
					fi.in.Mtime = user // a client's own time after its last write (I-230)
				} else {
					fi.in.Mtime = max(in.Mtime, w)
				}
			}
		} else {
			fi.size = in.Size
		}
	} else {
		fi.size = in.Size
	}
	return fi
}

func (f *FS) Rename(oldpath, newpath string) error { return f.rename(oldpath, newpath, false) }

// RenameNoReplace is Rename that refuses an existing newpath with EEXIST and moves
// nothing: decided by the leader (Rename.NoReplace) once every live member reads codec
// 9, so a name another gateway created first is refused too; before, by this gateway's
// lookup (I-224, I-226).
func (f *FS) RenameNoReplace(oldpath, newpath string) error {
	return f.rename(oldpath, newpath, true)
}

func (f *FS) rename(oldpath, newpath string, noReplace bool) error {
	ctx := context.Background()
	if isSnapshotPath(meta.SplitPath(oldpath)) || isSnapshotPath(meta.SplitPath(newpath)) {
		return mapErr("rename", oldpath, ErrReadOnly) // nothing moves into, out of or within /.snapshots (I-185)
	}
	op, on := parentOf(meta.SplitPath(oldpath))
	np, nn := parentOf(meta.SplitPath(newpath))
	if on == "" || nn == "" {
		return mapErr("rename", oldpath, meta.ErrBadCommand)
	}
	oldDir, err := f.st.Resolve(op)
	if err != nil {
		return mapErr("rename", oldpath, err)
	}
	// a directory cannot move under itself; refuse before creating any parent (the FSM
	// would refuse too, but only after mkdirAll had left new directories behind)
	if oldParts := meta.SplitPath(oldpath); len(oldParts) <= len(np) && slices.Equal(oldParts, np[:len(oldParts)]) {
		return mapErr("rename", oldpath, meta.ErrLoop)
	}
	if err := f.mkdirAll(ctx, np, 0o755); err != nil {
		return mapErr("rename", newpath, err)
	}
	newDir, err := f.st.Resolve(np)
	if err != nil {
		return mapErr("rename", newpath, err)
	}
	// a replaced entry goes to the trash and keeps its WAL (its flush commits into the
	// trash entry, L-203); an ephemeral one is unlinked and loses it (L-208)
	var replaced meta.Ino
	if dst, ok := f.st.Lookup(newDir.Ino, nn); ok && dst.Type != meta.TDir && hardDelete(np, nn) {
		if src, ok := f.st.Lookup(oldDir.Ino, on); !ok || src.Ino != dst.Ino {
			replaced = dst.Ino // a rename onto itself replaces nothing (I-238)
		}
	}
	if dst, ok := f.st.Lookup(newDir.Ino, nn); noReplace && ok && (oldDir.Ino != newDir.Ino || on != nn) {
		if src, ok := f.st.Lookup(oldDir.Ino, on); !ok || src.Ino != dst.Ino {
			return mapErr("rename", newpath, meta.ErrExist)
		}
	}
	if _, err := f.propose(ctx, meta.Command{Op: meta.OpRename, Rename: &meta.Rename{
		OldParent: oldDir.Ino, OldName: on, NewParent: newDir.Ino, NewName: nn, Now: f.now(),
		NoReplace: noReplace && f.codecAtLeast(meta.CodecRewrite),
	}}); err != nil && !landedEarlier(err, meta.ErrNotExist) { // the first attempt moved it
		return mapErr("rename", oldpath, err)
	}
	if replaced != 0 {
		f.ov.Drop(replaced)
	}
	return nil
}

// hardDelete reports whether a delete of name under the directory parts unlinks rather
// than trashes (docs/06 §1, I-157): an ephemeral name, or anything under the reserved
// folder of docs/13 §5. The FSM applies the same rule; this is the gateway's side of
// it, which decides what happens to the overlay.
func hardDelete(dir []string, name string) bool {
	return meta.IsEphemeral(name) || (len(dir) > 0 && dir[0] == meta.ReservedDir) || (len(dir) == 0 && name == meta.ReservedDir)
}

// Remove deletes filename into the trash (docs/02 §Trash): the name is gone from every
// gateway at once, the entry and its bytes stay for the browser UI to restore. A
// directory must be empty, as POSIX asks. An ephemeral name is unlinked outright. While
// a member is too old for the trash (meta.ErrCodec, docs/12 §6) the delete falls back to
// the plain unlink, so rm keeps working through an update.
func (f *FS) Remove(filename string) error { return f.remove(filename, false, 0) }

// RemoveIno is Remove of the file filename names only while it is still the inode ino,
// and nothing otherwise (ErrNotExist): a client may have renamed the file away and made
// a new one under the name. Once every live member reads codec 9 the leader decides it
// (Trash.Expect); before, this gateway's lookup does. own is RemoveOwn's (I-225).
//
// An own delete of a file that holds no data (I-229), Windows' copy engine's first pass
// over a batch, leaves nothing in the trash: the inode is purged once the name is gone.
func (f *FS) RemoveIno(filename string, ino meta.Ino, own bool) error {
	scratch := own && ino != 0 && f.unwritten(ino)
	err := f.remove(filename, own, ino)
	if own && errors.Is(err, syscall.EOPNOTSUPP) {
		err = f.remove(filename, false, ino)
	}
	if err == nil && scratch {
		f.purgeScratch(ino)
	}
	return err
}

// RemoveOwn is Remove for an own cleanup: the SMB handle that created the file deletes it,
// as a failed copy does, and a protected folder lets that through for a file (I-220).
// While a member is too old for it (meta.ErrCodec) it is a plain Remove, whose refusal
// under protection is the answer.
func (f *FS) RemoveOwn(filename string) error {
	err := f.remove(filename, true, 0)
	if errors.Is(err, syscall.EOPNOTSUPP) {
		return f.remove(filename, false, 0)
	}
	return err
}

// validData is ino's valid data length and size (File.ValidData, I-227). The overlay is
// read first, then the inode: a flush moves dirty bytes into the inode before it clears
// them here, so whatever commits in between shows in one of the two.
func (f *FS) validData(ino meta.Ino) (valid, size uint64, err error) {
	dirtyEnd, limit, dirty := f.ov.DataEnd(ino)
	in, ok := f.st.GetInodeAny(ino) // an open file in the trash still reads and flushes (I-155)
	if !ok {
		return 0, 0, meta.ErrNotExist
	}
	end := uint64(0)
	cs := uint64(f.st.Volume().ChunkSize)
	for i, h := range slices.Backward(in.Chunks) {
		if h != meta.HashZero {
			end = min(in.Size, uint64(i+1)*cs) //nolint:gosec // i is an index
			break
		}
	}
	size = in.Size
	if dirty {
		end = max(min(end, limit), dirtyEnd)
		if s, ok := f.ov.Size(ino, in.Size); ok {
			size = s
		}
	}
	return min(end, size), size, nil
}

// unwritten reports whether ino holds no data: every byte of it reads as zero (I-229).
func (f *FS) unwritten(ino meta.Ino) bool {
	v, _, err := f.validData(ino)
	return err == nil && v == 0
}

// purgeScratch removes for good a file an SMB handle created and deleted without
// writing a byte into it (I-229): it went to the trash as any delete does, and holds
// nothing to restore. It is checked again first: a byte that landed since is kept in
// the trash. A protected folder keeps it in the trash too (Purge obeys the flag).
func (f *FS) purgeScratch(ino meta.Ino) {
	if !f.unwritten(ino) {
		return
	}
	if err := f.Purge(ino); err != nil {
		return // left in the trash, where the retention purge takes it
	}
	f.ov.Drop(ino)
}

func (f *FS) remove(filename string, own bool, expect meta.Ino) error {
	ctx := context.Background()
	if isSnapshotPath(meta.SplitPath(filename)) {
		return mapErr("remove", filename, ErrReadOnly)
	}
	dp, name := parentOf(meta.SplitPath(filename))
	if name == "" {
		return mapErr("remove", filename, meta.ErrBadCommand)
	}
	dir, err := f.st.Resolve(dp)
	if err != nil {
		return mapErr("remove", filename, err)
	}
	in, ok := f.st.Lookup(dir.Ino, name)
	if !ok || (expect != 0 && in.Ino != expect) {
		return mapErr("remove", filename, meta.ErrNotExist) // another file holds the name (I-225)
	}
	hard := hardDelete(dp, name)
	trash := &meta.Trash{Parent: dir.Ino, Name: name, Own: own, Now: f.now()}
	if f.codecAtLeast(meta.CodecRewrite) {
		trash.Expect = expect // the leader decides; this gateway may not have seen a rename yet
	}
	_, err = f.propose(ctx, meta.Command{Op: meta.OpTrash, Trash: trash})
	if trash.Expect != 0 && errors.Is(err, meta.ErrCodec) {
		// the leader sees a member older than codec 9 (a rolling upgrade): the lookup
		// above decided, and the delete still goes to the trash, not a hard unlink
		plain := *trash
		plain.Expect = 0
		_, err = f.propose(ctx, meta.Command{Op: meta.OpTrash, Trash: &plain})
	}
	if expect != 0 && errors.Is(err, meta.ErrConflict) {
		return mapErr("remove", filename, meta.ErrNotExist) // the name is another file's now
	}
	if own && errors.Is(err, meta.ErrCodec) {
		return mapErr("remove", filename, err) // RemoveOwn retries it plain
	}
	if errors.Is(err, meta.ErrCodec) {
		hard = true
		var cmd meta.Command
		if in.Type == meta.TDir {
			cmd = meta.Command{Op: meta.OpRmdir, Rmdir: &meta.Rmdir{Parent: dir.Ino, Name: name, Now: f.now()}}
		} else {
			cmd = meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: dir.Ino, Name: name, Now: f.now()}}
		}
		_, err = f.propose(ctx, cmd)
	}
	if err != nil && !landedEarlier(err, meta.ErrNotExist) { // the first attempt removed it
		return mapErr("remove", filename, err)
	}
	if hard {
		f.ov.Drop(in.Ino) // a trashed file keeps its WAL: the flush commits into the trash entry (L-203)
	}
	return nil
}

// OpenTrash opens, read-only, a file inside the deleted root: the root itself with no
// path, or path relative to it (docs/02 §Reads; the browser UI's trash view, I-155).
func (f *FS) OpenTrash(root meta.Ino, path []string) (billy.File, error) {
	name := filepath.Join(append([]string{"trash", strconv.FormatUint(uint64(root), 10)}, path...)...)
	in, err := f.st.ResolveFrom(root, path)
	if err != nil {
		return nil, mapErr("open", name, err)
	}
	if in.Type != meta.TFile {
		return nil, mapErr("open", name, meta.ErrIsDir)
	}
	return &File{fs: f, ino: in.Ino, name: name, flag: os.O_RDONLY}, nil
}

// Join uses the OS separator like billy's osfs does; every entry point splits on both
// separators, so callers may pass either form.
func (f *FS) Join(elem ...string) string { return filepath.Join(elem...) }

// --- billy.TempFile ------------------------------------------------------------------

func (f *FS) TempFile(dir, prefix string) (billy.File, error) {
	var b [8]byte
	for range 100 {
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
	if parts := meta.SplitPath(p); isSnapshotPath(parts) {
		return f.snapReadDir(p, parts)
	}
	in, err := f.resolve(p)
	if err != nil {
		return nil, mapErr("readdir", p, err)
	}
	ents, err := f.st.ReadDir(in.Ino)
	if err != nil {
		return nil, mapErr("readdir", p, err)
	}
	// a folder the actor holds no role on lists only what is on the way to something
	// they hold one on (docs/21 §3, I-348); nothing, when there is nothing
	filter := false
	if f.enforced() && f.role(in.Ino) < meta.RoleRead {
		if !f.st.Traverses(f.actor, in.Ino) {
			if in.Ino == meta.RootIno {
				return []os.FileInfo{}, nil // nobody, or a user who holds nothing: an empty root (I-350)
			}
			return nil, mapErr("readdir", p, f.refuse("list", p))
		}
		filter = true
	}
	out := make([]os.FileInfo, 0, len(ents))
	for i := range ents {
		if in.Ino == meta.RootIno && ents[i].Name == SnapshotDir {
			continue // the reserved name: /.snapshots resolves to the snapshots, never lists (I-185)
		}
		if filter && !f.st.Traverses(f.actor, ents[i].Ino) {
			continue
		}
		out = append(out, f.info(ents[i]))
	}
	return out, nil
}

// LookupFold is the name of the entry of folder dir that differs from name only by case
// (I-267); ok is false where the names are not the FSM's (under /.snapshots), and the
// caller then lists the folder.
func (f *FS) LookupFold(dir, name string) (match string, found, ok bool) {
	parts := meta.SplitPath(dir)
	if isSnapshotPath(parts) {
		return "", false, false
	}
	in, err := f.resolve(dir)
	if err != nil {
		return "", false, true
	}
	match, found = f.st.LookupFold(in.Ino, name)
	if found && in.Ino == meta.RootIno && match == SnapshotDir {
		return "", false, true // the reserved name never lists (I-185), and never folds
	}
	if found && f.enforced() {
		// a name folds only for an actor who would see it listed (I-348, I-353)
		if child, ok := f.st.Lookup(in.Ino, match); !ok || (f.role(in.Ino) < meta.RoleRead && !f.st.Traverses(f.actor, child.Ino)) {
			return "", false, true
		}
	}
	return match, found, true
}

// Role reports what the actor holds on name for the NFS ACCESS answer (docs/21 §5,
// I-351): read and write by the role, and read alone on a directory on the way to
// something they hold a role on, whose listing they get filtered (I-348): a client asks
// ACCESS before it lists, and one answered no READ never sends the READDIR (the Linux
// client refused `ls` of such a folder on the VM rig). EACCES elsewhere.
func (f *FS) Role(name string) (read, write bool, err error) {
	parts := meta.SplitPath(name)
	if isSnapshotPath(parts) {
		n, lerr := f.snapLookup(context.Background(), parts)
		if lerr != nil {
			return false, false, snapErr("access", name, lerr)
		}
		if cerr := f.checkSnap(&n); cerr != nil {
			return false, false, mapErr("access", name, cerr)
		}
		return true, false, nil
	}
	in, err := f.resolve(name)
	if err != nil {
		return false, false, mapErr("access", name, err)
	}
	if !f.enforced() {
		return true, true, nil
	}
	role := f.role(in.Ino)
	if role < meta.RoleRead && in.Ino != meta.RootIno && !f.st.Traverses(f.actor, in.Ino) {
		return false, false, mapErr("access", name, f.refuse("stat", name))
	}
	return role >= meta.RoleRead || in.Type == meta.TDir, role >= meta.RoleWrite, nil
}

// OpenBest opens name for writing when the actor holds Write on it and for reading
// otherwise, telling the caller which; the SMB gateway, whose CREATE asks for no access
// of its own, keeps a Read-only user's handle read-only without a refusal recorded for
// every open (docs/21 §5).
func (f *FS) OpenBest(name string) (fh *File, writable bool, err error) {
	flag := os.O_RDWR
	if parts := meta.SplitPath(name); !isSnapshotPath(parts) && f.enforced() {
		if in, rerr := f.resolve(name); rerr == nil && f.role(in.Ino) < meta.RoleWrite {
			flag = os.O_RDONLY
		}
	}
	bf, err := f.OpenFile(name, flag, 0)
	if errors.Is(err, ErrReadOnly) {
		flag = os.O_RDONLY
		bf, err = f.OpenFile(name, flag, 0) // a snapshot reads; its writes are refused (I-185)
	}
	if err != nil {
		return nil, false, err
	}
	vf, ok := bf.(*File)
	if !ok {
		_ = bf.Close()
		return nil, false, fmt.Errorf("open %s: not a vfs file", name)
	}
	return vf, flag == os.O_RDWR, nil
}

func (f *FS) MkdirAll(filename string, perm os.FileMode) error {
	if err := f.mkdirAll(context.Background(), meta.SplitPath(filename), perm); err != nil {
		return mapErr("mkdir", filename, err)
	}
	return nil
}

// MkdirExcl makes the directory filename only when the name is free, as the leader
// decides: a name another client took first is os.ErrExist whether or not this gateway
// has applied that create yet, as an exclusive file create is (I-222). Missing parents
// are made. SMB's FILE_CREATE of a directory is this.
func (f *FS) MkdirExcl(filename string, perm os.FileMode) error {
	ctx := context.Background()
	parts := meta.SplitPath(filename)
	if isSnapshotPath(parts) {
		return mapErr("mkdir", filename, ErrReadOnly)
	}
	dp, name := parentOf(parts)
	if name == "" {
		return mapErr("mkdir", filename, meta.ErrExist) // the root
	}
	if err := f.mkdirAll(ctx, dp, 0o755); err != nil {
		return mapErr("mkdir", filename, err)
	}
	parent, err := f.st.Resolve(dp)
	if err != nil {
		return mapErr("mkdir", filename, err)
	}
	asked := f.now()
	_, err = f.propose(ctx, meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{
		Parent: parent.Ino, Name: name, Mode: uint32(perm.Perm()), Uid: f.uid, Gid: f.gid, Now: asked,
	}})
	if errors.Is(err, meta.ErrExist) && errors.Is(err, meta.ErrUnknownOutcome) {
		// our own first attempt may have landed (I-95): an empty directory made in the window
		if in, ok := f.st.Lookup(parent.Ino, name); ok && in.Type == meta.TDir && in.Ctime >= asked && in.Ctime <= f.now() {
			if ents, _ := f.st.ReadDir(in.Ino); len(ents) == 0 {
				return nil
			}
		}
	}
	return mapErr("mkdir", filename, err)
}

func (f *FS) mkdirAll(ctx context.Context, parts []string, perm os.FileMode) error {
	if isSnapshotPath(parts) {
		return ErrReadOnly
	}
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
			Parent: cur, Name: name, Mode: uint32(perm.Perm()), Uid: f.uid, Gid: f.gid, Now: f.now(),
		}})
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
	if isSnapshotPath(meta.SplitPath(link)) {
		return mapErr("symlink", link, ErrReadOnly)
	}
	dp, name := parentOf(meta.SplitPath(link))
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
		Parent: dir.Ino, Name: name, Type: meta.TSymlink, Mode: 0o777, Uid: f.uid, Gid: f.gid, Target: target, Now: f.now(),
	}}); err != nil {
		if landedEarlier(err, meta.ErrExist) { // the first attempt created it: it must be this link
			if in, ok := f.st.Lookup(dir.Ino, name); ok && in.Type == meta.TSymlink && in.Target == target {
				return nil
			}
		}
		return mapErr("symlink", link, err)
	}
	return nil
}

func (f *FS) Readlink(link string) (string, error) {
	if parts := meta.SplitPath(link); isSnapshotPath(parts) {
		return f.snapReadlink(link, parts)
	}
	in, err := f.resolve(link)
	if err != nil {
		return "", mapErr("readlink", link, err)
	}
	if err := f.checkRead(in.Ino, link); err != nil { // the target is the link's content (I-353)
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
	if isSnapshotPath(meta.SplitPath(name)) {
		return mapErr("setattr", name, ErrReadOnly)
	}
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

// Chtimes sets a client's times. One set while the file has unflushed writes is noted in
// the overlay, so the flush that commits those writes keeps it (I-230). A time outside
// what the volume represents (nanoseconds since 1970 in an int64: 1677–2262) is held at
// the nearest end rather than wrapped.
func (f *FS) Chtimes(name string, atime, mtime time.Time) error {
	sa := meta.SetAttr{Mask: meta.AttrAtime | meta.AttrMtime, Atime: clampNano(atime), Mtime: clampNano(mtime)}
	if err := f.setAttr(name, sa); err != nil {
		return err
	}
	if in, err := f.resolve(name); err == nil {
		f.ov.NoteTimes(in.Ino, sa.Atime, sa.Mtime)
	}
	return nil
}

var (
	earliestTime = time.Unix(0, math.MinInt64)
	latestTime   = time.Unix(0, math.MaxInt64)
)

// clampNano is t in nanoseconds since 1970, held within an int64.
func clampNano(t time.Time) int64 {
	switch {
	case t.Before(earliestTime):
		return math.MinInt64
	case t.After(latestTime):
		return math.MaxInt64
	}
	return t.UnixNano()
}
