package vfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// Invariant: I-7
// Lifecycle: L-26, L-35, L-36, L-39, L-50, L-51
func TestFlusher_SmallAndLargeChunksCommit(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	data := testharness.Pattern(int(cs)+100, 1) // chunk 0 full (k=2,m=1), chunk 1 = 100 B (k=1,m=1)
	must(t, f.Ov.Write(ino, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	if in.Size != cs+100 || len(in.Chunks) != 2 || in.Version != 1 {
		t.Fatalf("inode %+v", in)
	}
	c0, _ := f.FSM.GetChunk(in.Chunks[0])
	c1, _ := f.FSM.GetChunk(in.Chunks[1])
	if c0.K != 2 || c0.M != 1 || len(f.ShardsOn(in.Chunks[0], 2, 1)) != 3 {
		t.Fatalf("large chunk %+v on %v", c0, f.ShardsOn(in.Chunks[0], 2, 1))
	}
	if c1.K != 1 || c1.M != 1 || len(f.ShardsOn(in.Chunks[1], 1, 1)) != 2 {
		t.Fatalf("small chunk %+v", c1)
	}
	for _, c := range []meta.Chunk{c0, c1} {
		for _, slot := range c.Placement {
			if slot == 0 {
				t.Fatalf("missing slot in %+v", c)
			}
		}
	}
	if len(f.Ov.Dirty()) != 0 {
		t.Fatalf("still dirty: %+v", f.Ov.Dirty())
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatalf("read back differs at %d", testharness.FirstDiff(got, data))
	}
}

// Invariant: I-7
// Lifecycle: L-42, L-155, L-59
func TestFlusher_FewerThanKAcks_StaysDirty(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 2)
	must(t, f.Ov.Write(ino, 0, 0, data))
	f.SetDown(2, true)
	f.SetDown(3, true) // only self is live: 1 ack < k
	err := f.Flusher().FlushInode(ctx, ino)
	if err == nil {
		t.Fatal("flush succeeded with < k acks")
	}
	in := f.Inode(ino)
	if in.Version != 0 || in.Size != 0 {
		t.Fatalf("commit happened: %+v", in)
	}
	d := f.Ov.Dirty()
	if len(d) != 1 || d[0].DirtyBytes != uint64(len(data)) || d[0].Flushing {
		t.Fatalf("dirty state lost: %+v", d)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatal("read-your-writes lost after failed flush")
	}
	// nodes return → the same data flushes
	f.SetDown(2, false)
	f.SetDown(3, false)
	must(t, f.Flusher().FlushInode(ctx, ino))
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatal("content after recovery differs")
	}
}

// Lifecycle: L-41, L-52
func TestFlusher_DegradedCommitRecordsZeroSlots(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 3)
	must(t, f.Ov.Write(ino, 0, 0, data))
	f.SetDown(3, true) // 2 live = k; no spare for re-placement
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	c, _ := f.FSM.GetChunk(in.Chunks[0])
	zeros := 0
	for _, s := range c.Placement {
		switch s {
		case 0:
			zeros++
		case 3:
			t.Fatal("dead node recorded as holder")
		}
	}
	if zeros != 1 {
		t.Fatalf("placement %v", c.Placement)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatal("degraded chunk unreadable")
	}
}

// Lifecycle: L-40
func TestFlusher_FailedSlotReplacedOnce(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 4)
	must(t, f.Ov.Write(ino, 0, 0, data))
	// find which node the placement will pick and make its first PUT fail
	h := sha256Of(data)
	cands := meta.Candidates(f.FSM.Nodes(), func(meta.NodeIdx) bool { return true })
	victim := meta.Place(h, cands, 3)[1]
	f.FailPutOnce(victim, 1)
	must(t, f.Flusher().FlushInode(ctx, ino))
	c, _ := f.FSM.GetChunk(h)
	for _, s := range c.Placement {
		if s == 0 || s == victim {
			t.Fatalf("placement %v should be full and avoid %d", c.Placement, victim)
		}
	}
	if len(f.ShardsOn(h, 2, 1)) != 3 {
		t.Fatalf("shards on %v", f.ShardsOn(h, 2, 1))
	}
}

// Lifecycle: L-44
func TestFlusher_FullNodeExcluded(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 5)
	must(t, f.Ov.Write(ino, 0, 0, data))
	f.SetFull(2, true)
	must(t, f.Flusher().FlushInode(ctx, ino))
	c, _ := f.FSM.GetChunk(sha256Of(data))
	for _, s := range c.Placement {
		if s == 0 || s == 2 {
			t.Fatalf("placement %v", c.Placement)
		}
	}
}

// Lifecycle: L-32, L-152
func TestFlusher_HoleChunksAreHashZero(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 3*cs, []byte("far")))
	must(t, f.Ov.Write(ino, 0, cs, make([]byte, cs))) // explicit zeros are a hole too
	before := f.Puts()
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	if len(in.Chunks) != 4 || in.Chunks[0] != meta.HashZero || in.Chunks[1] != meta.HashZero || in.Chunks[2] != meta.HashZero {
		t.Fatalf("chunks %v", in.Chunks)
	}
	if f.Puts()-before != 2 { // only the 3-byte chunk: k=1,m=1 → 2 PUTs
		t.Fatalf("%d PUTs for holes", f.Puts()-before)
	}
	if _, ok := f.FSM.GetChunk(meta.HashZero); ok {
		t.Fatal("HashZero entry")
	}
}

// Lifecycle: L-33, L-34
// Invariant: I-2
func TestFlusher_DedupLiveAndTombstoned_NoUpload(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 6)
	a := f.Create("a")
	must(t, f.Ov.Write(a, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, a))
	h := f.Inode(a).Chunks[0]
	before := f.Puts()
	b := f.Create("b")
	must(t, f.Ov.Write(b, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, b))
	if f.Puts() != before {
		t.Fatalf("dedup uploaded %d shards", f.Puts()-before)
	}
	if c, _ := f.FSM.GetChunk(h); c.Refs != 2 {
		t.Fatalf("refs %d", c.Refs)
	}
	// tombstone it, then revive by a third write: still no upload
	f.MustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "a", Now: 5}})
	f.MustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "b", Now: 6}})
	if c, _ := f.FSM.GetChunk(h); c.Refs != 0 || c.TombstonedAt == 0 {
		t.Fatal("not tombstoned")
	}
	c := f.Create("c")
	must(t, f.Ov.Write(c, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, c))
	if f.Puts() != before {
		t.Fatal("revive uploaded")
	}
	if ch, _ := f.FSM.GetChunk(h); ch.Refs != 1 || ch.TombstonedAt != 0 {
		t.Fatalf("revived %+v", ch)
	}
}

// Review #6: a dedup hit against an entry with < k live slots fills the slots first.
// Lifecycle: L-146, L-167, L-176
func TestFlusher_DedupDegradedEntry_FillsSlotsBeforeCommit(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 7)
	a := f.Create("a")
	must(t, f.Ov.Write(a, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, a))
	h := f.Inode(a).Chunks[0]
	c, _ := f.FSM.GetChunk(h)
	// two of three holders die; one spare joins
	f.SetDown(c.Placement[1], true)
	f.SetDown(c.Placement[2], true)
	spare := f.AddNode()
	b := f.Create("b")
	must(t, f.Ov.Write(b, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, b))
	c2, _ := f.FSM.GetChunk(h)
	live := 0
	onSpare := false
	for _, s := range c2.Placement {
		if s != 0 && !f.IsDown(s) {
			live++
		}
		if s == spare {
			onSpare = true
		}
	}
	if live < 2 || !onSpare || c2.Refs != 2 {
		t.Fatalf("placement after fill %v refs %d", c2.Placement, c2.Refs)
	}
	ops := f.Ops()
	if ops[len(ops)-2] != meta.OpSetPlacement || ops[len(ops)-1] != meta.OpCommit {
		t.Fatalf("ops %v", ops)
	}
	if got := f.ReadFile(b); !bytes.Equal(got, data) {
		t.Fatal("unreadable after fill")
	}
}

// A live holder may have lost its shard file (disk restored from backup, data dir wiped).
// Liveness alone is not trusted on a dedup hit: with fewer than k slots actually present
// the flusher re-PUTs the absent ones from its own bytes before the Commit; with k or
// more present it dedups as usual.
// Invariant: I-12
// Lifecycle: L-167, L-176, L-183
func TestFlusher_DedupHit_LiveButMissingShards_RePuts(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 21)
	a := f.Create("a")
	must(t, f.Ov.Write(a, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, a))
	h := f.Inode(a).Chunks[0]
	c, _ := f.FSM.GetChunk(h)
	key := func(slot int) chunk.ShardKey { return chunk.Key(h, c.K, c.M, slot) }

	// one holder lost its file: still k present → dedup, no PUT
	if _, err := f.Stores[c.Placement[0]].Delete(key(0)); err != nil {
		t.Fatal(err)
	}
	before := f.Puts()
	cs := uint64(testharness.ChunkSize)
	b := f.Create("b")
	must(t, f.Ov.Write(b, 0, cs, data)) // chunk 1 is the dedup'd chunk; chunk 0 a hole
	must(t, f.Flusher().FlushInode(ctx, b))
	if f.Puts() != before {
		t.Fatalf("dedup with k present slots uploaded %d shards", f.Puts()-before)
	}

	// a second holder lost its file: fewer than k present → fill before the commit
	if _, err := f.Stores[c.Placement[1]].Delete(key(1)); err != nil {
		t.Fatal(err)
	}
	before = f.Puts()
	d := f.Create("d")
	must(t, f.Ov.Write(d, 0, cs, data))
	must(t, f.Flusher().FlushInode(ctx, d))
	if f.Puts()-before != 2 {
		t.Fatalf("fill PUT %d shards, want the 2 absent slots", f.Puts()-before)
	}
	ops := f.Ops()
	if ops[len(ops)-2] != meta.OpSetPlacement || ops[len(ops)-1] != meta.OpCommit {
		t.Fatalf("ops %v, want SetPlacement then Commit", ops[len(ops)-3:])
	}
	c2, _ := f.FSM.GetChunk(h)
	for i, n := range c2.Placement {
		if n == 0 || !f.Stores[n].Has(chunk.Key(h, c2.K, c2.M, i)) {
			t.Fatalf("slot %d node %d not present after fill: %v", i, n, c2.Placement)
		}
	}
	in := f.Inode(d)
	out := make([]byte, len(data))
	if _, err := f.ReaderNoCache().ReadAt(ctx, in, out, int64(cs)); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("other-gateway read differs at %d", testharness.FirstDiff(out, data))
	}
}

// A retried Commit (here after a transport failure of the proposal) re-runs the PUTs:
// the chunk is registered only by the Commit that applies, so the shards are PUT again,
// which refreshes their mtime and keeps the orphan sweep away however late the retry.
// Invariant: I-7, I-14
// Lifecycle: L-58
func TestFlusher_LateRetry_RePutsBeforeCommit(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 22)
	must(t, f.Ov.Write(ino, 0, 8192, data))
	f.ProposeErrOnce(errors.New("leadership lost"))
	if err := f.Flusher().FlushInode(ctx, ino); err == nil {
		t.Fatal("expected the proposal to fail")
	}
	first := f.Puts()
	if first != 3 { // k+m shards of the one non-hole chunk... plus the straddling one
		t.Logf("first attempt PUT %d shards", first)
	}
	f.Clk.Advance(6 * time.Minute) // well past grace/2
	must(t, f.Flusher().FlushInode(ctx, ino))
	if f.Puts() <= first {
		t.Fatalf("retry did not re-PUT: %d PUTs before, %d after", first, f.Puts())
	}
	if f.Puts() != 2*first {
		t.Fatalf("retry PUT %d shards, want every shard again (%d)", f.Puts()-first, first)
	}
	want := append(make([]byte, 8192), data...)
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("differs at %d", testharness.FirstDiff(got, want))
	}
}

// Invariant: I-35
// Lifecycle: L-21, L-53
func TestFlusher_ConflictRebase_PreservesConcurrentAppend(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	base := testharness.Pattern(int(2*cs), 8)
	must(t, f.Ov.Write(ino, 0, 0, base))
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	// this gateway overwrites chunk 0; another gateway appends chunk 2 before our commit
	mine := testharness.Pattern(int(cs), 9)
	must(t, f.Ov.Write(ino, in.Size, 0, mine))
	other := testharness.Pattern(int(cs), 10)
	oh := sha256Of(other)
	var done bool
	fl := f.Flusher()
	fl.Hooks.BeforeCommit = func() {
		if done {
			return
		}
		done = true
		must(t, f.Put(ctx, 1, chunk.Key(oh, 2, 1, 0), other[:cs/2]))
		f.MustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, BaseVersion: in.Version, NewSize: 3 * cs,
			Writes: []meta.ChunkWrite{{Idx: 2, Hash: oh}}, NewChunks: []meta.ChunkDesc{{Hash: oh, Size: uint32(cs), K: 2, M: 1, Placement: []meta.NodeIdx{1, 2, 3}}}, Now: 7}})
	}
	must(t, fl.FlushInode(ctx, ino))
	final := f.Inode(ino)
	if final.Size != 3*cs || len(final.Chunks) != 3 || final.Chunks[0] != sha256Of(mine) || final.Chunks[2] != oh || final.Chunks[1] != in.Chunks[1] {
		t.Fatalf("final %+v", final)
	}
	if len(f.Ov.Dirty()) != 0 {
		t.Fatal("extents left dirty")
	}
}

// Invariant: I-35
// Lifecycle: L-22
func TestFlusher_ConflictRebase_TruncateWins(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, testharness.Pattern(int(2*cs), 8)))
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	must(t, f.Ov.Truncate(ino, in.Size, cs/2))
	other := testharness.Pattern(int(cs), 10)
	oh := sha256Of(other)
	var done bool
	fl := f.Flusher()
	fl.Hooks.BeforeCommit = func() {
		if done {
			return
		}
		done = true
		f.MustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, BaseVersion: in.Version, NewSize: 3 * cs,
			Writes: []meta.ChunkWrite{{Idx: 2, Hash: oh}}, NewChunks: []meta.ChunkDesc{{Hash: oh, Size: uint32(cs), K: 2, M: 1, Placement: []meta.NodeIdx{1, 2, 3}}}, Now: 7}})
	}
	must(t, fl.FlushInode(ctx, ino))
	final := f.Inode(ino)
	if final.Size != cs/2 || len(final.Chunks) != 1 {
		t.Fatalf("truncate did not win: %+v", final)
	}
	if c, _ := f.FSM.GetChunk(oh); c.Refs != 0 {
		t.Fatal("appended chunk still referenced")
	}
}

// Lifecycle: L-54
func TestFlusher_UnknownChunk_ReUpload(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 11)
	a := f.Create("a")
	must(t, f.Ov.Write(a, 0, 0, data))
	must(t, f.Flusher().FlushInode(ctx, a))
	h := f.Inode(a).Chunks[0]
	f.MustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "a", Now: 5}})
	b := f.Create("b")
	must(t, f.Ov.Write(b, 0, 0, data))
	fl := f.Flusher()
	var once bool
	fl.Hooks.BeforeCommit = func() {
		if once {
			return
		}
		once = true
		// GC collects the tombstone between dedup and commit
		r := f.MustApply(meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: 1 << 62}})
		for _, v := range r.Victims {
			for i, n := range v.Placement {
				if n != 0 {
					_ = f.Delete(ctx, n, chunk.Key(v.Hash, 2, 1, i))
				}
			}
		}
	}
	must(t, fl.FlushInode(ctx, b))
	c, ok := f.FSM.GetChunk(h)
	if !ok || c.Refs != 1 {
		t.Fatalf("chunk after re-upload %+v %v", c, ok)
	}
	if len(f.ShardsOn(h, 2, 1)) != 3 {
		t.Fatalf("shards on %v", f.ShardsOn(h, 2, 1))
	}
	if got := f.ReadFile(b); !bytes.Equal(got, data) {
		t.Fatal("content")
	}
}

// Invariant: I-35
// Lifecycle: L-58
func TestFlusher_ProposeFailureRetriesSamePlan(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize)+10, 12)
	must(t, f.Ov.Write(ino, 0, 0, data))
	f.ProposeErrOnce(errors.New("leadership lost"))
	if err := f.Flusher().FlushInode(ctx, ino); err == nil {
		t.Fatal("expected error")
	}
	if d := f.Ov.Dirty(); len(d) != 1 || d[0].Flushing || d[0].DirtyBytes != uint64(len(data)) {
		t.Fatalf("dirty after failure: %+v", d)
	}
	must(t, f.Flusher().FlushInode(ctx, ino))
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatal("content")
	}
}

// Lifecycle: L-55
func TestFlusher_UnlinkedBeforeFlush_Dropped(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), 13)))
	f.MustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "a", Now: 5}})
	before := f.Puts()
	must(t, f.Flusher().FlushInode(ctx, ino))
	if f.Puts() != before {
		t.Fatal("uploaded for a dead inode")
	}
	if _, ok := f.Ov.Size(ino, 0); ok {
		t.Fatal("overlay still tracks the inode")
	}
	if st := f.FSM.Stats(); st.Chunks != 0 {
		t.Fatalf("chunk entries created: %+v", st)
	}
}

// Invariant: I-35
// Lifecycle: L-27
func TestFlusher_PartialFlush_SizeNeverAheadOfData(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	data := testharness.Pattern(int(2*cs+cs/2), 14)
	must(t, f.Ov.Write(ino, 0, 0, data))
	must(t, f.Flusher().FlushPartial(ctx, ino))
	in := f.Inode(ino)
	if in.Size != 2*cs || len(in.Chunks) != 2 {
		t.Fatalf("partial flush committed %+v", in)
	}
	if d := f.Ov.Dirty(); len(d) != 1 || d[0].DirtyBytes != cs/2 {
		t.Fatalf("residual %+v", d)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatal("read-your-writes after partial flush")
	}
	must(t, f.Flusher().FlushInode(ctx, ino))
	if in := f.Inode(ino); in.Size != 2*cs+cs/2 || len(in.Chunks) != 3 {
		t.Fatalf("after full flush %+v", in)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatal("content after full flush")
	}
}

// A partial flush commits only entirely dirty chunks. The bytes of a partly dirty chunk
// below the last committed one (here the head chunk the append starts in) are not
// committed, so they must stay dirty (residual) rather than vanish (L-179).
// Invariant: I-7, I-35
// Lifecycle: L-27, L-179
func TestFlusher_PartialFlush_UnalignedAppend_KeepsHeadChunk(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	head := testharness.Pattern(int(cs+cs/2), 17)
	must(t, f.Ov.Write(ino, 0, 0, head))
	must(t, f.Flusher().FlushInode(ctx, ino))
	base := f.Inode(ino)
	tail := testharness.Pattern(int(40*cs), 18)
	must(t, f.Ov.Write(ino, base.Size, base.Size, tail)) // append starts mid-chunk 1
	want := append(append([]byte{}, head...), tail...)
	must(t, f.Flusher().FlushPartial(ctx, ino))
	in := f.Inode(ino)
	if in.Size != 41*cs || len(in.Chunks) != 41 {
		t.Fatalf("partial flush committed size %d, %d chunks", in.Size, len(in.Chunks))
	}
	// residual: the dirty half of chunk 1 and the dirty half of chunk 41
	if d := f.Ov.Dirty(); len(d) != 1 || d[0].DirtyBytes != cs {
		t.Fatalf("residual %+v, want %d dirty bytes", d, cs)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("read-your-writes after partial flush differs at %d", testharness.FirstDiff(got, want))
	}
	f.RestartOverlay()
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("after restart differs at %d", testharness.FirstDiff(got, want))
	}
	must(t, f.Flusher().FlushInode(ctx, ino))
	if d := f.Ov.Dirty(); len(d) != 0 {
		t.Fatalf("still dirty after full flush: %+v", d)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("after full flush differs at %d", testharness.FirstDiff(got, want))
	}
	if in := f.Inode(ino); in.Size != uint64(len(want)) || len(in.Chunks) != 42 {
		t.Fatalf("final inode %+v", in)
	}
}

// Two extents with a gap: the second starts mid-chunk. The partially dirty chunk between
// them is below the last committed chunk and must stay dirty (L-179), while the untouched
// gap becomes holes.
// Invariant: I-7, I-35
// Lifecycle: L-27, L-179
func TestFlusher_PartialFlush_TwoExtents_KeepsPartialChunks(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	first := testharness.Pattern(int(10*cs), 19)
	second := testharness.Pattern(int(60*cs-(20*cs+cs/2)), 20)
	must(t, f.Ov.Write(ino, 0, 0, first))
	must(t, f.Ov.Write(ino, 0, 20*cs+cs/2, second))
	want := make([]byte, 60*cs)
	copy(want, first)
	copy(want[20*cs+cs/2:], second)
	must(t, f.Flusher().FlushPartial(ctx, ino))
	in := f.Inode(ino)
	if in.Size != 60*cs || len(in.Chunks) != 60 {
		t.Fatalf("partial flush committed size %d, %d chunks", in.Size, len(in.Chunks))
	}
	for i := 10; i <= 20; i++ {
		if in.Chunks[i] != meta.HashZero {
			t.Fatalf("chunk %d should be a hole", i)
		}
	}
	if d := f.Ov.Dirty(); len(d) != 1 || d[0].DirtyBytes != cs/2 {
		t.Fatalf("residual %+v, want %d dirty bytes", d, cs/2)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("read-your-writes after partial flush differs at %d", testharness.FirstDiff(got, want))
	}
	f.RestartOverlay()
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("after restart differs at %d", testharness.FirstDiff(got, want))
	}
	must(t, f.Flusher().FlushInode(ctx, ino))
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("after full flush differs at %d", testharness.FirstDiff(got, want))
	}
	if in := f.Inode(ino); in.Chunks[20] == meta.HashZero || in.Chunks[19] != meta.HashZero {
		t.Fatalf("final chunks %v", in.Chunks[18:22])
	}
}

// Review #2 end to end: through overlay, flusher, FSM and reader.
// Invariant: I-1, I-35
// Lifecycle: L-8, L-11, L-38, L-168
func TestVFS_TruncateThenGrow_OldBytesDoNotResurface(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := uint64(testharness.ChunkSize)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, testharness.Pattern(int(4*cs), 15)))
	must(t, f.Flusher().FlushInode(ctx, ino))
	in := f.Inode(ino)
	must(t, f.Ov.Truncate(ino, in.Size, cs+cs/2))
	must(t, f.Ov.Write(ino, in.Size, 3*cs, []byte("tail")))
	must(t, f.Flusher().FlushInode(ctx, ino))
	want := make([]byte, 3*cs+4)
	copy(want, testharness.Pattern(int(cs+cs/2), 15))
	copy(want[3*cs:], "tail")
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("differs at %d", testharness.FirstDiff(got, want))
	}
	final := f.Inode(ino)
	if len(final.Chunks) != 4 || final.Chunks[2] != meta.HashZero {
		t.Fatalf("chunks %v", final.Chunks)
	}
	// a fresh reader (other gateway view) agrees
	out := make([]byte, len(want))
	if _, err := f.Reader().ReadAt(ctx, final, out, 0); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("other-gateway view differs at %d", testharness.FirstDiff(out, want))
	}
}

// Lifecycle: L-26, L-29
func TestFlusher_RunIdleTriggerAndShutdownFlush(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fl := f.Flusher()
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { fl.Run(rctx); close(done) }()
	ino := f.Create("a")
	data := testharness.Pattern(5000, 16)
	must(t, f.Ov.Write(ino, 0, 0, data))
	f.Clk.Advance(3 * time.Second) // idle trigger
	testharness.Eventually(t, func() bool { return f.Inode(ino).Version == 1 }, "idle flush")
	// shutdown flushes what is dirty
	must(t, f.Ov.Write(ino, 5000, 5000, []byte("more")))
	cancel()
	<-done
	if in := f.Inode(ino); in.Version != 2 || in.Size != 5004 {
		t.Fatalf("shutdown flush: %+v", in)
	}
}

// A region rewritten over and over keeps the dirty bytes small while the WAL grows with
// every record; at the WAL cap the run loop flushes the inode in full (so the WAL
// compacts) without waiting for the idle timer or the dirty-byte thresholds.
// Lifecycle: L-186
func TestFlusher_WALCapTriggersFlush(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	cs := int(testharness.ChunkSize)
	f.RestartOverlay(vfs.WithWALCap(int64(3*cs - 1)))
	fl := f.Flusher(func(c *vfs.Config) { c.IdleFlush = time.Hour; c.FileDirtyHi = 1 << 40; c.TotalDirtyHi = 1 << 40 })
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { fl.Run(rctx); close(done) }()
	ino := f.Create("a")
	data := testharness.Pattern(cs, 23)
	must(t, f.Ov.Write(ino, 0, uint64(cs), data)) // the same chunk, three times
	must(t, f.Ov.Write(ino, 0, uint64(cs), data))
	f.Clk.Advance(250 * time.Millisecond)
	if v := f.Inode(ino).Version; v != 0 {
		t.Fatalf("flushed below the WAL cap (version %d)", v)
	}
	must(t, f.Ov.Write(ino, 0, uint64(cs), data)) // WAL now ≥ cap, dirty bytes still one chunk
	f.Clk.Advance(250 * time.Millisecond)
	testharness.Eventually(t, func() bool { return f.Inode(ino).Version == 1 }, "WAL cap flush")
	testharness.Eventually(t, func() bool { return len(f.Ov.Dirty()) == 0 }, "WAL released")
	want := append(make([]byte, cs), data...)
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("differs at %d", testharness.FirstDiff(got, want))
	}
	cancel()
	<-done
}

func sha256Of(b []byte) meta.Hash { return sha256.Sum256(b) }
