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
	LogicalBytes uint64 // Σ file sizes
	StoredBytes  uint64 // Σ over live chunks of ShardLen(size,k)·(k+m)
}

func copyInode(in *Inode) Inode {
	cp := *in
	if in.Children != nil {
		cp.Children = make(map[string]Ino, len(in.Children))
		for k, v := range in.Children {
			cp.Children[k] = v
		}
	}
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

// GetInode returns a copy of the inode.
func (f *FSM) GetInode(ino Ino) (Inode, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	in, ok := f.st.Inodes[ino]
	if !ok {
		return Inode{}, false
	}
	return copyInode(in), true
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

// ForEachChunk calls fn for every chunk in hash order until fn returns false.
// The read lock is held per batch of 1000 so Apply is never starved.
func (f *FSM) ForEachChunk(fn func(Chunk) bool) {
	var after *Hash
	for {
		f.mu.RLock()
		keys := make([]Hash, 0, len(f.st.Chunks))
		for h := range f.st.Chunks {
			if after == nil || bytes.Compare(h[:], after[:]) > 0 {
				keys = append(keys, h)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
		if len(keys) > 1000 {
			keys = keys[:1000]
		}
		batch := make([]Chunk, len(keys))
		for i, h := range keys {
			batch[i] = copyChunk(f.st.Chunks[h])
		}
		f.mu.RUnlock()
		if len(batch) == 0 {
			return
		}
		for i := range batch {
			if !fn(batch[i]) {
				return
			}
		}
		last := keys[len(keys)-1]
		after = &last
	}
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

// Stats computes volume statistics.
func (f *FSM) Stats() Stats {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var s Stats
	s.Inodes = len(f.st.Inodes)
	for _, in := range f.st.Inodes {
		if in.Type == TFile {
			s.LogicalBytes += in.Size
		}
	}
	for _, c := range f.st.Chunks {
		if c.Refs == 0 {
			s.Tombstoned++
			continue
		}
		s.Chunks++
		s.StoredBytes += uint64(ec.ShardLen(int(c.Size), int(c.K))) * uint64(int(c.K)+int(c.M))
	}
	return s
}
