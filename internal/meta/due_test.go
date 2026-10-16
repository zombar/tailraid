package meta

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"math"
	"runtime"
	"slices"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/testseed"
)

// raw applies c without the per-step checks of tfsm.apply (each a walk of the state), so
// a test can build a backlog of tens of thousands of entries.
func (tf *tfsm) raw(c Command) Result {
	tf.t.Helper()
	tf.idx++
	res := tf.f.Apply(&raft.Log{Index: tf.idx, Type: raft.LogCommand, Data: MustEncodeCommand(c)}).(Result)
	if res.Err != nil {
		tf.t.Fatalf("op %d: %v", c.Op, res.Err)
	}
	return res
}

// allocBytes is what f allocates on the heap.
func allocBytes(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

const backlog = 50_000

// A GCDelete of a small batch costs the same behind fifty thousand due tombstones as
// behind none: it never gathers every due entry to keep the lowest hashes (docs/02
// §Apply rules, I-381).
// Invariant: I-381
func TestFSM_GCDelete_CostIndependentOfBacklog(t *testing.T) {
	tf := newTFSM(t)
	ino := tf.create(RootIno, "f")
	writes := make([]ChunkWrite, backlog)
	news := make([]ChunkDesc, backlog)
	for i := range backlog {
		h := hashN(uint64(i) + 1)
		writes[i], news[i] = ChunkWrite{Idx: uint32(i), Hash: h}, desc(h)
	}
	tf.raw(Command{Op: OpCommit, Commit: &Commit{Ino: ino, NewSize: backlog * testCS, Writes: writes, NewChunks: news, Now: 1}})
	tf.raw(Command{Op: OpCommit, Commit: &Commit{Ino: ino, BaseVersion: 1, HasTruncate: true, NewSize: 0, Now: 10}})
	r := tf.raw(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 11, Limit: 16}})
	if len(r.Victims) != 16 {
		t.Fatalf("%d victims, want 16", len(r.Victims))
	}
	// the first entry moves every due tombstone into the hash queue once; the next is
	// the steady cost, which must not scale with the backlog
	n := allocBytes(func() { r = tf.raw(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 11, Limit: 16}}) })
	if len(r.Victims) != 16 || n > 64<<10 {
		t.Fatalf("a 16-victim GCDelete behind %d due tombstones allocated %d bytes (%d victims)", backlog, n, len(r.Victims))
	}
	t.Logf("a 16-victim GCDelete behind %d due tombstones: %d bytes", backlog, n)
	if v := CheckStats(tf.f.state(), tf.f.stats); v != nil {
		t.Fatal(v)
	}
}

// A Purge of one root costs the same behind fifty thousand due roots as behind one: it
// never gathers and sorts every due root (I-382).
// Invariant: I-382
func TestFSM_Purge_CostIndependentOfBacklog(t *testing.T) {
	tf := newTFSM(t)
	for i := range backlog {
		name := fmt.Sprintf("f%d", i)
		tf.raw(Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: name, Type: TFile, Mode: 0o644, Now: 1}})
		tf.raw(Command{Op: OpTrash, Trash: &Trash{Parent: RootIno, Name: name, Now: int64(2 + i)}})
	}
	was := tf.f.Stats().TrashInodes
	n := allocBytes(func() { tf.raw(Command{Op: OpPurge, Purge: &Purge{Before: 1 << 40, Limit: 1, Now: 1 << 40}}) })
	if got := was - tf.f.Stats().TrashInodes; got != 1 || n > 64<<10 {
		t.Fatalf("a one-root Purge behind %d due roots allocated %d bytes (purged %d)", backlog, n, got)
	}
	t.Logf("a one-root Purge behind %d due roots: %d bytes", backlog, n)
	if v := CheckStats(tf.f.state(), tf.f.stats); v != nil {
		t.Fatal(v)
	}
}

// replayAgainstWalk runs random command sequences and, before each entry op would take
// from the queues, works out what the walk the rule describes takes from the same state;
// check compares the two.
func replayAgainstWalk(t *testing.T, op Op, check func(step int, c Command, st, after *State, r Result)) {
	rng := testseed.Seed(t)
	f := NewFSM()
	gen := &cmdGen{rng: rng, f: f, now: 1000}
	idx := uint64(1)
	f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: MustEncodeCommand(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})})
	seen := 0
	for step := range 6000 {
		c := gen.next()
		var before *State
		if c.Op == op {
			before = f.state().Clone()
		}
		idx++
		r := f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: MustEncodeCommand(c)}).(Result)
		if before != nil && r.Err == nil {
			check(step, c, before, f.state(), r)
			seen++
		}
		if v := CheckStats(f.state(), f.stats); v != nil {
			t.Fatalf("step %d op %d: %v", step, c.Op, v)
		}
	}
	if seen < 50 {
		t.Fatalf("only %d entries of op %d checked", seen, op)
	}
}

// The victims a GCDelete takes from its queues are the walk's: every tombstone older
// than Before (after the sync hold), lowest hashes first, at most Limit, over random
// sequences of commits, purges, snapshots holding GC and re-references (I-381, I-103).
// Invariant: I-381
func TestFSM_GCDelete_QueueMatchesWalk(t *testing.T) {
	replayAgainstWalk(t, OpGCDelete, func(step int, c Command, st, _ *State, r Result) {
		before := c.GCDelete.Before
		if at, ok := st.syncHold(); ok && at < before {
			before = at
		}
		var want []Hash
		for h := range st.Tombs {
			if st.Chunks[h].TombstonedAt < before {
				want = append(want, h)
			}
		}
		slices.SortFunc(want, func(a, b Hash) int { return bytes.Compare(a[:], b[:]) })
		if l := int(c.GCDelete.Limit); l != 0 && len(want) > l {
			want = want[:l]
		}
		got := make([]Hash, len(r.Victims))
		for i, v := range r.Victims {
			got[i] = v.Hash
		}
		if !slices.Equal(got, want) {
			t.Fatalf("step %d: GCDelete %+v took %d victims, the walk %d", step, *c.GCDelete, len(got), len(want))
		}
	})
}

// A Purge with Before deletes what the walk's roots in (DeletedAt, Ino) order would:
// the same inodes leave, the same roots stay, the same chunks are tombstoned (I-382,
// I-153).
// Invariant: I-382
func TestFSM_Purge_QueueMatchesWalk(t *testing.T) {
	replayAgainstWalk(t, OpPurge, func(step int, c Command, st, got *State, _ Result) {
		p := c.Purge
		if p.Ino != 0 {
			return
		}
		var roots []*Inode
		for _, set := range st.Trash {
			for ino := range set {
				if in := st.Inodes[ino]; in.DeletedAt < p.Before {
					roots = append(roots, in)
				}
			}
		}
		slices.SortFunc(roots, func(a, b *Inode) int { return cmp.Or(cmp.Compare(a.DeletedAt, b.DeletedAt), cmp.Compare(a.Ino, b.Ino)) })
		stats := computeStats(st)
		a := &applier{st: st, stats: &stats, feed: &changeFeed{max: FeedMax}} // a feed of its own: the purge records what it deletes
		limit := int(p.Limit)
		if limit == 0 {
			limit = math.MaxInt
		}
		if err := a.deleteAll(a.collect(func() *Inode {
			if len(roots) == 0 {
				return nil
			}
			r := roots[0]
			roots = roots[1:]
			return r
		}, limit), p.Now); err != nil {
			t.Fatalf("step %d: the walk's purge: %v", step, err)
		}
		if !slices.Equal(slices.Sorted(maps.Keys(got.Inodes)), slices.Sorted(maps.Keys(st.Inodes))) || !maps.EqualFunc(got.Trash, st.Trash, maps.Equal) || !maps.Equal(got.Tombs, st.Tombs) {
			t.Fatalf("step %d: Purge %+v left another state than the walk's", step, *p)
		}
	})
}
