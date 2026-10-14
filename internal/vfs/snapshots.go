package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
)

// SnapshotDir is the reserved name at the root under which every mirror's snapshots are
// read (docs/16 §6, I-185): absent from the root's listing, resolved by name, read-only.
const SnapshotDir = ".snapshots"

// MirrorSource reads a mirror's trees and objects wherever the mirror is attached: the
// host's own disk, or the host over the node RPC (docs/16 §6). Both are verified against
// their hash before they are returned.
type MirrorSource interface {
	Tree(ctx context.Context, m meta.NodeIdx, h meta.Hash) (mirror.Tree, error)
	Object(ctx context.Context, m meta.NodeIdx, h meta.Hash) ([]byte, error)
	Online(m meta.NodeIdx) bool
}

// Bounds of a gateway's snapshot caches (docs/16 §8): decoded trees by hash, and the
// (ino, gen) → path index a handle is resolved through, per snapshot.
const (
	maxSnapTrees   = 1024
	maxSnapIndexed = 1 << 20
)

// Fileids of the synthetic directories, above any real inode (docs/07).
const (
	fileidSnapshots uint64 = 1 << 63
	fileidMirror           = fileidSnapshots | 1<<62
	fileidSnapRoot         = fileidSnapshots | 1<<61
)

type snapKey struct {
	ino uint64
	gen uint32
}

// snapState is the gateway's view of the snapshots: its mirror source and its caches.
type snapState struct {
	src     MirrorSource
	mu      sync.Mutex
	trees   map[meta.Hash]mirror.Tree
	index   map[meta.SnapID]map[snapKey][]string
	indexed int
}

// SetMirrors gives the filesystem its mirrors; without them a snapshot lists from the
// catalogue and every read in it is EIO.
func (f *FS) SetMirrors(src MirrorSource) {
	f.snaps.mu.Lock()
	f.snaps.src = src
	f.snaps.mu.Unlock()
}

// online is the source when mirror m is attached, else nil.
func (s *snapState) online(m meta.NodeIdx) MirrorSource {
	s.mu.Lock()
	src := s.src
	s.mu.Unlock()
	if src == nil || !src.Online(m) {
		return nil
	}
	return src
}

func isSnapshotPath(parts []string) bool { return len(parts) > 0 && parts[0] == SnapshotDir }

// snapNode is a resolved path under /.snapshots: depth 1 the directory itself, 2 a
// mirror's, 3 a snapshot's root and below it its entries.
type snapNode struct {
	depth int
	row   meta.Node
	sn    meta.Snapshot
	e     mirror.Entry
	rel   []string // the path within the snapshot
}

// mirrorDirName is a mirror's directory: its row name with "/" as "-" (pike/m1 → pike-m1).
func mirrorDirName(row *meta.Node) string { return strings.ReplaceAll(row.Name, "/", "-") }

func (f *FS) mirrorRows() []meta.Node {
	var out []meta.Node
	nodes := f.st.Nodes()
	for i := range nodes {
		if n := &nodes[i]; n.Kind == meta.KindMirror && n.State != meta.Removed {
			out = append(out, *n)
		}
	}
	slices.SortFunc(out, func(a, b meta.Node) int { return strings.Compare(mirrorDirName(&a), mirrorDirName(&b)) })
	return out
}

// readable is whether a snapshot shows on the mounts: complete or incomplete (docs/16 §5).
func readable(sn *meta.Snapshot) bool {
	return sn.State == meta.SnapComplete || sn.State == meta.SnapIncomplete
}

func (f *FS) snapshotsOf(m meta.NodeIdx) []meta.Snapshot {
	var out []meta.Snapshot
	for _, sn := range f.st.Snapshots() {
		if sn.Mirror == m && readable(&sn) {
			out = append(out, sn)
		}
	}
	slices.SortFunc(out, func(a, b meta.Snapshot) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func snapRootEntry(sn *meta.Snapshot) mirror.Entry {
	return mirror.Entry{Name: sn.Name, Type: mirror.TypeDir, Mode: 0o555, Mtime: sn.At, Child: sn.Root}
}

// ErrMirrorOffline is a snapshot whose mirror is offline: a read under /.snapshots maps
// it to EIO, the browser answers 409 offline and says why (docs/13 §3).
var ErrMirrorOffline = errors.New("vfs: the mirror is offline")

// SnapshotRef is one readable snapshot as the browser addresses it (docs/13 §3): its
// catalogue row, its root under /.snapshots, its mirror's directory name and whether the
// mirror is online now.
type SnapshotRef struct {
	Snapshot meta.Snapshot
	Root     string // "/.snapshots/<mirror>/<name>"
	Mirror   string // pike-m1
	Online   bool
}

// Snapshot finds the complete or incomplete snapshot id, false when the catalogue has
// no such readable row.
func (f *FS) Snapshot(id meta.SnapID) (SnapshotRef, bool) {
	sn, ok := f.st.SnapshotRow(id)
	if !ok || !readable(&sn) {
		return SnapshotRef{}, false
	}
	row, ok := f.st.Node(sn.Mirror)
	if !ok || row.Kind != meta.KindMirror || row.State == meta.Removed {
		return SnapshotRef{}, false
	}
	dir := mirrorDirName(&row)
	return SnapshotRef{Snapshot: sn, Root: "/" + SnapshotDir + "/" + dir + "/" + sn.Name, Mirror: dir, Online: f.snaps.online(sn.Mirror) != nil}, true
}

// MirrorDirName is a mirror's directory under /.snapshots: its row name with "/" as "-".
func MirrorDirName(row *meta.Node) string { return mirrorDirName(row) }

// snapLookup resolves parts (parts[0] == SnapshotDir).
func (f *FS) snapLookup(ctx context.Context, parts []string) (snapNode, error) {
	n := snapNode{depth: 1}
	if len(parts) == 1 {
		return n, nil
	}
	rows := f.mirrorRows()
	i := slices.IndexFunc(rows, func(r meta.Node) bool { return mirrorDirName(&r) == parts[1] })
	if i < 0 {
		return n, meta.ErrNotExist
	}
	n.depth, n.row = 2, rows[i]
	if len(parts) == 2 {
		return n, nil
	}
	sns := f.snapshotsOf(n.row.Idx)
	j := slices.IndexFunc(sns, func(sn meta.Snapshot) bool { return sn.Name == parts[2] })
	if j < 0 {
		return n, meta.ErrNotExist
	}
	n.depth, n.sn, n.e = 3, sns[j], snapRootEntry(&sns[j])
	for _, name := range parts[3:] {
		if n.e.Type != mirror.TypeDir {
			return n, meta.ErrNotDir
		}
		t, err := f.snapTree(ctx, n.row.Idx, n.e.Child)
		if err != nil {
			return n, err
		}
		e, ok := t.Lookup(name)
		if !ok {
			return n, meta.ErrNotExist
		}
		n.e, n.rel = e, append(n.rel, name)
	}
	return n, nil
}

// snapTree reads tree h of mirror m, cached by hash (trees are immutable).
func (f *FS) snapTree(ctx context.Context, m meta.NodeIdx, h meta.Hash) (mirror.Tree, error) {
	s := &f.snaps
	src := s.online(m)
	if src == nil { // before the cache: offline reads EIO even where a tree is cached
		return mirror.Tree{}, ErrMirrorOffline
	}
	s.mu.Lock()
	t, ok := s.trees[h]
	s.mu.Unlock()
	if ok {
		return t, nil
	}
	t, err := src.Tree(ctx, m, h)
	if err != nil {
		return mirror.Tree{}, err
	}
	s.mu.Lock()
	if len(s.trees) >= maxSnapTrees {
		clear(s.trees)
	}
	s.trees[h] = t
	s.mu.Unlock()
	return t, nil
}

// snapErr maps a failure under /.snapshots: a missing name is ENOENT, a mutation EROFS,
// anything the mirror could not answer EIO (I-185).
func snapErr(op, name string, err error) error {
	switch {
	case errors.Is(err, meta.ErrNotExist), errors.Is(err, meta.ErrNotDir), errors.Is(err, meta.ErrIsDir), errors.Is(err, ErrReadOnly):
		return mapErr(op, name, err)
	case errors.Is(err, mirror.ErrNotFound): // a path the tree names but the mirror lacks
		return mapErr(op, name, syscall.EIO)
	}
	return mapErr(op, name, syscall.EIO)
}

// snapInfo is the os.FileInfo of an entry under /.snapshots: read-only, its fileid the
// recorded inode (or a synthetic one above every inode for the directories above a
// snapshot's tree).
type snapInfo struct {
	name   string
	typ    mirror.EntryType
	mode   uint32
	size   uint64
	mtime  int64
	uid    uint32
	gid    uint32
	fileid uint64
}

var _ os.FileInfo = (*snapInfo)(nil)

func (fi *snapInfo) Name() string { return fi.name }
func (fi *snapInfo) Size() int64  { return int64(fi.size) } //nolint:gosec // a file's size is below MaxFileSize
func (fi *snapInfo) Mode() os.FileMode {
	m := os.FileMode(fi.mode & 0o555) // read-only: no write bit (I-185)
	switch fi.typ {
	case mirror.TypeDir:
		m |= os.ModeDir
	case mirror.TypeSymlink:
		m |= os.ModeSymlink
	case mirror.TypeFile:
	}
	return m
}
func (fi *snapInfo) ModTime() time.Time { return time.Unix(0, fi.mtime) }
func (fi *snapInfo) IsDir() bool        { return fi.typ == mirror.TypeDir }
func (fi *snapInfo) Ino() meta.Ino      { return meta.Ino(fi.fileid) }

func (fi *snapInfo) Times() (atime, mtime, ctime time.Time) {
	t := time.Unix(0, fi.mtime)
	return t, t, t
}

func (fi *snapInfo) Sys() any {
	nlink := uint32(1)
	if fi.typ == mirror.TypeDir {
		nlink = 2
	}
	return &nfsfile.FileInfo{Nlink: nlink, UID: fi.uid, GID: fi.gid, Fileid: fi.fileid}
}

func entryInfo(e *mirror.Entry) *snapInfo {
	return &snapInfo{name: e.Name, typ: e.Type, mode: e.Mode, size: e.Size, mtime: e.Mtime, uid: e.Uid, gid: e.Gid, fileid: e.Ino}
}

func dirInfo(name string, mtime int64, fileid uint64) *snapInfo {
	return &snapInfo{name: name, typ: mirror.TypeDir, mode: 0o555, mtime: mtime, fileid: fileid}
}

// newestAt is the time of the newest snapshot among sns (the directory above them
// changes when one is added).
func newestAt(sns []meta.Snapshot, floor int64) int64 {
	for i := range sns {
		floor = max(floor, sns[i].At)
	}
	return floor
}

func (f *FS) snapInfoOf(n *snapNode) *snapInfo {
	switch n.depth {
	case 1:
		var at int64
		rows := f.mirrorRows()
		for i := range rows {
			at = newestAt(f.snapshotsOf(rows[i].Idx), at)
		}
		return dirInfo(SnapshotDir, at, fileidSnapshots)
	case 2:
		return dirInfo(mirrorDirName(&n.row), newestAt(f.snapshotsOf(n.row.Idx), n.row.JoinedAt), fileidMirror|uint64(n.row.Idx))
	}
	fi := entryInfo(&n.e)
	if len(n.rel) == 0 {
		fi.fileid = fileidSnapRoot | uint64(n.sn.ID)
	}
	return fi
}

// checkSnap applies the live grants to a snapshot entry (docs/21 §3, I-348): the
// directories above the entries are navigation; an entry is read with Read on the
// inode it was captured from, and by admins alone once that inode is gone.
func (f *FS) checkSnap(n *snapNode) error {
	if !f.enforced() || n.depth <= 2 || (len(n.rel) == 0 && n.e.Type == mirror.TypeDir) {
		return nil
	}
	ino := meta.Ino(n.e.Ino)
	if _, ok := f.st.GetInodeAny(ino); !ok {
		if f.st.IsAdmin(f.actor) {
			return nil
		}
		return syscall.EACCES
	}
	if f.role(ino) < meta.RoleRead {
		return syscall.EACCES
	}
	return nil
}

func (f *FS) snapStat(name string, parts []string) (os.FileInfo, error) {
	n, err := f.snapLookup(context.Background(), parts)
	if err != nil {
		return nil, snapErr("stat", name, err)
	}
	if err := f.checkSnap(&n); err != nil {
		return nil, mapErr("stat", name, err)
	}
	return f.snapInfoOf(&n), nil
}

func (f *FS) snapReadDir(name string, parts []string) ([]os.FileInfo, error) {
	ctx := context.Background()
	n, err := f.snapLookup(ctx, parts)
	if err != nil {
		return nil, snapErr("readdir", name, err)
	}
	if err := f.checkSnap(&n); err != nil {
		return nil, mapErr("readdir", name, err)
	}
	var out []os.FileInfo
	switch n.depth {
	case 1:
		rows := f.mirrorRows()
		for i := range rows {
			out = append(out, f.snapInfoOf(&snapNode{depth: 2, row: rows[i]}))
		}
	case 2:
		for _, sn := range f.snapshotsOf(n.row.Idx) {
			out = append(out, f.snapInfoOf(&snapNode{depth: 3, row: n.row, sn: sn, e: snapRootEntry(&sn)}))
		}
	default:
		if n.e.Type != mirror.TypeDir {
			return nil, snapErr("readdir", name, meta.ErrNotDir)
		}
		t, err := f.snapTree(ctx, n.row.Idx, n.e.Child)
		if err != nil {
			return nil, snapErr("readdir", name, err)
		}
		out = make([]os.FileInfo, len(t.Entries))
		for i := range t.Entries {
			out[i] = entryInfo(&t.Entries[i])
		}
	}
	return out, nil
}

func (f *FS) snapOpen(name string, parts []string, flag int) (*File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, mapErr("open", name, ErrReadOnly)
	}
	n, err := f.snapLookup(context.Background(), parts)
	if err != nil {
		return nil, snapErr("open", name, err)
	}
	if err := f.checkSnap(&n); err != nil {
		return nil, mapErr("open", name, err)
	}
	switch {
	case n.depth < 3 || n.e.Type == mirror.TypeDir:
		return nil, mapErr("open", name, meta.ErrIsDir)
	case n.e.Type != mirror.TypeFile:
		return nil, mapErr("open", name, syscall.ELOOP)
	}
	return &File{fs: f, ino: meta.Ino(n.e.Ino), name: filepath.Join(parts...), flag: flag, snap: &snapFile{m: n.row.Idx, e: n.e}}, nil
}

func (f *FS) snapReadlink(name string, parts []string) (string, error) {
	n, err := f.snapLookup(context.Background(), parts)
	if err != nil {
		return "", snapErr("readlink", name, err)
	}
	if err := f.checkSnap(&n); err != nil {
		return "", mapErr("readlink", name, err)
	}
	if n.depth < 3 || n.e.Type != mirror.TypeSymlink {
		return "", mapErr("readlink", name, syscall.EINVAL)
	}
	return n.e.Target, nil
}

// snapFile is an open file of a snapshot: its mirror and its entry.
type snapFile struct {
	m meta.NodeIdx
	e mirror.Entry
}

// snapReadAt reads a snapshot file's bytes chunk by chunk: from the pool while the chunk
// table still holds the chunk (the reader, its cache included), from the mirror's object
// otherwise, zero-filling holes and a short chunk's tail (docs/16 §6).
func (f *FS) snapReadAt(ctx context.Context, sf *snapFile, p []byte, off int64) (int, error) {
	size := sf.e.Size
	if uint64(off) >= size {
		return 0, io.EOF
	}
	cs := uint64(f.st.Volume().ChunkSize)
	n := min(uint64(len(p)), size-uint64(off))
	for pos := uint64(off); pos < uint64(off)+n; {
		idx := pos / cs
		inChunk := pos - idx*cs
		want := min(min(cs, size-idx*cs)-inChunk, uint64(off)+n-pos)
		dst := p[pos-uint64(off) : pos-uint64(off)+want]
		var h meta.Hash
		if int(idx) < len(sf.e.Chunks) {
			h = sf.e.Chunks[idx]
		}
		data, err := f.snapChunk(ctx, sf.m, h)
		if err != nil {
			return 0, err
		}
		clear(dst)
		if uint64(len(data)) > inChunk {
			copy(dst, data[inChunk:])
		}
		pos += want
	}
	f.bytesRead.Add(n)
	if uint64(len(p)) > n {
		return int(n), io.EOF //nolint:gosec // n ≤ len(p)
	}
	return int(n), nil //nolint:gosec // n ≤ len(p)
}

func (f *FS) snapChunk(ctx context.Context, m meta.NodeIdx, h meta.Hash) ([]byte, error) {
	if h == meta.HashZero {
		return nil, nil
	}
	if _, inPool := f.st.GetChunk(h); inPool {
		if data, err := f.rd.ReadChunk(ctx, h); err == nil {
			return data, nil
		}
	}
	src := f.snaps.online(m)
	if src == nil {
		return nil, ErrMirrorOffline
	}
	return src.Object(ctx, m, h)
}

// SnapshotHandle is the handle of the entry at parts under /.snapshots (false for any
// other path), recording its path so SnapshotPath finds it again (docs/01, I-185).
func (f *FS) SnapshotHandle(parts []string) (meta.SnapHandle, bool) {
	if !isSnapshotPath(parts) {
		return meta.SnapHandle{}, false
	}
	n, err := f.snapLookup(context.Background(), parts)
	switch {
	case err != nil:
		return meta.SnapHandle{}, false
	case n.depth == 1:
		return meta.SnapHandle{}, true
	case n.depth == 2:
		return meta.SnapHandle{Gen: meta.SnapGenMirror, Ino: uint64(n.row.Idx)}, true
	case len(n.rel) == 0:
		return meta.SnapHandle{Snap: n.sn.ID}, true
	}
	h := meta.SnapHandle{Snap: n.sn.ID, Gen: n.e.Gen, Ino: n.e.Ino}
	f.snaps.remember(n.sn.ID, snapKey{n.e.Ino, n.e.Gen}, n.rel)
	return h, true
}

func (s *snapState) remember(id meta.SnapID, k snapKey, rel []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.index[id]
	if idx == nil {
		idx = map[snapKey][]string{}
		s.index[id] = idx
	}
	if _, ok := idx[k]; !ok {
		if s.indexed >= maxSnapIndexed {
			clear(s.index) // rebuilt by a walk on the next miss
			s.indexed = 0
			idx = map[snapKey][]string{}
			s.index[id] = idx
		}
		s.indexed++
	}
	idx[k] = slices.Clone(rel)
}

func (s *snapState) recall(id meta.SnapID, k snapKey) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.index[id][k]
	return p, ok
}

// SnapshotPath is the path of a snapshot handle's entry, false when the handle is stale:
// its row gone, its mirror offline, or its (ino, gen) in no tree of the snapshot. A miss
// in the index walks the snapshot's trees once (docs/01 §NFS file handle).
func (f *FS) SnapshotPath(h meta.SnapHandle) ([]string, bool) {
	if h.Snap == 0 {
		switch {
		case h.Ino == 0 && h.Gen == 0:
			return []string{SnapshotDir}, true
		case h.Gen == meta.SnapGenMirror:
			rows := f.mirrorRows()
			for i := range rows {
				if uint64(rows[i].Idx) == h.Ino {
					return []string{SnapshotDir, mirrorDirName(&rows[i])}, true
				}
			}
		}
		return nil, false
	}
	sn, ok := f.st.SnapshotRow(h.Snap)
	if !ok || !readable(&sn) {
		return nil, false
	}
	row, ok := f.st.Node(sn.Mirror)
	if !ok || row.Kind != meta.KindMirror || row.State == meta.Removed || f.snaps.online(row.Idx) == nil {
		return nil, false
	}
	if h.Ino == 0 && h.Gen == 0 {
		return []string{SnapshotDir, mirrorDirName(&row), sn.Name}, true
	}
	k := snapKey{h.Ino, h.Gen}
	rel, ok := f.snaps.recall(sn.ID, k)
	if !ok {
		f.indexSnapshot(&sn, row.Idx)
		if rel, ok = f.snaps.recall(sn.ID, k); !ok {
			return nil, false
		}
	}
	return append([]string{SnapshotDir, mirrorDirName(&row), sn.Name}, rel...), true
}

// indexSnapshot walks a snapshot's trees and records every entry's path, bounded by the
// index's own cap.
func (f *FS) indexSnapshot(sn *meta.Snapshot, m meta.NodeIdx) {
	ctx := context.Background()
	var walk func(h meta.Hash, rel []string) bool
	walk = func(h meta.Hash, rel []string) bool {
		t, err := f.snapTree(ctx, m, h)
		if err != nil {
			return false
		}
		for i := range t.Entries {
			e := &t.Entries[i]
			p := append(slices.Clip(rel), e.Name)
			f.snaps.remember(sn.ID, snapKey{e.Ino, e.Gen}, p)
			if e.Type == mirror.TypeDir && !walk(e.Child, p) {
				return false
			}
		}
		return true
	}
	walk(sn.Root, nil)
}

// SnapshotTreeHash is the hash of the tree a directory under a snapshot lists, the READDIR
// cookie verifier of its listing (docs/07); false for any other path.
func (f *FS) SnapshotTreeHash(parts []string) (meta.Hash, bool) {
	if !isSnapshotPath(parts) {
		return meta.Hash{}, false
	}
	n, err := f.snapLookup(context.Background(), parts)
	if err != nil || n.depth < 3 || n.e.Type != mirror.TypeDir {
		return meta.Hash{}, false
	}
	return n.e.Child, true
}
