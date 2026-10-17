package meta

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/ec"
)

// FSM is the replicated metadata state machine (docs/02). Apply is deterministic:
// it never reads the clock or the disk, every failure is a sentinel error in
// Result.Err that leaves State unchanged, and map iteration never influences an
// outcome. The read API (store.go) serves copies under a read lock.
type FSM struct {
	// Log, when set, reports entries this binary cannot decode (docs/12 §6); nil is silent.
	Log *slog.Logger

	incompatible atomic.Uint64 // entries that failed to decode (I-105)
	snapHook     atomic.Pointer[SnapshotHook]
	applyHook    atomic.Pointer[ApplyHook]
	lastBadLog   time.Time // rate limit for the log line above; not state

	mu      sync.RWMutex
	st      *State
	stats   Stats      // volume totals, kept in step with every apply (Inodes filled on read)
	feed    changeFeed // which inodes each entry changed, for the consumers that follow it (I-386)
	applied atomic.Uint64

	waitMu sync.Mutex
	waitCh chan struct{} // closed and replaced on every applied index: one broadcast, no goroutine per waiter
	closed bool          // set by Close: no further index will ever be applied
}

// SnapshotHook is called inside Apply for every SnapshotBegin that applied, with the new
// row, its mirror's host (index and node ID) and a clone of the state at its index: the
// host takes the clone (docs/16 §3), every other node returns at once. It runs under the
// apply lock, so it must not call back into the FSM (not even a read: the lock is not
// reentrant), and clone is valid only during the call.
type SnapshotHook func(sn Snapshot, host NodeIdx, hostID string, clone func() *State)

// ApplyHook is called after every applied command, outside the apply lock, with the
// command, its index and its result: the audit log's observer (docs/21 §8, I-357). It
// may read the FSM.
type ApplyHook func(cmd Command, index uint64, res Result)

// SetApplyHook installs (or, with nil, removes) the apply hook; safe while Apply runs.
func (f *FSM) SetApplyHook(h ApplyHook) {
	if h == nil {
		f.applyHook.Store(nil)
		return
	}
	f.applyHook.Store(&h)
}

// SetSnapshotHook installs (or, with nil, removes) the hook; safe while Apply runs.
func (f *FSM) SetSnapshotHook(h SnapshotHook) {
	if h == nil {
		f.snapHook.Store(nil)
		return
	}
	f.snapHook.Store(&h)
}

// NewFSM returns an empty FSM holding only the root directory.
func NewFSM() *FSM {
	st := NewState()
	return &FSM{st: st, feed: changeFeed{max: FeedMax}, stats: Stats{cls: newClassTable(st), due: newDueIndex(), lib: newLibraries()}, waitCh: make(chan struct{})}
}

// state returns the live state; callers hold f.mu. Only the package's tests use it.
func (f *FSM) state() *State { return f.st }

// Incompatible is the number of log entries this binary could not decode (I-105): zero
// on a cluster that runs one release.
func (f *FSM) Incompatible() uint64 { return f.incompatible.Load() }

// Apply implements raft.FSM. Non-command logs advance the applied index only.
func (f *FSM) Apply(l *raft.Log) any {
	var cmd Command
	var decodeErr error
	if l.Type == raft.LogCommand {
		cmd, decodeErr = DecodeCommand(l.Data) // outside the lock: readers are not held up by decoding
	}
	f.mu.Lock()
	var res any
	if l.Type == raft.LogCommand {
		res = f.applyLocked(cmd, decodeErr, l.Index)
	}
	f.st.Applied = l.Index
	if r, ok := res.(Result); ok && r.SnapID != 0 && cmd.Op == OpSnapshotBegin && f.snapHook.Load() != nil {
		// the volume as of this index, for the mirror's host (docs/16 §3): clone is
		// valid only during the call, under the apply lock; nothing replicated changes
		sn := *f.st.Snaps[r.SnapID]
		var host NodeIdx
		var hostID string
		if m := f.st.Nodes[sn.Mirror]; m != nil {
			host = m.Host
			if h := f.st.Nodes[host]; h != nil {
				hostID = h.ID
			}
		}
		(*f.snapHook.Load())(sn, host, hostID, f.st.Clone)
	}
	f.mu.Unlock()
	f.setApplied(l.Index)
	if h := f.applyHook.Load(); h != nil && l.Type == raft.LogCommand && decodeErr == nil {
		if r, ok := res.(Result); ok {
			(*h)(cmd, l.Index, r)
		}
	}
	return res
}

func (f *FSM) setApplied(idx uint64) {
	f.waitMu.Lock()
	f.applied.Store(idx)
	close(f.waitCh)
	f.waitCh = make(chan struct{})
	f.waitMu.Unlock()
}

func (f *FSM) applyLocked(cmd Command, decodeErr error, index uint64) Result {
	if decodeErr != nil {
		// an entry this binary cannot decode: written by a newer release (docs/12 §6).
		// The index advances identically on every replica; the count makes it visible
		// (status, I-105) and the log says so once a minute.
		f.incompatible.Add(1)
		if f.Log != nil && time.Since(f.lastBadLog) > time.Minute {
			f.lastBadLog = time.Now()
			f.Log.Error("log entry from a newer release could not be decoded; update this node", "index", index, "err", decodeErr, "total", f.incompatible.Load())
		}
		return Result{Err: ErrBadCommand}
	}
	a := &applier{st: f.st, stats: &f.stats, feed: &f.feed, index: index}
	res, err := a.apply(cmd)
	if err != nil {
		return Result{Err: err}
	}
	if a.now != 0 {
		f.st.LastNow = a.now
	}
	return res
}

// applier carries one command's context. Every op validates fully before mutating.
type applier struct {
	st    *State
	stats *Stats
	feed  *changeFeed
	index uint64
	now   int64 // clamped command clock; 0 when the op carries no Now
}

// clock clamps a command's Now to the monotonic FSM clock (review #8).
func (a *applier) clock(now int64) int64 {
	if now < a.st.LastNow {
		now = a.st.LastNow
	}
	a.now = now
	return now
}

func (a *applier) apply(c Command) (Result, error) {
	if err := a.authorize(c); err != nil { // the permission check of docs/21 §4 (I-349)
		return Result{}, err
	}
	switch c.Op {
	case OpSetVolume:
		return Result{}, a.setVolume(c.SetVolume)
	case OpMetaSet:
		return Result{}, a.metaSet(c.MetaSet)
	case OpNodeUpsert:
		return a.nodeUpsert(c.NodeUpsert)
	case OpNodeSetState:
		return Result{}, a.nodeSetState(c.NodeSetState)
	case OpCreate:
		return a.create(c.Create)
	case OpMkdir:
		return a.mkdir(c.Mkdir)
	case OpUnlink:
		return Result{}, a.unlink(c.Unlink)
	case OpRmdir:
		return Result{}, a.rmdir(c.Rmdir)
	case OpRename:
		return Result{}, a.rename(c.Rename)
	case OpSetAttr:
		return Result{}, a.setAttr(c.SetAttr)
	case OpCommit:
		if err := a.commit(c.Commit); err != nil {
			return Result{}, err
		}
		if c.Commit.RewriteStart { // the gateway's rewrite id (docs/04 §6, I-223)
			return Result{Version: a.st.Inodes[c.Commit.Ino].RewriteAt}, nil
		}
		return Result{}, nil
	case OpSetPlacement:
		return Result{}, a.setPlacement(c.SetPlacement)
	case OpGCDelete:
		return a.gcDelete(c.GCDelete)
	case OpSetVolumeMode:
		return Result{}, a.setVolumeMode(c.SetVolumeMode)
	case OpTrash:
		return Result{}, a.trash(c.Trash)
	case OpRestore:
		return Result{}, a.restore(c.Restore)
	case OpPurge:
		return Result{}, a.purge(c.Purge)
	case OpSetShardSums:
		return Result{}, a.setShardSums(c.SetShardSums)
	case OpSnapshotBegin:
		return a.snapshotBegin(c.SnapshotBegin)
	case OpSnapshotEnd:
		return Result{}, a.snapshotEnd(c.SnapshotEnd)
	case OpSnapshotDelete:
		return Result{}, a.snapshotDelete(c.SnapshotDelete)
	case OpSnapshotImport:
		return a.snapshotImport(c.SnapshotImport)
	case OpSnapshotHeal:
		return Result{}, a.snapshotHeal(c.SnapshotHeal)
	case OpSecretKeyInit:
		return Result{}, a.secretKeyInit(c.SecretKeyInit)
	case OpSecretPut:
		return Result{}, a.secretPut(c.Actor, c.SecretPut)
	case OpSecretDelete:
		return Result{}, a.secretDelete(c.Actor, c.SecretDelete)
	case OpInvite:
		return Result{}, a.inviteOp(c)
	case OpUser:
		return a.userOp(c)
	case OpGroup:
		return a.groupOp(c)
	case OpGrant:
		return Result{}, a.grantOp(c)
	case OpSession:
		return Result{}, a.sessionOp(c)
	}
	return Result{}, ErrBadCommand
}

// setVolumeMode flips automatic redundancy (docs/03); the policy itself and its version
// are untouched, the next repair pass applies the rule.
func (a *applier) setVolumeMode(c *SetVolumeMode) error {
	if c == nil {
		return ErrBadCommand
	}
	a.clock(c.Now)
	a.st.Vol.Auto = c.Auto
	return nil
}

// --- volume, meta, nodes -----------------------------------------------------

func (a *applier) setVolume(c *SetVolume) error {
	if c == nil {
		return ErrBadCommand
	}
	st := a.st
	if c.K < 1 || int(c.K)+int(c.M) > ec.MaxShards {
		return ErrPolicy
	}
	first := st.Vol.ChunkSize == 0
	if first {
		if c.ChunkSize == 0 || c.ChunkSize > MaxChunkSize {
			return ErrPolicy // I-67: one bound for init, the FSM and the shard store
		}
		st.Vol.ChunkSize = c.ChunkSize
	} else if c.ChunkSize != 0 && c.ChunkSize != st.Vol.ChunkSize {
		return ErrPolicy
	}
	a.clock(c.Now)
	if st.Vol.K != c.K || st.Vol.M != c.M {
		st.Vol.K, st.Vol.M = c.K, c.M
		if !first {
			st.Vol.PolicyVersion++ // a policy change; the bootstrap policy is version 0
		}
	}
	return nil
}

func (a *applier) metaSet(c *MetaSet) error {
	if c == nil {
		return ErrBadCommand
	}
	st := a.st
	var zero [16]byte
	if c.ClusterID != zero {
		if st.Meta.ClusterID != zero && st.Meta.ClusterID != c.ClusterID {
			return ErrBadCommand
		}
		st.Meta.ClusterID = c.ClusterID
	}
	if c.JoinToken != "" {
		st.Meta.JoinToken = c.JoinToken
	}
	if st.Meta.CreatedAt == 0 && c.CreatedAt != 0 {
		st.Meta.CreatedAt = c.CreatedAt
	}
	return nil
}

func (a *applier) nodeUpsert(c *NodeUpsert) (Result, error) {
	if c == nil {
		return Result{}, ErrBadCommand
	}
	st := a.st
	n := c.Node
	if n.Idx != 0 {
		if _, ok := st.Nodes[n.Idx]; !ok {
			return Result{}, ErrNoSuchNode
		}
	}
	switch n.Kind {
	case KindDevice, KindS3:
		if n.Host != 0 {
			return Result{}, ErrBadCommand
		}
	case KindDisk, KindMirror:
		// a disk or a mirror belongs to an Active device for its whole life (docs/14 §1,
		// docs/16 §1); a re-homed mirror changes only its Host
		if h, ok := st.Nodes[n.Host]; !ok || h.Kind != KindDevice || h.State != Active {
			return Result{}, ErrBadCommand
		}
		if n.Kind == KindMirror && (n.Capacity != 0 || n.Voter) {
			return Result{}, ErrBadCommand // never a placement target, never a voter (I-184)
		}
	default:
		return Result{}, ErrBadCommand
	}
	if n.Idx == 0 && n.ID != "" {
		// one record per node ID: a repeated join (a retry after a timeout, possibly at a
		// new leader whose copy has not applied the first upsert) updates it instead of
		// allocating a second index (I-62)
		for idx, cur := range st.Nodes {
			if cur.ID == n.ID && cur.State != Removed {
				n.Idx = idx
				break
			}
		}
	}
	if n.Idx == 0 {
		idx := st.NextNode
		st.NextNode++
		n.Idx = idx
		n.State = Active
		st.Nodes[idx] = &n
		return Result{Ino: Ino(idx)}, nil
	}
	cur := st.Nodes[n.Idx]
	n.State = cur.State // state changes only through NodeSetState
	*cur = n
	return Result{Ino: Ino(n.Idx)}, nil
}

func (a *applier) nodeSetState(c *NodeSetState) error {
	if c == nil {
		return ErrBadCommand
	}
	if c.State != Active && c.State != Draining && c.State != Removed {
		return ErrBadCommand
	}
	n, ok := a.st.Nodes[c.Idx]
	if !ok {
		return ErrNoSuchNode
	}
	if n.State == Removed {
		return ErrBadCommand
	}
	if c.State == Removed && n.Kind == KindDevice {
		// a host leaves after its disks and its mirrors (docs/14 §1, docs/16 §1)
		for _, d := range a.st.Nodes {
			if (d.Kind == KindDisk || d.Kind == KindMirror) && d.Host == c.Idx && d.State != Removed {
				return ErrNodeNotEmpty
			}
		}
	}
	if n.Kind == KindMirror && c.State == Draining {
		return ErrBadCommand // a mirror holds no slot to move
	}
	if c.State == Removed && n.Kind == KindMirror {
		// its catalogue rows go with it (the disk keeps its snapshots, docs/16 §5); a
		// sync must end first, so no hold is dropped silently
		for _, sn := range a.st.Snaps {
			if sn.Mirror == c.Idx && sn.State == SnapSyncing {
				return ErrNodeNotEmpty
			}
		}
		for id, sn := range a.st.Snaps {
			if sn.Mirror == c.Idx {
				delete(a.st.Snaps, id)
			}
		}
	}
	if c.State == Removed {
		// Every slot is the sole copy of its shard index (I-9): a node that still holds
		// one cannot leave the table (docs/02). `node remove --force` zeroes the slots first.
		for _, ch := range a.st.Chunks {
			if slices.Contains(ch.Placement, c.Idx) {
				return ErrNodeNotEmpty
			}
		}
	}
	a.clock(c.Now)
	n.State = c.State
	return nil
}

// --- namespace ---------------------------------------------------------------

// validName enforces I-66: a name is 1..MaxNameLen bytes, not "." or "..", and holds
// neither '/' nor NUL. Too long is its own error so the gateway can say NAMETOOLONG.
// pathLen is the byte length of the path a new entry name under dir would have, "/" and
// the name included (I-115). A missing dir counts as the root; the caller reports it.
func (a *applier) pathLen(dir Ino, name string) int {
	n := 1 + len(name)
	in, ok := a.st.Inodes[dir]
	for steps := 0; ok && in.Ino != RootIno && steps <= len(a.st.Inodes); steps++ {
		n += 1 + len(in.Name)
		in, ok = a.st.Inodes[in.Parent]
	}
	return n
}

func validName(name string) error {
	switch {
	case len(name) > MaxNameLen:
		return ErrNameTooLong
	case name == "", name == ".", name == "..", strings.ContainsAny(name, "/\x00"):
		return ErrBadCommand
	}
	return nil
}

func (a *applier) dir(ino Ino) (*Inode, error) {
	in, ok := a.st.Inodes[ino]
	if !ok {
		return nil, ErrNotExist
	}
	if in.Type != TDir {
		return nil, ErrNotDir
	}
	return in, nil
}

// liveDir is dir for the namespace commands: a parent in the trash is refused, since
// only the flusher's Commit writes inside the trash (docs/02 §Protection).
func (a *applier) liveDir(ino Ino) (*Inode, error) {
	in, err := a.dir(ino)
	if err != nil {
		return nil, err
	}
	if a.st.lineage(ino).inTrash {
		return nil, ErrInTrash
	}
	return in, nil
}

func (a *applier) newInode(typ FileType, parent *Inode, name string, mode, uid, gid uint32, now int64) *Inode {
	st := a.st
	in := &Inode{
		Ino: st.NextIno, Gen: uint32(a.index), Type: typ, Mode: mode, Uid: uid, Gid: gid,
		Atime: now, Mtime: now, Ctime: now, Nlink: 1, Parent: parent.Ino, Name: name,
	}
	st.NextIno++
	st.Inodes[in.Ino] = in
	parent.Children[name] = in.Ino
	parent.Mtime, parent.Ctime = now, now
	parent.Version++ // the name table changed (docs/01)
	a.touch(in.Ino, parent.Ino)
	return in
}

func (a *applier) create(c *Create) (Result, error) {
	if c == nil {
		return Result{}, ErrBadCommand
	}
	if err := validName(c.Name); err != nil {
		return Result{}, err
	}
	if a.pathLen(c.Parent, c.Name) > MaxPathLen {
		return Result{}, ErrNameTooLong
	}
	if c.Type != TFile && c.Type != TSymlink {
		return Result{}, ErrBadCommand
	}
	if c.Type == TSymlink && c.Target == "" {
		return Result{}, ErrBadCommand
	}
	p, err := a.liveDir(c.Parent)
	if err != nil {
		return Result{}, err
	}
	if _, exists := p.Children[c.Name]; exists {
		return Result{}, ErrExist
	}
	now := a.clock(c.Now)
	in := a.newInode(c.Type, p, c.Name, c.Mode, c.Uid, c.Gid, now)
	if c.Type == TSymlink {
		in.Target = c.Target
		in.Size = uint64(len(c.Target))
	}
	return Result{Ino: in.Ino, Gen: in.Gen}, nil
}

func (a *applier) mkdir(c *Mkdir) (Result, error) {
	if c == nil {
		return Result{}, ErrBadCommand
	}
	if err := validName(c.Name); err != nil {
		return Result{}, err
	}
	if a.pathLen(c.Parent, c.Name) > MaxPathLen {
		return Result{}, ErrNameTooLong
	}
	p, err := a.liveDir(c.Parent)
	if err != nil {
		return Result{}, err
	}
	if _, exists := p.Children[c.Name]; exists {
		return Result{}, ErrExist
	}
	now := a.clock(c.Now)
	in := a.newInode(TDir, p, c.Name, c.Mode, c.Uid, c.Gid, now)
	in.Nlink = 2
	in.Children = map[string]Ino{}
	p.Nlink++
	return Result{Ino: in.Ino, Gen: in.Gen}, nil
}

// refDelta accumulates reference-count changes keyed by hash, HashZero excluded.
type refDelta map[Hash]int

func (d refDelta) add(hs []Hash, n int) {
	for _, h := range hs {
		if h != HashZero {
			d[h] += n
		}
	}
}

// check verifies every delta applies without underflow or unknown chunks.
func (d refDelta) check(st *State) error {
	for h, n := range d {
		c, ok := st.Chunks[h]
		if !ok {
			return ErrUnknownChunk
		}
		if int(c.Refs)+n < 0 {
			return ErrBadCommand
		}
	}
	return nil
}

// commit applies the deltas, fixes tombstone bookkeeping and keeps the chunk totals.
// Call only after check.
func (d refDelta) commit(st *State, stats *Stats, now int64) {
	for h, n := range d {
		c := st.mutChunk(h)
		fresh := c.Refs == 0 && c.TombstonedAt == 0 // registered by this command, not counted yet
		wasLive := c.Refs > 0
		c.Refs = uint32(int(c.Refs) + n)
		if c.Refs == 0 {
			if c.TombstonedAt == 0 {
				c.TombstonedAt = now
			}
			st.Tombs[h] = struct{}{}
			stats.due.tomb(h, c.TombstonedAt)
		} else {
			c.TombstonedAt = 0
			delete(st.Tombs, h)
			stats.due.untomb(h)
		}
		if !fresh {
			stats.chunkOut(c, wasLive)
		}
		stats.chunkIn(c)
	}
}

func (a *applier) unlink(c *Unlink) error {
	if c == nil {
		return ErrBadCommand
	}
	if err := validName(c.Name); err != nil {
		return err
	}
	st := a.st
	p, err := a.liveDir(c.Parent)
	if err != nil {
		return err
	}
	ino, ok := p.Children[c.Name]
	if !ok {
		return ErrNotExist
	}
	in := st.Inodes[ino]
	if in.Type == TDir {
		return ErrIsDir
	}
	if st.lineage(ino).protected {
		return ErrProtected // nothing under a protected directory leaves the namespace (I-154)
	}
	d := refDelta{}
	d.add(in.Chunks, -1)
	if err := d.check(st); err != nil {
		return err
	}
	now := a.clock(c.Now)
	d.commit(st, a.stats, now)
	if in.Type == TFile {
		a.stats.LogicalBytes -= in.Size
	}
	delete(p.Children, c.Name)
	delete(st.Inodes, ino)
	st.dropGrants(ino)
	a.stats.lib.drop(ino)
	a.touch(ino, p.Ino)
	p.Mtime, p.Ctime = now, now
	p.Version++ // the name table changed (docs/01)
	return nil
}

func (a *applier) rmdir(c *Rmdir) error {
	if c == nil {
		return ErrBadCommand
	}
	if err := validName(c.Name); err != nil {
		return err
	}
	st := a.st
	p, err := a.liveDir(c.Parent)
	if err != nil {
		return err
	}
	ino, ok := p.Children[c.Name]
	if !ok {
		return ErrNotExist
	}
	in := st.Inodes[ino]
	if in.Type != TDir {
		return ErrNotDir
	}
	if st.lineage(ino).protected {
		return ErrProtected
	}
	if len(in.Children) != 0 {
		return ErrNotEmpty
	}
	if len(st.Trash[ino]) != 0 {
		// an empty directory whose earlier deletes are still roots under it: the
		// mount's rmdir of a folder it emptied with rm. Trash it instead (fold), so the
		// roots come back with it (docs/02 §Trash).
		a.trashEntry(in, p, a.clock(c.Now))
		return nil
	}
	now := a.clock(c.Now)
	delete(p.Children, c.Name)
	delete(st.Inodes, ino)
	st.dropGrants(ino)
	a.stats.lib.drop(ino)
	a.touch(ino, p.Ino)
	p.Nlink--
	p.Mtime, p.Ctime = now, now
	p.Version++ // the name table changed (docs/01)
	return nil
}

// isAncestor reports whether anc is dir or one of its ancestors.
func (a *applier) isAncestor(anc, dir Ino) bool {
	for cur := dir; ; {
		if cur == anc {
			return true
		}
		in, ok := a.st.Inodes[cur]
		if !ok || cur == RootIno || in.Parent == 0 {
			return false
		}
		cur = in.Parent
	}
}

func (a *applier) rename(c *Rename) error {
	if c == nil {
		return ErrBadCommand
	}
	if err := validName(c.OldName); err != nil {
		return err
	}
	if err := validName(c.NewName); err != nil {
		return err
	}
	if a.pathLen(c.NewParent, c.NewName) > MaxPathLen {
		return ErrNameTooLong
	}
	st := a.st
	op, err := a.liveDir(c.OldParent)
	if err != nil {
		return err
	}
	srcIno, ok := op.Children[c.OldName]
	if !ok {
		return ErrNotExist
	}
	np, err := a.liveDir(c.NewParent)
	if err != nil {
		return err
	}
	src := st.Inodes[srcIno]
	if c.OldParent == c.NewParent && c.OldName == c.NewName {
		return nil
	}
	if _, taken := np.Children[c.NewName]; taken && c.NoReplace {
		return ErrExist // decided here, not by a gateway's lookup (I-224, I-226)
	}
	if src.Type == TDir && a.isAncestor(srcIno, c.NewParent) {
		return ErrLoop
	}
	// protection (I-154): the protected folder itself stays put, nothing leaves a
	// protected subtree, and nothing under one is replaced; moving in is fine
	if src.Type == TDir && src.Flags&FlagProtected != 0 {
		return ErrProtected
	}
	for _, guard := range a.protectedAncestors(c.OldParent) {
		if !a.isAncestor(guard, c.NewParent) {
			return ErrProtected
		}
	}
	var dst *Inode
	if dstIno, exists := np.Children[c.NewName]; exists {
		dst = st.Inodes[dstIno]
		switch {
		case src.Type != TDir && dst.Type == TDir:
			return ErrIsDir
		case src.Type == TDir && dst.Type != TDir:
			return ErrNotDir
		case dst.Type == TDir && len(dst.Children) != 0:
			return ErrNotEmpty
		case st.lineage(dstIno).protected:
			return ErrProtected // a replace is a removal
		}
	}
	// the replaced entry goes to the trash unless its name or place is ephemeral
	// (docs/06 §1, L-205, L-208)
	hard := dst != nil && (IsEphemeral(dst.Name) || st.lineage(dst.Ino).reserved)
	d := refDelta{}
	if hard {
		d.add(dst.Chunks, -1)
	}
	if err := d.check(st); err != nil {
		return err
	}
	now := a.clock(c.Now)
	switch {
	case hard:
		d.commit(st, a.stats, now)
		if dst.Type == TFile {
			a.stats.LogicalBytes -= dst.Size
		}
		delete(st.Inodes, dst.Ino)
		st.dropGrants(dst.Ino)
		a.stats.lib.drop(dst.Ino)
		if dst.Type == TDir {
			np.Nlink--
		}
	case dst != nil:
		a.trashEntry(dst, np, now)
	}
	delete(op.Children, c.OldName)
	np.Children[c.NewName] = srcIno
	if src.Type == TDir && c.OldParent != c.NewParent {
		op.Nlink--
		np.Nlink++
	}
	src.Parent, src.Name, src.Ctime = c.NewParent, c.NewName, now
	op.Mtime, op.Ctime = now, now
	op.Version++ // the name table changed (docs/01)
	np.Mtime, np.Ctime = now, now
	np.Version++ // the name table changed (docs/01)
	a.touch(srcIno, op.Ino, np.Ino)
	if dst != nil {
		a.touch(dst.Ino)
	}
	return nil
}

func (a *applier) setAttr(c *SetAttr) error {
	if c == nil {
		return ErrBadCommand
	}
	const known = AttrMode | AttrUid | AttrGid | AttrAtime | AttrMtime | AttrFlags
	if c.Mask&^known != 0 {
		return ErrBadCommand
	}
	in, ok := a.st.Inodes[c.Ino]
	if !ok {
		return ErrNotExist
	}
	if c.Mask&AttrFlags != 0 {
		switch {
		case in.Type != TDir:
			return ErrNotDir // flags are a directory's (docs/02 §SetAttr)
		case c.Flags&^knownFlags != 0, !(Inode{Flags: c.Flags}).Library().Known():
			return ErrBadCommand
		case a.st.lineage(c.Ino).inTrash:
			return ErrInTrash
		}
	}
	now := a.clock(c.Now)
	a.touch(in.Ino)
	if c.Mask&AttrFlags != 0 {
		in.Flags = c.Flags
		a.stats.lib.set(in)
	}
	if c.Mask&AttrMode != 0 {
		in.Mode = c.Mode
	}
	if c.Mask&AttrUid != 0 {
		in.Uid = c.Uid
	}
	if c.Mask&AttrGid != 0 {
		in.Gid = c.Gid
	}
	if c.Mask&AttrAtime != 0 {
		in.Atime = c.Atime
	}
	if c.Mask&AttrMtime != 0 {
		in.Mtime = c.Mtime
	}
	in.Ctime = now
	return nil
}

// --- commit --------------------------------------------------------------------

// ValidPlacement reports whether p is a placement the FSM accepts for (k, m): k+m slots,
// no target named twice (empty slots aside).
func ValidPlacement(k, m uint8, p []NodeIdx) bool {
	if k < 1 || int(k)+int(m) > ec.MaxShards || len(p) != int(k)+int(m) {
		return false
	}
	seen := map[NodeIdx]bool{}
	for _, idx := range p {
		if idx == 0 {
			continue
		}
		if seen[idx] {
			return false
		}
		seen[idx] = true
	}
	return true
}

// onMirror reports whether a placement names a mirror row: a mirror holds snapshots,
// never a slot (docs/16 §1, I-184), so such a placement is a bad command.
func (a *applier) onMirror(p []NodeIdx) bool {
	for _, idx := range p {
		if n, ok := a.st.Nodes[idx]; ok && n.Kind == KindMirror {
			return true
		}
	}
	return false
}

func (a *applier) commit(c *Commit) error {
	if c == nil {
		return ErrBadCommand
	}
	st := a.st
	if st.Vol.ChunkSize == 0 {
		return ErrPolicy
	}
	cs := uint64(st.Vol.ChunkSize)
	in, ok := st.Inodes[c.Ino]
	if !ok {
		return ErrNotExist
	}
	if in.Type != TFile {
		return ErrIsDir
	}
	switch {
	case c.RewriteStart:
		// a rewrite replaces the whole file whatever was committed since it was planned
		// (docs/04 §6, I-223): no BaseVersion check, and it must truncate to 0
		if !c.HasTruncate || c.TruncateTo != 0 || c.Rewrite != 0 {
			return ErrBadCommand
		}
	case c.Rewrite != 0 && in.RewriteAt != c.Rewrite:
		return ErrSuperseded // another rewrite of the file started after this one
	case in.Version != c.BaseVersion:
		return ErrConflict
	}
	if c.HasTruncate && c.TruncateTo > c.NewSize {
		return ErrBadCommand
	}
	if !c.HasTruncate && c.NewSize < in.Size {
		return ErrBadCommand // a file shrinks only through an explicit truncate (L-175)
	}
	if c.NewSize > MaxFileSize(st.Vol.ChunkSize) {
		return ErrBadCommand // bounded chunk list; checked before anything is sized by NewSize (I-64, L-182)
	}
	// Validate NewChunks before touching anything (a failing commit registers nothing).
	type newEntry struct {
		d ChunkDesc
	}
	var fresh []newEntry
	var fill []ChunkDesc // dedup hits whose sums the chunk lacked (I-201)
	for _, d := range c.NewChunks {
		if d.Hash == HashZero {
			return ErrBadCommand
		}
		if cur, exists := st.Chunks[d.Hash]; exists {
			if cur.Size != d.Size {
				return ErrBadCommand
			}
			switch {
			case len(d.Sums) == 0:
			case len(d.Sums) != int(cur.K):
				return ErrBadCommand
			case cur.Sums == nil:
				fill = append(fill, d)
			case !slices.Equal(cur.Sums, d.Sums):
				return ErrConflict // a pure function of the bytes: a difference is a bug caught here
			}
			continue
		}
		if d.Size == 0 || uint64(d.Size) > cs || !ValidPlacement(d.K, d.M, d.Placement) || a.onMirror(d.Placement) {
			return ErrBadCommand
		}
		if len(d.Sums) != 0 && len(d.Sums) != int(d.K) {
			return ErrBadCommand
		}
		fresh = append(fresh, newEntry{d})
	}
	known := func(h Hash) bool {
		if h == HashZero {
			return true
		}
		if _, exists := st.Chunks[h]; exists {
			return true
		}
		for _, e := range fresh {
			if e.d.Hash == h {
				return true
			}
		}
		return false
	}
	// Build the new chunk list: truncate, grow with holes, apply writes.
	oldList := in.Chunks
	n := ChunkCount(c.NewSize, st.Vol.ChunkSize)
	written := make(map[uint32]bool, len(c.Writes))
	for _, w := range c.Writes {
		if int(w.Idx) >= n || written[w.Idx] {
			return ErrBadCommand
		}
		if !known(w.Hash) {
			return ErrUnknownChunk
		}
		written[w.Idx] = true
	}
	keep := len(oldList)
	effSize := in.Size // logical end of retained base data
	if c.HasTruncate {
		n0 := ChunkCount(c.TruncateTo, st.Vol.ChunkSize)
		if n0 < keep {
			keep = n0
		}
		if c.TruncateTo < effSize {
			effSize = c.TruncateTo
		}
	}
	if n < keep {
		keep = n
	}
	if c.NewSize < effSize {
		effSize = c.NewSize
	}
	// The chunk straddling effSize must be rewritten unless it is a hole (review #2).
	if effSize%cs != 0 && effSize < in.Size {
		tail := uint32(effSize / cs)
		if int(tail) < keep && !written[tail] && oldList[tail] != HashZero {
			return ErrNeedTailRewrite
		}
	}
	// Reference deltas, checked before mutation.
	for _, e := range fresh {
		d := e.d
		ch := &Chunk{Hash: d.Hash, Size: d.Size, K: d.K, M: d.M, Placement: append([]NodeIdx(nil), d.Placement...)}
		if len(d.Sums) > 0 {
			ch.Sums = append([]Hash(nil), d.Sums...)
		}
		st.Chunks[d.Hash] = ch
	}
	for _, d := range fill {
		st.mutChunk(d.Hash).Sums = append([]Hash(nil), d.Sums...)
	}
	// Only the indices that change carry a delta: written ones (old entry out, new in),
	// and the entries a truncate drops; everything below keep that was not written is
	// the same hash in both lists and would cancel. Commit therefore costs O(writes),
	// not O(file length), under the FSM write lock.
	delta := refDelta{}
	for _, w := range c.Writes {
		delta.add([]Hash{w.Hash}, +1)
		if int(w.Idx) < keep {
			delta.add([]Hash{oldList[w.Idx]}, -1)
		}
	}
	delta.add(oldList[keep:], -1)
	for _, e := range fresh {
		if _, touched := delta[e.d.Hash]; !touched {
			delta[e.d.Hash] += 0 // ensure tombstone bookkeeping runs for unreferenced new entries
		}
	}
	if err := delta.check(st); err != nil {
		for _, e := range fresh {
			delete(st.Chunks, e.d.Hash)
		}
		return err
	}
	now := a.clock(c.Now)
	delta.commit(st, a.stats, now)
	// Chunk lists are immutable (copy-on-write): readers, snapshots and the clone share
	// the slice without a lock, so no element they can see changes (I-104). A Commit that
	// only appends extends the list in place, into capacity no earlier list covers (this
	// list is the longest on its array: every other Commit makes a new one), growing by
	// doubling: O(writes) amortized, so a streamed file's commits are O(n) in all, not
	// O(n²) (I-385). Any other Commit allocates a new list.
	appendOnly := !c.HasTruncate
	for _, w := range c.Writes {
		appendOnly = appendOnly && int(w.Idx) >= len(oldList)
	}
	var newList []Hash
	if appendOnly && keep == len(oldList) {
		newList = slices.Grow(oldList, n-len(oldList))[:n]
		clear(newList[len(oldList):])
	} else {
		newList = make([]Hash, n)
		copy(newList, oldList[:keep])
	}
	for _, w := range c.Writes {
		newList[w.Idx] = w.Hash
	}
	in.Chunks = newList
	if st.lineage(c.Ino).inTrash { // a dirty file deleted before its flush commits into its trash entry (L-203)
		a.stats.TrashBytes += c.NewSize - in.Size // unsigned: wraps and unwraps exactly
	} else {
		a.stats.LogicalBytes += c.NewSize - in.Size
	}
	a.touch(in.Ino)
	in.Size = c.NewSize
	in.Version++
	if c.RewriteStart {
		in.RewriteAt = in.Version
	}
	in.Mtime, in.Ctime = now, now
	applyCommitAttrs(in, c.Attrs) // the client's, after the Commit's own stamp (I-401)
	return nil
}

// applyCommitAttrs sets the fields a Commit's attributes carry, as SetAttr does; ctime
// stays the Commit's.
func applyCommitAttrs(in *Inode, a CommitAttrs) {
	if a.Mask&AttrMode != 0 {
		in.Mode = a.Mode
	}
	if a.Mask&AttrUid != 0 {
		in.Uid = a.Uid
	}
	if a.Mask&AttrGid != 0 {
		in.Gid = a.Gid
	}
	if a.Mask&AttrAtime != 0 {
		in.Atime = a.Atime
	}
	if a.Mask&AttrMtime != 0 {
		in.Mtime = a.Mtime
	}
}

func (a *applier) setPlacement(c *SetPlacement) error {
	if c == nil {
		return ErrBadCommand
	}
	ch, ok := a.st.Chunks[c.Hash]
	if !ok {
		return ErrUnknownChunk
	}
	if !ValidPlacement(c.K, c.M, c.Placement) || a.onMirror(c.Placement) {
		return ErrBadCommand
	}
	if c.Expect != nil && !slices.Equal(ch.Placement, c.Expect) {
		return ErrConflict // another proposer re-placed it first (I-102, L-197)
	}
	if ch.Refs > 0 {
		a.stats.StoredBytes -= storedBytes(ch)
		a.stats.cls.out(ch)
	}
	ch = a.st.mutChunk(c.Hash)
	if ch.K != c.K || ch.M != c.M {
		ch.Sums = nil // the shards are different bytes now; the backfill records the new sums (I-201)
	}
	ch.K, ch.M = c.K, c.M
	ch.Placement = append([]NodeIdx(nil), c.Placement...)
	if ch.Refs > 0 {
		a.stats.StoredBytes += storedBytes(ch)
		a.stats.cls.in(ch)
	}
	return nil
}

// setShardSums records a chunk's data-shard hashes (docs/04 §4, I-201): the backfill's
// command for a chunk committed without them. Refs, placement and the tombstone are
// untouched, so GC and repair see nothing change.
func (a *applier) setShardSums(c *SetShardSums) error {
	if c == nil {
		return ErrBadCommand
	}
	ch, ok := a.st.Chunks[c.Hash]
	if !ok {
		return ErrUnknownChunk
	}
	if len(c.Sums) != int(ch.K) {
		return ErrBadCommand
	}
	if ch.Sums != nil {
		if slices.Equal(ch.Sums, c.Sums) {
			return nil
		}
		return ErrConflict
	}
	a.st.mutChunk(c.Hash).Sums = append([]Hash(nil), c.Sums...)
	return nil
}

func (a *applier) gcDelete(c *GCDelete) (Result, error) {
	if c == nil {
		return Result{}, ErrBadCommand
	}
	st := a.st
	before := c.Before
	if at, ok := st.syncHold(); ok && at < before {
		// the sync hold (docs/16 §4, I-181): a chunk tombstoned after a snapshot began
		// may be referenced by it, so it waits until the copy to its mirror ends
		before = at
	}
	// the queues give the lowest due hashes without a walk of Tombs (I-381): the same
	// victims in the same order on every replica, the hash order the rule names (I-103)
	hashes := a.stats.due.takeTombs(before, int(c.Limit))
	victims := make([]GCVictim, 0, len(hashes))
	for _, h := range hashes {
		ch := st.Chunks[h]
		victims = append(victims, GCVictim{Hash: h, K: ch.K, M: ch.M, Placement: append([]NodeIdx(nil), ch.Placement...)})
		delete(st.Chunks, h)
		delete(st.Tombs, h)
	}
	a.stats.Tombstoned -= len(victims)
	return Result{Victims: victims}, nil
}

// --- snapshot / restore ----------------------------------------------------------

// Snapshot implements raft.FSM: clone under the read lock so Persist can encode
// without blocking Apply.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	cl := f.st.Clone()
	f.mu.RUnlock()
	return &snapshot{st: cl}, nil
}

type snapshot struct{ st *State }

func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	if err := EncodeState(sink, s.st); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *snapshot) Release() {}

// State exposes the cloned state (fsck runs CheckInvariants on it without holding the FSM lock).
func (s *snapshot) State() *State { return s.st }

// Restore implements raft.FSM: decode fully, then swap atomically.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	st, err := DecodeState(rc)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.st = st
	f.stats = computeStats(st)
	f.feed.reset(st.Applied) // nothing before the snapshot is known (I-386)
	f.mu.Unlock()
	f.setApplied(st.Applied)
	return nil
}

// WaitApplied blocks until the applied index reaches index, ctx is done, or the FSM is
// closed (ErrClosed: the node shut down with the entry in flight, so the outcome is
// unknown to this caller).
func (f *FSM) WaitApplied(ctx context.Context, index uint64) error {
	for {
		if f.applied.Load() >= index {
			return nil
		}
		f.waitMu.Lock()
		if f.closed {
			f.waitMu.Unlock()
			return ErrClosed
		}
		if f.applied.Load() >= index {
			f.waitMu.Unlock()
			return nil
		}
		ch := f.waitCh
		f.waitMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close wakes every waiter with ErrClosed; the FSM applies nothing afterwards (the node's
// Raft instance has shut down). Reads keep working on the last applied state.
func (f *FSM) Close() {
	f.waitMu.Lock()
	f.closed = true
	close(f.waitCh)
	f.waitCh = make(chan struct{})
	f.waitMu.Unlock()
}
