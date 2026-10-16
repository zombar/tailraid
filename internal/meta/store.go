package meta

import (
	"bytes"
	"maps"
	"slices"
	"sort"
	"strings"

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

	cls *classTable // the placement classes, kept with the totals (I-377); never copied out
	due *dueIndex   // the GC and purge queues (I-381, I-382); never copied out
	lib *libraries  // the library directories (I-384); never copied out
}

// copyInode is the read API's view of an inode: every field but the Children map, which
// is the directory's live name table and would cost O(entries) per LOOKUP or GETATTR to
// copy (docs/02 §Reads). Names are read through Lookup and ReadDir.
func copyInode(in *Inode) Inode {
	cp := *in
	cp.Children = nil
	cp.Chunks = slices.Clip(in.Chunks) // an append by the caller reallocates, never writes where the FSM grows the list (I-385)
	return cp
}

func copyChunk(c *Chunk) Chunk {
	cp := *c
	cp.Placement = append([]NodeIdx(nil), c.Placement...)
	if c.Sums != nil {
		cp.Sums = append([]Hash(nil), c.Sums...)
	}
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

// EffectiveLibrary is the kind of the nearest flagged ancestor of ino, its own first;
// none for a missing inode or one with no flagged ancestor (docs/18 §9, I-210).
func (f *FSM) EffectiveLibrary(ino Ino) LibraryKind {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, ok := f.st.Inodes[ino]
	for n := 0; ok && n <= len(f.st.Inodes); n++ {
		if k := in.Library(); k != LibraryNone {
			return k
		}
		if in.Ino == RootIno || in.Parent == 0 {
			break
		}
		in, ok = f.st.Inodes[in.Parent]
	}
	return LibraryNone
}

// Library is a flagged folder the Media tab lists.
type Library struct {
	Ino  Ino
	Kind LibraryKind
	Path string
}

// Libraries lists every live flagged folder in kind-then-path order, the same on every
// node (docs/18 §9, I-210); a flagged folder in the trash is left out.
func (f *FSM) Libraries() []Library {
	f.mu.RLock()
	var out []Library
	for ino := range f.stats.lib.dirs { // the flagged directories, never every inode (I-384)
		if f.st.lineage(ino).inTrash {
			continue
		}
		in := f.st.Inodes[ino]
		parts, err := f.pathLocked(ino)
		if err != nil {
			continue
		}
		out = append(out, Library{Ino: ino, Kind: in.Library(), Path: "/" + strings.Join(parts, "/")})
	}
	f.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Path < out[j].Path
	})
	return out
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

// Protected reports whether ino or a directory above it carries FlagProtected (I-154):
// what a gateway asks before it marks an entry for a delete it cannot report (docs/15).
func (f *FSM) Protected(ino Ino) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.st.lineage(ino).protected
}

// TrashDue reports whether a Purge{Before: before} would take a root: the front of the
// root queue, O(1) (I-382).
func (f *FSM) TrashDue(before int64) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.stats.due.roots.front()
	return ok && e.at < before
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

// LookupFold is the name of parent's entry that differs from name only by case, the
// first by sorted name when several do (docs/15 §5, I-267): one pass over the folder's
// names under the read lock, nothing copied, so a caseless lookup costs no listing.
func (f *FSM) LookupFold(parent Ino, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	p, ok := f.st.Inodes[parent]
	if !ok || p.Type != TDir {
		return "", false
	}
	if _, ok := p.Children[name]; ok {
		return name, true
	}
	best := ""
	for n := range p.Children {
		if strings.EqualFold(n, name) && (best == "" || n < best) {
			best = n
		}
	}
	return best, best != ""
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
	return f.pathLocked(ino)
}

// pathLocked is PathOf under the caller's read lock.
func (f *FSM) pathLocked(ino Ino) ([]string, error) {
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
	walkBatches(f, keys, func(h Hash) (Chunk, bool) {
		c, ok := f.st.Chunks[h]
		if !ok {
			return Chunk{}, false
		}
		return copyChunk(c), true
	}, fn)
}

// ForEachInode calls fn for every inode present when the walk starts, in inode order,
// until fn returns false, by ForEachChunk's rules: keys sorted once, copies taken in
// batches of 1000 under the read lock so Apply is never starved (an inode deleted
// meanwhile is skipped, one added meanwhile is not visited). Entries in the trash are
// visited too: GetInode tells the caller which are live (docs/18 §5).
func (f *FSM) ForEachInode(fn func(Inode) bool) {
	f.mu.RLock()
	keys := make([]Ino, 0, len(f.st.Inodes))
	for ino := range f.st.Inodes {
		keys = append(keys, ino)
	}
	f.mu.RUnlock()
	slices.Sort(keys)
	walkBatches(f, keys, func(ino Ino) (Inode, bool) {
		in, ok := f.st.Inodes[ino]
		if !ok {
			return Inode{}, false
		}
		return copyInode(in), true
	}, fn)
}

// walkBatches copies the entries of the sorted keys in batches of 1000 under the read
// lock and calls fn for each with the lock released, until fn returns false.
func walkBatches[K, V any](f *FSM, keys []K, get func(K) (V, bool), fn func(V) bool) {
	const batchSize = 1000
	for start := 0; start < len(keys); start += batchSize {
		end := min(start+batchSize, len(keys))
		batch := make([]V, 0, end-start)
		f.mu.RLock()
		for _, k := range keys[start:end] {
			if v, ok := get(k); ok {
				batch = append(batch, v)
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

// Snapshots returns the snapshot catalogue in ID order (docs/16 §5): every mirror's rows,
// online or not, Deleting ones included (callers that list for a user skip them).
func (f *FSM) Snapshots() []Snapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	ids := slices.Sorted(maps.Keys(f.st.Snaps))
	out := make([]Snapshot, len(ids))
	for i, id := range ids {
		out[i] = *f.st.Snaps[id]
	}
	return out
}

// SnapshotRow returns one catalogue row (FSM.Snapshot is Raft's).
func (f *FSM) SnapshotRow(id SnapID) (Snapshot, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	sn, ok := f.st.Snaps[id]
	if !ok {
		return Snapshot{}, false
	}
	return *sn, true
}

// Stats returns the volume statistics. They are kept in step with every apply, so FSSTAT
// (which macOS issues constantly) costs O(1) rather than a walk of every inode and chunk.
func (f *FSM) Stats() Stats {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s := f.stats
	s.Inodes = len(f.st.Inodes)
	s.cls, s.due, s.lib = nil, nil, nil
	return s
}

// computeStats walks the state (Restore, tests); the applier keeps the same totals
// incrementally.
func computeStats(st *State) Stats {
	s := Stats{cls: buildClasses(st), due: buildDue(st), lib: buildLibraries(st)}
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
	s.cls.in(c)
	s.Chunks++
	s.StoredBytes += storedBytes(c)
}

func (s *Stats) chunkOut(c *Chunk, wasLive bool) {
	if !wasLive {
		s.Tombstoned--
		return
	}
	if c.Refs == 0 { // out of the class it was in while live: its slots have not changed
		live := *c
		live.Refs = 1
		s.cls.out(&live)
	} else {
		s.cls.out(c)
	}
	s.Chunks--
	s.StoredBytes -= storedBytes(c)
}

// Usage is the one capacity formula behind FSSTAT and `status`: total = Σ Active capacity
// · k/(k+m), used = stored bytes · k/(k+m), free = total − used (0 when over).
func Usage(vol Volume, nodes []Node, st Stats) (total, used, free uint64) {
	var capacity uint64
	for i := range nodes {
		if nodes[i].State == Active && nodes[i].Kind.Target() { // a device stores nothing (docs/14 §9), a mirror holds snapshots (docs/16)
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
