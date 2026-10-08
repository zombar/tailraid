package meta

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/ec"
)

// FSM is the replicated metadata state machine (docs/02). Apply is deterministic:
// it never reads the clock or the disk, every failure is a sentinel error in
// Result.Err that leaves State unchanged, and map iteration never influences an
// outcome. The read API (store.go) serves copies under a read lock.
type FSM struct {
	mu      sync.RWMutex
	st      *State
	applied atomic.Uint64

	waitMu   sync.Mutex
	waitCond *sync.Cond
	closed   bool // set by Close: no further index will ever be applied
}

// NewFSM returns an empty FSM holding only the root directory.
func NewFSM() *FSM {
	f := &FSM{st: NewState()}
	f.waitCond = sync.NewCond(&f.waitMu)
	return f
}

// state returns the live state; callers hold f.mu. Tests use it directly.
func (f *FSM) state() *State { return f.st }

// Apply implements raft.FSM. Non-command logs advance the applied index only.
func (f *FSM) Apply(l *raft.Log) interface{} {
	f.mu.Lock()
	var res interface{}
	if l.Type == raft.LogCommand {
		res = f.applyLocked(l)
	}
	f.st.Applied = l.Index
	f.mu.Unlock()
	f.setApplied(l.Index)
	return res
}

func (f *FSM) setApplied(idx uint64) {
	f.waitMu.Lock()
	f.applied.Store(idx)
	f.waitCond.Broadcast()
	f.waitMu.Unlock()
}

func (f *FSM) applyLocked(l *raft.Log) Result {
	cmd, err := DecodeCommand(l.Data)
	if err != nil {
		return Result{Err: ErrBadCommand}
	}
	a := &applier{st: f.st, index: l.Index}
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
		return Result{}, a.commit(c.Commit)
	case OpSetPlacement:
		return Result{}, a.setPlacement(c.SetPlacement)
	case OpGCDelete:
		return a.gcDelete(c.GCDelete)
	}
	return Result{}, ErrBadCommand
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
	if n.Kind != KindDevice && n.Kind != KindS3 {
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
	a.clock(c.Now)
	n.State = c.State
	return nil
}

// --- namespace ---------------------------------------------------------------

// validName enforces I-66: a name is 1..MaxNameLen bytes, not "." or "..", and holds
// neither '/' nor NUL. Too long is its own error so the gateway can say NAMETOOLONG.
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
	return in
}

func (a *applier) create(c *Create) (Result, error) {
	if c == nil {
		return Result{}, ErrBadCommand
	}
	if err := validName(c.Name); err != nil {
		return Result{}, err
	}
	if c.Type != TFile && c.Type != TSymlink {
		return Result{}, ErrBadCommand
	}
	if c.Type == TSymlink && c.Target == "" {
		return Result{}, ErrBadCommand
	}
	p, err := a.dir(c.Parent)
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
	p, err := a.dir(c.Parent)
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

// commit applies the deltas and fixes tombstone bookkeeping. Call only after check.
func (d refDelta) commit(st *State, now int64) {
	for h, n := range d {
		c := st.Chunks[h]
		c.Refs = uint32(int(c.Refs) + n)
		if c.Refs == 0 {
			if c.TombstonedAt == 0 {
				c.TombstonedAt = now
			}
			st.Tombs[h] = struct{}{}
		} else {
			c.TombstonedAt = 0
			delete(st.Tombs, h)
		}
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
	p, err := a.dir(c.Parent)
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
	d := refDelta{}
	d.add(in.Chunks, -1)
	if err := d.check(st); err != nil {
		return err
	}
	now := a.clock(c.Now)
	d.commit(st, now)
	delete(p.Children, c.Name)
	delete(st.Inodes, ino)
	p.Mtime, p.Ctime = now, now
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
	p, err := a.dir(c.Parent)
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
	if len(in.Children) != 0 {
		return ErrNotEmpty
	}
	now := a.clock(c.Now)
	delete(p.Children, c.Name)
	delete(st.Inodes, ino)
	p.Nlink--
	p.Mtime, p.Ctime = now, now
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
	st := a.st
	op, err := a.dir(c.OldParent)
	if err != nil {
		return err
	}
	srcIno, ok := op.Children[c.OldName]
	if !ok {
		return ErrNotExist
	}
	np, err := a.dir(c.NewParent)
	if err != nil {
		return err
	}
	src := st.Inodes[srcIno]
	if c.OldParent == c.NewParent && c.OldName == c.NewName {
		return nil
	}
	if src.Type == TDir && a.isAncestor(srcIno, c.NewParent) {
		return ErrLoop
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
		}
	}
	d := refDelta{}
	if dst != nil {
		d.add(dst.Chunks, -1)
	}
	if err := d.check(st); err != nil {
		return err
	}
	now := a.clock(c.Now)
	if dst != nil {
		d.commit(st, now)
		delete(st.Inodes, dst.Ino)
		if dst.Type == TDir {
			np.Nlink--
		}
	}
	delete(op.Children, c.OldName)
	np.Children[c.NewName] = srcIno
	if src.Type == TDir && c.OldParent != c.NewParent {
		op.Nlink--
		np.Nlink++
	}
	src.Parent, src.Name, src.Ctime = c.NewParent, c.NewName, now
	op.Mtime, op.Ctime = now, now
	np.Mtime, np.Ctime = now, now
	return nil
}

func (a *applier) setAttr(c *SetAttr) error {
	if c == nil {
		return ErrBadCommand
	}
	const known = AttrMode | AttrUid | AttrGid | AttrAtime | AttrMtime
	if c.Mask&^known != 0 {
		return ErrBadCommand
	}
	in, ok := a.st.Inodes[c.Ino]
	if !ok {
		return ErrNotExist
	}
	now := a.clock(c.Now)
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

func validPlacement(k, m uint8, p []NodeIdx) bool {
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
	if in.Version != c.BaseVersion {
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
	for _, d := range c.NewChunks {
		if d.Hash == HashZero {
			return ErrBadCommand
		}
		if cur, exists := st.Chunks[d.Hash]; exists {
			if cur.Size != d.Size {
				return ErrBadCommand
			}
			continue
		}
		if d.Size == 0 || uint64(d.Size) > cs || !validPlacement(d.K, d.M, d.Placement) {
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
	newList := make([]Hash, n)
	copy(newList, oldList[:keep])
	for _, w := range c.Writes {
		newList[w.Idx] = w.Hash
	}
	// Reference deltas, checked before mutation.
	for _, e := range fresh {
		d := e.d
		st.Chunks[d.Hash] = &Chunk{Hash: d.Hash, Size: d.Size, K: d.K, M: d.M, Placement: append([]NodeIdx(nil), d.Placement...)}
	}
	delta := refDelta{}
	delta.add(newList, +1)
	delta.add(oldList, -1)
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
	delta.commit(st, now)
	in.Chunks = newList
	in.Size = c.NewSize
	in.Version++
	in.Mtime, in.Ctime = now, now
	return nil
}

func (a *applier) setPlacement(c *SetPlacement) error {
	if c == nil {
		return ErrBadCommand
	}
	ch, ok := a.st.Chunks[c.Hash]
	if !ok {
		return ErrUnknownChunk
	}
	if !validPlacement(c.K, c.M, c.Placement) {
		return ErrBadCommand
	}
	ch.K, ch.M = c.K, c.M
	ch.Placement = append([]NodeIdx(nil), c.Placement...)
	return nil
}

func (a *applier) gcDelete(c *GCDelete) (Result, error) {
	if c == nil {
		return Result{}, ErrBadCommand
	}
	st := a.st
	var victims []GCVictim
	for h := range st.Tombs {
		ch := st.Chunks[h]
		if ch.Refs == 0 && ch.TombstonedAt < c.Before {
			victims = append(victims, GCVictim{Hash: h, K: ch.K, M: ch.M, Placement: append([]NodeIdx(nil), ch.Placement...)})
		}
	}
	sort.Slice(victims, func(i, j int) bool { return bytes.Compare(victims[i].Hash[:], victims[j].Hash[:]) < 0 })
	for _, v := range victims {
		delete(st.Chunks, v.Hash)
		delete(st.Tombs, v.Hash)
	}
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
	f.mu.Unlock()
	f.setApplied(st.Applied)
	return nil
}

// WaitApplied blocks until the applied index reaches index, ctx is done, or the FSM is
// closed (ErrClosed: the node shut down with the entry in flight, so the outcome is
// unknown to this caller).
func (f *FSM) WaitApplied(ctx context.Context, index uint64) error {
	if f.applied.Load() >= index {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		f.waitMu.Lock()
		for f.applied.Load() < index {
			if f.closed {
				f.waitMu.Unlock()
				done <- ErrClosed
				return
			}
			select {
			case <-ctx.Done():
				f.waitMu.Unlock()
				return
			default:
			}
			f.waitCond.Wait()
		}
		f.waitMu.Unlock()
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		f.waitMu.Lock()
		f.waitCond.Broadcast()
		f.waitMu.Unlock()
		return ctx.Err()
	}
}

// Close wakes every waiter with ErrClosed; the FSM applies nothing afterwards (the node's
// Raft instance has shut down). Reads keep working on the last applied state.
func (f *FSM) Close() {
	f.waitMu.Lock()
	f.closed = true
	f.waitCond.Broadcast()
	f.waitMu.Unlock()
}
