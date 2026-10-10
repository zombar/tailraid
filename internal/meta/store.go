package meta

import (
	"bytes"
	"sort"

	"github.com/zombar/tailraid/internal/ec"
)

// Store is the read API of the FSM (docs/02 §"meta.Store"). All methods take the
// read lock and return copies; an Inode copy shares its immutable Chunks slice.
type Store = FSM

// Stats summarises the volume for status output.
type Stats struct {
	Inodes       int
	Chunks       int
	Tombstoned   int
	LogicalBytes uint64 // Σ live file sizes
	StoredBytes  uint64 // Σ over live chunks of ShardLen(size,k)·(k+m)
	TrashInodes  int    // inodes in the trash: the deleted roots and everything under them
	TrashBytes   uint64 // Σ file sizes in the trash
}

// copyInode is the read API's view of an inode: every field but the Children map, which
// is the directory's live name table and would cost O(entries) per LOOKUP or GETATTR to
// copy (docs/02 §Reads). Names are read through Lookup and ReadDir.
func copyInode(in *Inode) Inode {
	cp := *in
	cp.Children = nil
	return cp
}

func copyChunk(c *Chunk) Chunk {
	cp := *c
	cp.Placement = append([]NodeIdx(nil), c.Placement...)
	return cp
}

// Volume returns the volume policy.
func (f *FSM) Volume() Volume {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st.Vol
}

// Meta returns the cluster metadata record.
func (f *FSM) Meta() Meta {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st.Meta
}

// LastNow returns the monotonic FSM clock.
func (f *FSM) LastNow() int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st.LastNow
}

// Applied returns the last applied log index.
func (f *FSM) Applied() uint64 { return f.applied.Load() }

// GetInode returns a copy of a live inode: a deleted root and everything under it are
// not found, so a gateway's handle to a deleted file is stale (I-17, I-155).
func (f *FSM) GetInode(ino Ino) (Inode, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, ok := f.st.Inodes[ino]
	if !ok || f.st.lineage(ino).inTrash {
		return Inode{}, false
	}
	return copyInode(in), true
}

// GetInodeAny returns a copy of the inode whether it is live or in the trash: the
// flusher commits into a trashed file and the browser UI reads one (docs/02 §Reads).
func (f *FSM) GetInodeAny(ino Ino) (Inode, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, ok := f.st.Inodes[ino]
	if !ok {
		return Inode{}, false
	}
	return copyInode(in), true
}

// TrashOf lists the deleted roots under parent, newest first then by inode.
func (f *FSM) TrashOf(parent Ino) []Inode {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.trashList(f.st.Trash[parent])
}

// Trash lists every deleted root, newest first then by inode.
func (f *FSM) Trash() []Inode {
	f.mu.RLock()
	defer f.mu.RUnlock()
	all := map[Ino]struct{}{}
	for _, set := range f.st.Trash {
		for ino := range set {
			all[ino] = struct{}{}
		}
	}
	return f.trashList(all)
}

func (f *FSM) trashList(set map[Ino]struct{}) []Inode {
	out := make([]Inode, 0, len(set))
	for ino := range set {
		out = append(out, copyInode(f.st.Inodes[ino]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeletedAt != out[j].DeletedAt {
			return out[i].DeletedAt > out[j].DeletedAt
		}
		return out[i].Ino < out[j].Ino
	})
	return out
}

// TrashDue counts the deleted roots a Purge{Before: before} would take.
func (f *FSM) TrashDue(before int64) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	n := 0
	for _, set := range f.st.Trash {
		for ino := range set {
			if f.st.Inodes[ino].DeletedAt < before {
				n++
			}
		}
	}
	return n
}

// ResolveFrom walks path inside the deleted root; an empty path is the root itself.
// root must be a deleted root (ErrNotExist otherwise).
func (f *FSM) ResolveFrom(root Ino, path []string) (Inode, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	cur, ok := f.st.Inodes[root]
	if !ok || cur.DeletedAt == 0 {
		return Inode{}, ErrNotExist
	}
	for _, name := range path {
		if cur.Type != TDir {
			return Inode{}, ErrNotDir
		}
		ino, ok := cur.Children[name]
		if !ok {
			return Inode{}, ErrNotExist
		}
		cur = f.st.Inodes[ino]
	}
	return copyInode(cur), nil
}

// Lookup resolves name inside directory parent.
func (f *FSM) Lookup(parent Ino, name string) (Inode, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p, ok := f.st.Inodes[parent]
	if !ok || p.Type != TDir {
		return Inode{}, false
	}
	ino, ok := p.Children[name]
	if !ok {
		return Inode{}, false
	}
	return copyInode(f.st.Inodes[ino]), true
}

// Resolve walks path from the root. An empty path is the root.
func (f *FSM) Resolve(path []string) (Inode, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	cur := f.st.Inodes[RootIno]
	for _, name := range path {
		if cur.Type != TDir {
			return Inode{}, ErrNotDir
		}
		ino, ok := cur.Children[name]
		if !ok {
			return Inode{}, ErrNotExist
		}
		cur = f.st.Inodes[ino]
	}
	return copyInode(cur), nil
}

// PathOf returns the path components from the root to ino.
func (f *FSM) PathOf(ino Ino) ([]string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, ok := f.st.Inodes[ino]
	if !ok {
		return nil, ErrNotExist
	}
	var rev []string
	for in.Ino != RootIno {
		rev = append(rev, in.Name)
		p, ok := f.st.Inodes[in.Parent]
		if !ok || len(rev) > len(f.st.Inodes) {
			return nil, ErrNotExist
		}
		in = p
	}
	path := make([]string, len(rev))
	for i, s := range rev {
		path[len(rev)-1-i] = s
	}
	return path, nil
}

// ReadDir returns the directory's entries sorted by name.
func (f *FSM) ReadDir(ino Ino) ([]Inode, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	d, ok := f.st.Inodes[ino]
	if !ok {
		return nil, ErrNotExist
	}
	if d.Type != TDir {
		return nil, ErrNotDir
	}
	names := make([]string, 0, len(d.Children))
	for n := range d.Children {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Inode, 0, len(names))
	for _, n := range names {
		out = append(out, copyInode(f.st.Inodes[d.Children[n]]))
	}
	return out, nil
}

// GetChunk returns a copy of the chunk entry.
func (f *FSM) GetChunk(h Hash) (Chunk, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	c, ok := f.st.Chunks[h]
	if !ok {
		return Chunk{}, false
	}
	return copyChunk(c), true
}

// ForEachChunk calls fn for every chunk present when the walk starts, in hash order,
// until fn returns false. The keys are sorted once; entries are copied under the read
// lock in batches of 1000 so Apply is never starved (an entry deleted meanwhile is
// skipped, one added meanwhile is not visited).
func (f *FSM) ForEachChunk(fn func(Chunk) bool) {
	f.mu.RLock()
	keys := make([]Hash, 0, len(f.st.Chunks))
	for h := range f.st.Chunks {
		keys = append(keys, h)
	}
	f.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	const batchSize = 1000
	for start := 0; start < len(keys); start += batchSize {
		end := min(start+batchSize, len(keys))
		batch := make([]Chunk, 0, end-start)
		f.mu.RLock()
		for _, h := range keys[start:end] {
			if c, ok := f.st.Chunks[h]; ok {
				batch = append(batch, copyChunk(c))
			}
		}
		f.mu.RUnlock()
		for i := range batch {
			if !fn(batch[i]) {
				return
			}
		}
	}
}

// Node returns the record at idx.
func (f *FSM) Node(idx NodeIdx) (Node, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	n, ok := f.st.Nodes[idx]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// ForEachNode calls fn for every node record, in index order, until fn returns false.
// The records are copied under the read lock and fn runs with the lock released: a
// callback is free to read the FSM again (a nested read lock behind a queued Apply would
// otherwise deadlock the node, as the liveness scan once did).
func (f *FSM) ForEachNode(fn func(Node) bool) {
	f.mu.RLock()
	nodes := make([]Node, 0, len(f.st.Nodes))
	for _, n := range f.st.Nodes {
		nodes = append(nodes, *n)
	}
	f.mu.RUnlock()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Idx < nodes[j].Idx })
	for i := range nodes {
		if !fn(nodes[i]) {
			return
		}
	}
}

// NodeByID returns the record with the given raft ID that is not Removed.
func (f *FSM) NodeByID(id string) (Node, bool) {
	return f.findNode(func(n *Node) bool { return n.ID == id && n.State != Removed })
}

// NodeByAddr returns the device record at addr that is not Removed (the RPC authorizer's
// member lookup, docs/08).
func (f *FSM) NodeByAddr(addr string) (Node, bool) {
	return f.findNode(func(n *Node) bool { return n.Kind == KindDevice && n.State != Removed && n.Addr == addr })
}

func (f *FSM) findNode(pred func(*Node) bool) (Node, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var found *Node
	for _, n := range f.st.Nodes {
		if pred(n) && (found == nil || n.Idx < found.Idx) { // deterministic when several match
			found = n
		}
	}
	if found == nil {
		return Node{}, false
	}
	return *found, true
}

// Nodes returns all node records sorted by Idx.
func (f *FSM) Nodes() []Node {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Node, 0, len(f.st.Nodes))
	for _, n := range f.st.Nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
	return out
}

// Stats returns the volume statistics. They are kept in step with every apply, so FSSTAT
// (which macOS issues constantly) costs O(1) rather than a walk of every inode and chunk.
func (f *FSM) Stats() Stats {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s := f.stats
	s.Inodes = len(f.st.Inodes)
	return s
}

// computeStats walks the state (Restore, tests); the applier keeps the same totals
// incrementally.
func computeStats(st *State) Stats {
	var s Stats
	s.Inodes = len(st.Inodes)
	trashed := map[Ino]bool{}
	for _, set := range st.Trash {
		for ino := range set {
			n, size := st.subtreeStats(st.Inodes[ino])
			s.TrashInodes += n
			s.TrashBytes += size
			var mark func(*Inode)
			mark = func(x *Inode) {
				trashed[x.Ino] = true
				for _, child := range x.Children {
					mark(st.Inodes[child])
				}
			}
			mark(st.Inodes[ino])
		}
	}
	for ino, in := range st.Inodes {
		if in.Type == TFile && !trashed[ino] {
			s.LogicalBytes += in.Size
		}
	}
	for _, c := range st.Chunks {
		if c.Refs == 0 {
			s.Tombstoned++
			continue
		}
		s.Chunks++
		s.StoredBytes += storedBytes(c)
	}
	return s
}

// storedBytes is what a live chunk occupies across its shards.
func storedBytes(c *Chunk) uint64 {
	return uint64(ec.ShardLen(int(c.Size), int(c.K))) * uint64(int(c.K)+int(c.M))
}

// chunkIn counts a chunk entry in its current state; chunkOut removes it from the state
// it was in (live or tombstoned).
func (s *Stats) chunkIn(c *Chunk) {
	if c.Refs == 0 {
		s.Tombstoned++
		return
	}
	s.Chunks++
	s.StoredBytes += storedBytes(c)
}

func (s *Stats) chunkOut(c *Chunk, wasLive bool) {
	if !wasLive {
		s.Tombstoned--
		return
	}
	s.Chunks--
	s.StoredBytes -= storedBytes(c)
}

// Usage is the one capacity formula behind FSSTAT and `status`: total = Σ Active capacity
// · k/(k+m), used = stored bytes · k/(k+m), free = total − used (0 when over).
func Usage(vol Volume, nodes []Node, st Stats) (total, used, free uint64) {
	var capacity uint64
	for i := range nodes {
		if nodes[i].State == Active && nodes[i].Kind != KindDevice { // a device stores nothing (docs/14 §9)
			capacity += nodes[i].Capacity
		}
	}
	k, km := uint64(vol.K), uint64(vol.K)+uint64(vol.M)
	if km == 0 {
		k, km = 1, 1
	}
	total = capacity * k / km
	used = st.StoredBytes * k / km
	if total > used {
		free = total - used
	}
	return total, used, free
}
