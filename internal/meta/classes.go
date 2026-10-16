package meta

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"

	"github.com/zombar/tailraid/internal/ec"
)

// A placement class is the live chunks that share a policy and a set of slots (docs/06
// §4a, I-377). Everything ChunkState derives for a chunk without shard reports (its bad
// slots, readable count, pending migration and host skew) depends on nothing else, so
// `status` judges a class once instead of every chunk in it (I-378), and the repair pass
// visits only the chunks of classes that need work (I-379). The number of classes is the
// number of distinct slot sets in use, which the targets bound, not the volume.
type Class struct {
	Key       string    // K, M, the small bit and the sorted slots, as bytes
	K, M      uint8     // the chunks' policy
	Small     bool      // k = 1 chunks below the small-chunk bound: they follow Vol.M only (TargetPolicy)
	Placement []NodeIdx // the slots, sorted, 0 (a missing slot) first
	Chunks    int       // live chunks in the class
	Shard     uint64    // Σ over them of one shard's length: what each slot of the class holds
}

// classTable is the applier's: kept in step with every apply (Stats.chunkIn, chunkOut and
// setPlacement) and rebuilt on Restore; never encoded.
type classTable struct {
	st    *State // for the chunk size the small bit is judged by
	byKey map[string]*classEntry
}

type classEntry struct {
	Class
	members map[Hash]struct{}
}

func newClassTable(st *State) *classTable {
	return &classTable{st: st, byKey: map[string]*classEntry{}}
}

// classOf is c's class key and its sorted slots under a volume of chunk size cs.
func classOf(c *Chunk, cs uint32) (key string, place []NodeIdx, small bool) {
	small = c.K == 1 && IsSmallChunk(c.Size, cs)
	place = slices.Clone(c.Placement)
	slices.Sort(place)
	b := make([]byte, 3, 3+2*len(place))
	b[0], b[1] = c.K, c.M
	if small {
		b[2] = 1
	}
	for _, n := range place {
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	}
	return string(b), place, small
}

// ClassKey is the key of the class live chunk c belongs to in a volume of chunk size cs.
func ClassKey(c *Chunk, cs uint32) string {
	key, _, _ := classOf(c, cs)
	return key
}

// in counts live chunk c in its class; out removes it. Tombstoned chunks are in no class.
func (t *classTable) in(c *Chunk) {
	if t == nil || c.Refs == 0 {
		return
	}
	key, place, small := classOf(c, t.st.Vol.ChunkSize)
	e := t.byKey[key]
	if e == nil {
		e = &classEntry{Key: key, K: c.K, M: c.M, Small: small, Placement: place, members: map[Hash]struct{}{}}
		t.byKey[key] = e
	}
	e.Chunks++
	e.Shard += uint64(ec.ShardLen(int(c.Size), int(c.K)))
	e.members[c.Hash] = struct{}{}
}

func (t *classTable) out(c *Chunk) {
	if t == nil || c.Refs == 0 {
		return
	}
	key, _, _ := classOf(c, t.st.Vol.ChunkSize)
	e := t.byKey[key]
	if e == nil {
		return
	}
	e.Chunks--
	e.Shard -= uint64(ec.ShardLen(int(c.Size), int(c.K)))
	delete(e.members, c.Hash)
	if e.Chunks == 0 {
		delete(t.byKey, key)
	}
}

func buildClasses(st *State) *classTable {
	t := newClassTable(st)
	for _, c := range st.Chunks {
		t.in(c)
	}
	return t
}

// Classes lists the placement classes in key order: O(classes), never O(chunks) (I-378).
func (f *FSM) Classes() []Class {
	f.mu.RLock()
	out := make([]Class, 0, len(f.stats.cls.byKey))
	for _, e := range f.stats.cls.byKey {
		c := e.Class
		c.Placement = slices.Clone(e.Placement)
		out = append(out, c)
	}
	f.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ClassMembers lists the hashes of the live chunks in class key, in hash order; none for a
// class that no longer exists.
func (f *FSM) ClassMembers(key string) []Hash {
	f.mu.RLock()
	e := f.stats.cls.byKey[key]
	var out []Hash
	if e != nil {
		out = make([]Hash, 0, len(e.members))
		for h := range e.members {
			out = append(out, h)
		}
	}
	f.mu.RUnlock()
	slices.SortFunc(out, func(a, b Hash) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// Representative is a chunk of the class for ChunkState: its policy, its slots and a size
// with the class's small bit, everything the derivation reads (I-378).
func (c Class) Representative(vol Volume) Chunk {
	size := vol.ChunkSize
	if c.Small {
		size = 1
	}
	return Chunk{K: c.K, M: c.M, Size: size, Refs: 1, Placement: slices.Clone(c.Placement)}
}

func classesDrift(want, kept *classTable) []string {
	if kept == nil {
		return []string{"no class table kept"}
	}
	if len(kept.byKey) != len(want.byKey) {
		return []string{fmt.Sprintf("class count drifted: kept %d, recount %d", len(kept.byKey), len(want.byKey))}
	}
	for key, k := range kept.byKey {
		w := want.byKey[key]
		switch {
		case w == nil:
			return []string{fmt.Sprintf("class %x kept with no chunks in the state", key)}
		case k.Chunks != w.Chunks || k.Shard != w.Shard || k.K != w.K || k.M != w.M || k.Small != w.Small || !slices.Equal(k.Placement, w.Placement):
			return []string{fmt.Sprintf("class %x drifted: kept %+v, recount %+v", key, k.Class, w.Class)}
		case len(k.members) != len(w.members):
			return []string{fmt.Sprintf("class %x members drifted", key)}
		}
		for h := range k.members {
			if _, ok := w.members[h]; !ok {
				return []string{fmt.Sprintf("class %x holds %x, not in the recount", key, h[:4])}
			}
		}
	}
	return nil
}
