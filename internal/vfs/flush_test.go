package vfs

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gurgeh/tailraid/internal/chunk"
	"github.com/gurgeh/tailraid/internal/meta"
)

var ctx = context.Background()

// Invariant: I-7
// Lifecycle: L-26, L-35, L-36, L-39, L-50, L-51
func TestFlusher_SmallAndLargeChunksCommit(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	cs := uint64(fabCS)
	ino := f.create("a")
	data := pattern(int(cs)+100, 1) // chunk 0 full (k=2,m=1), chunk 1 = 100 B (k=1,m=1)
	must(t, f.ov.Write(ino, 0, 0, data))
	must(t, f.flusher().FlushInode(ctx, ino))
	in := f.inode(ino)
	if in.Size != cs+100 || len(in.Chunks) != 2 || in.Version != 1 {
		t.Fatalf("inode %+v", in)
	}
	c0, _ := f.fsm.GetChunk(in.Chunks[0])
	c1, _ := f.fsm.GetChunk(in.Chunks[1])
	if c0.K != 2 || c0.M != 1 || len(f.shardsOn(in.Chunks[0], 2, 1)) != 3 {
		t.Fatalf("large chunk %+v on %v", c0, f.shardsOn(in.Chunks[0], 2, 1))
	}
	if c1.K != 1 || c1.M != 1 || len(f.shardsOn(in.Chunks[1], 1, 1)) != 2 {
		t.Fatalf("small chunk %+v", c1)
	}
	for _, c := range []meta.Chunk{c0, c1} {
		for _, slot := range c.Placement {
			if slot == 0 {
				t.Fatalf("missing slot in %+v", c)
			}
		}
	}
	if len(f.ov.Dirty()) != 0 {
		t.Fatalf("still dirty: %+v", f.ov.Dirty())
	}
	if got := f.readFile(ino); !bytes.Equal(got, data) {
		t.Fatalf("read back differs at %d", firstDiff(got, data))
	}
}

// Invariant: I-7
// Lifecycle: L-42, L-155, L-59
func TestFlusher_FewerThanKAcks_StaysDirty(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	ino := f.create("a")
	data := pattern(int(fabCS), 2)
	must(t, f.ov.Write(ino, 0, 0, data))
	f.setDown(2, true)
	f.setDown(3, true) // only self is live: 1 ack < k
	err := f.flusher().FlushInode(ctx, ino)
	if err == nil {
		t.Fatal("flush succeeded with < k acks")
	}
	in := f.inode(ino)
	if in.Version != 0 || in.Size != 0 {
		t.Fatalf("commit happened: %+v", in)
	}
	d := f.ov.Dirty()
	if len(d) != 1 || d[0].DirtyBytes != uint64(len(data)) || d[0].Flushing {
		t.Fatalf("dirty state lost: %+v", d)
	}
	if got := f.readFile(ino); !bytes.Equal(got, data) {
		t.Fatal("read-your-writes lost after failed flush")
	}
	// nodes return → the same data flushes
	f.setDown(2, false)
	f.setDown(3, false)
	must(t, f.flusher().FlushInode(ctx, ino))
	if got := f.readFile(ino); !bytes.Equal(got, data) {
		t.Fatal("content after recovery differs")
	}
}

// Lifecycle: L-41, L-52
func TestFlusher_DegradedCommitRecordsZeroSlots(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	ino := f.create("a")
	data := pattern(int(fabCS), 3)
	must(t, f.ov.Write(ino, 0, 0, data))
	f.setDown(3, true) // 2 live = k; no spare for re-placement
	must(t, f.flusher().FlushInode(ctx, ino))
	in := f.inode(ino)
	c, _ := f.fsm.GetChunk(in.Chunks[0])
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
	if got := f.readFile(ino); !bytes.Equal(got, data) {
		t.Fatal("degraded chunk unreadable")
	}
}

// Lifecycle: L-40
func TestFlusher_FailedSlotReplacedOnce(t *testing.T) {
	f := newFabric(t, 4, 2, 1)
	ino := f.create("a")
	data := pattern(int(fabCS), 4)
	must(t, f.ov.Write(ino, 0, 0, data))
	// find which node the placement will pick and make its first PUT fail
	h := sha256Of(data)
	cands := meta.Candidates(f.fsm.Nodes(), func(meta.NodeIdx) bool { return true })
	victim := meta.Place(h, cands, 3)[1]
	f.failPutOnce[victim] = 1
	must(t, f.flusher().FlushInode(ctx, ino))
	c, _ := f.fsm.GetChunk(h)
	for _, s := range c.Placement {
		if s == 0 || s == victim {
			t.Fatalf("placement %v should be full and avoid %d", c.Placement, victim)
		}
	}
	if len(f.shardsOn(h, 2, 1)) != 3 {
		t.Fatalf("shards on %v", f.shardsOn(h, 2, 1))
	}
}

// Lifecycle: L-44
func TestFlusher_FullNodeExcluded(t *testing.T) {
	f := newFabric(t, 4, 2, 1)
	ino := f.create("a")
	data := pattern(int(fabCS), 5)
	must(t, f.ov.Write(ino, 0, 0, data))
	f.full[2] = true
	must(t, f.flusher().FlushInode(ctx, ino))
	c, _ := f.fsm.GetChunk(sha256Of(data))
	for _, s := range c.Placement {
		if s == 0 || s == 2 {
			t.Fatalf("placement %v", c.Placement)
		}
	}
}

// Lifecycle: L-32, L-152
func TestFlusher_HoleChunksAreHashZero(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	cs := uint64(fabCS)
	ino := f.create("a")
	must(t, f.ov.Write(ino, 0, 3*cs, []byte("far")))
	must(t, f.ov.Write(ino, 0, cs, make([]byte, cs))) // explicit zeros are a hole too
	before := f.puts()
	must(t, f.flusher().FlushInode(ctx, ino))
	in := f.inode(ino)
	if len(in.Chunks) != 4 || in.Chunks[0] != meta.HashZero || in.Chunks[1] != meta.HashZero || in.Chunks[2] != meta.HashZero {
		t.Fatalf("chunks %v", in.Chunks)
	}
	if f.puts()-before != 2 { // only the 3-byte chunk: k=1,m=1 → 2 PUTs
		t.Fatalf("%d PUTs for holes", f.puts()-before)
	}
	if _, ok := f.fsm.GetChunk(meta.HashZero); ok {
		t.Fatal("HashZero entry")
	}
}

// Lifecycle: L-33, L-34
func TestFlusher_DedupLiveAndTombstoned_NoUpload(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	data := pattern(int(fabCS), 6)
	a := f.create("a")
	must(t, f.ov.Write(a, 0, 0, data))
	must(t, f.flusher().FlushInode(ctx, a))
	h := f.inode(a).Chunks[0]
	before := f.puts()
	b := f.create("b")
	must(t, f.ov.Write(b, 0, 0, data))
	must(t, f.flusher().FlushInode(ctx, b))
	if f.puts() != before {
		t.Fatalf("dedup uploaded %d shards", f.puts()-before)
	}
	if c, _ := f.fsm.GetChunk(h); c.Refs != 2 {
		t.Fatalf("refs %d", c.Refs)
	}
	// tombstone it, then revive by a third write: still no upload
	f.mustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "a", Now: 5}})
	f.mustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "b", Now: 6}})
	if c, _ := f.fsm.GetChunk(h); c.Refs != 0 || c.TombstonedAt == 0 {
		t.Fatal("not tombstoned")
	}
	c := f.create("c")
	must(t, f.ov.Write(c, 0, 0, data))
	must(t, f.flusher().FlushInode(ctx, c))
	if f.puts() != before {
		t.Fatal("revive uploaded")
	}
	if ch, _ := f.fsm.GetChunk(h); ch.Refs != 1 || ch.TombstonedAt != 0 {
		t.Fatalf("revived %+v", ch)
	}
}

// Review #6: a dedup hit against an entry with < k live slots fills the slots first.
// Lifecycle: L-146
func TestFlusher_DedupDegradedEntry_FillsSlotsBeforeCommit(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	data := pattern(int(fabCS), 7)
	a := f.create("a")
	must(t, f.ov.Write(a, 0, 0, data))
	must(t, f.flusher().FlushInode(ctx, a))
	h := f.inode(a).Chunks[0]
	c, _ := f.fsm.GetChunk(h)
	// two of three holders die; one spare joins
	f.setDown(c.Placement[1], true)
	f.setDown(c.Placement[2], true)
	spare := f.addNode()
	b := f.create("b")
	must(t, f.ov.Write(b, 0, 0, data))
	must(t, f.flusher().FlushInode(ctx, b))
	c2, _ := f.fsm.GetChunk(h)
	live := 0
	onSpare := false
	for _, s := range c2.Placement {
		if s != 0 && !f.down[s] {
			live++
		}
		if s == spare {
			onSpare = true
		}
	}
	if live < 2 || !onSpare || c2.Refs != 2 {
		t.Fatalf("placement after fill %v refs %d", c2.Placement, c2.Refs)
	}
	ops := f.ops()
	if ops[len(ops)-2] != meta.OpSetPlacement || ops[len(ops)-1] != meta.OpCommit {
		t.Fatalf("ops %v", ops)
	}
	if got := f.readFile(b); !bytes.Equal(got, data) {
		t.Fatal("unreadable after fill")
	}
}

// Invariant: I-35
// Lifecycle: L-21, L-53
func TestFlusher_ConflictRebase_PreservesConcurrentAppend(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	cs := uint64(fabCS)
	ino := f.create("a")
	base := pattern(int(2*cs), 8)
	must(t, f.ov.Write(ino, 0, 0, base))
	must(t, f.flusher().FlushInode(ctx, ino))
	in := f.inode(ino)
	// this gateway overwrites chunk 0; another gateway appends chunk 2 before our commit
	mine := pattern(int(cs), 9)
	must(t, f.ov.Write(ino, in.Size, 0, mine))
	other := pattern(int(cs), 10)
	oh := sha256Of(other)
	var done bool
	fl := f.flusher()
	fl.Hooks.BeforeCommit = func() {
		if done {
			return
		}
		done = true
		must(t, f.Put(ctx, 1, chunk.Key(oh, 2, 1, 0), other[:cs/2]))
		f.mustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, BaseVersion: in.Version, NewSize: 3 * cs,
			Writes: []meta.ChunkWrite{{Idx: 2, Hash: oh}}, NewChunks: []meta.ChunkDesc{{Hash: oh, Size: uint32(cs), K: 2, M: 1, Placement: []meta.NodeIdx{1, 2, 3}}}, Now: 7}})
	}
	must(t, fl.FlushInode(ctx, ino))
	final := f.inode(ino)
	if final.Size != 3*cs || len(final.Chunks) != 3 || final.Chunks[0] != sha256Of(mine) || final.Chunks[2] != oh || final.Chunks[1] != in.Chunks[1] {
		t.Fatalf("final %+v", final)
	}
	if len(f.ov.Dirty()) != 0 {
		t.Fatal("extents left dirty")
	}
}

// Invariant: I-35
// Lifecycle: L-22
func TestFlusher_ConflictRebase_TruncateWins(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	cs := uint64(fabCS)
	ino := f.create("a")
	must(t, f.ov.Write(ino, 0, 0, pattern(int(2*cs), 8)))
	must(t, f.flusher().FlushInode(ctx, ino))
	in := f.inode(ino)
	must(t, f.ov.Truncate(ino, in.Size, cs/2))
	other := pattern(int(cs), 10)
	oh := sha256Of(other)
	var done bool
	fl := f.flusher()
	fl.Hooks.BeforeCommit = func() {
		if done {
			return
		}
		done = true
		f.mustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, BaseVersion: in.Version, NewSize: 3 * cs,
			Writes: []meta.ChunkWrite{{Idx: 2, Hash: oh}}, NewChunks: []meta.ChunkDesc{{Hash: oh, Size: uint32(cs), K: 2, M: 1, Placement: []meta.NodeIdx{1, 2, 3}}}, Now: 7}})
	}
	must(t, fl.FlushInode(ctx, ino))
	final := f.inode(ino)
	if final.Size != cs/2 || len(final.Chunks) != 1 {
		t.Fatalf("truncate did not win: %+v", final)
	}
	if c, _ := f.fsm.GetChunk(oh); c.Refs != 0 {
		t.Fatal("appended chunk still referenced")
	}
}

// Lifecycle: L-54
func TestFlusher_UnknownChunk_ReUpload(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	data := pattern(int(fabCS), 11)
	a := f.create("a")
	must(t, f.ov.Write(a, 0, 0, data))
	must(t, f.flusher().FlushInode(ctx, a))
	h := f.inode(a).Chunks[0]
	f.mustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "a", Now: 5}})
	b := f.create("b")
	must(t, f.ov.Write(b, 0, 0, data))
	fl := f.flusher()
	var once bool
	fl.Hooks.BeforeCommit = func() {
		if once {
			return
		}
		once = true
		// GC collects the tombstone between dedup and commit
		r := f.mustApply(meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: 1 << 62}})
		for _, v := range r.Victims {
			for i, n := range v.Placement {
				if n != 0 {
					_ = f.Delete(ctx, n, chunk.Key(v.Hash, 2, 1, i))
				}
			}
		}
	}
	must(t, fl.FlushInode(ctx, b))
	c, ok := f.fsm.GetChunk(h)
	if !ok || c.Refs != 1 {
		t.Fatalf("chunk after re-upload %+v %v", c, ok)
	}
	if len(f.shardsOn(h, 2, 1)) != 3 {
		t.Fatalf("shards on %v", f.shardsOn(h, 2, 1))
	}
	if got := f.readFile(b); !bytes.Equal(got, data) {
		t.Fatal("content")
	}
}

// Invariant: I-35
// Lifecycle: L-58
func TestFlusher_ProposeFailureRetriesSamePlan(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	ino := f.create("a")
	data := pattern(int(fabCS)+10, 12)
	must(t, f.ov.Write(ino, 0, 0, data))
	f.proposeErr = errors.New("leadership lost")
	if err := f.flusher().FlushInode(ctx, ino); err == nil {
		t.Fatal("expected error")
	}
	if d := f.ov.Dirty(); len(d) != 1 || d[0].Flushing || d[0].DirtyBytes != uint64(len(data)) {
		t.Fatalf("dirty after failure: %+v", d)
	}
	must(t, f.flusher().FlushInode(ctx, ino))
	if got := f.readFile(ino); !bytes.Equal(got, data) {
		t.Fatal("content")
	}
}

// Lifecycle: L-55
func TestFlusher_UnlinkedBeforeFlush_Dropped(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	ino := f.create("a")
	must(t, f.ov.Write(ino, 0, 0, pattern(int(fabCS), 13)))
	f.mustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "a", Now: 5}})
	before := f.puts()
	must(t, f.flusher().FlushInode(ctx, ino))
	if f.puts() != before {
		t.Fatal("uploaded for a dead inode")
	}
	if _, ok := f.ov.Size(ino); ok {
		t.Fatal("overlay still tracks the inode")
	}
	if st := f.fsm.Stats(); st.Chunks != 0 {
		t.Fatalf("chunk entries created: %+v", st)
	}
}

// Invariant: I-35
// Lifecycle: L-27
func TestFlusher_PartialFlush_SizeNeverAheadOfData(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	cs := uint64(fabCS)
	ino := f.create("a")
	data := pattern(int(2*cs+cs/2), 14)
	must(t, f.ov.Write(ino, 0, 0, data))
	must(t, f.flusher().flush(ctx, ino, true))
	in := f.inode(ino)
	if in.Size != 2*cs || len(in.Chunks) != 2 {
		t.Fatalf("partial flush committed %+v", in)
	}
	if d := f.ov.Dirty(); len(d) != 1 || d[0].DirtyBytes != cs/2 {
		t.Fatalf("residual %+v", d)
	}
	if got := f.readFile(ino); !bytes.Equal(got, data) {
		t.Fatal("read-your-writes after partial flush")
	}
	must(t, f.flusher().FlushInode(ctx, ino))
	if in := f.inode(ino); in.Size != 2*cs+cs/2 || len(in.Chunks) != 3 {
		t.Fatalf("after full flush %+v", in)
	}
	if got := f.readFile(ino); !bytes.Equal(got, data) {
		t.Fatal("content after full flush")
	}
}

// Review #2 end to end: through overlay, flusher, FSM and reader.
// Invariant: I-1, I-35
// Lifecycle: L-8, L-11, L-38
func TestVFS_TruncateThenGrow_OldBytesDoNotResurface(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	cs := uint64(fabCS)
	ino := f.create("a")
	must(t, f.ov.Write(ino, 0, 0, pattern(int(4*cs), 15)))
	must(t, f.flusher().FlushInode(ctx, ino))
	in := f.inode(ino)
	must(t, f.ov.Truncate(ino, in.Size, cs+cs/2))
	must(t, f.ov.Write(ino, in.Size, 3*cs, []byte("tail")))
	must(t, f.flusher().FlushInode(ctx, ino))
	want := make([]byte, 3*cs+4)
	copy(want, pattern(int(cs+cs/2), 15))
	copy(want[3*cs:], "tail")
	if got := f.readFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("differs at %d", firstDiff(got, want))
	}
	final := f.inode(ino)
	if len(final.Chunks) != 4 || final.Chunks[2] != meta.HashZero {
		t.Fatalf("chunks %v", final.Chunks)
	}
	// a fresh reader (other gateway view) agrees
	out := make([]byte, len(want))
	if _, err := f.reader().ReadAt(ctx, final, out, 0); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("other-gateway view differs at %d", firstDiff(out, want))
	}
}

// Lifecycle: L-26, L-29
func TestFlusher_RunIdleTriggerAndShutdownFlush(t *testing.T) {
	f := newFabric(t, 3, 2, 1)
	fl := f.flusher()
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { fl.Run(rctx); close(done) }()
	ino := f.create("a")
	data := pattern(5000, 16)
	must(t, f.ov.Write(ino, 0, 0, data))
	f.clk.Advance(3 * time.Second) // idle trigger
	eventually(t, func() bool { return f.inode(ino).Version == 1 }, "idle flush")
	// shutdown flushes what is dirty
	must(t, f.ov.Write(ino, 5000, 5000, []byte("more")))
	cancel()
	<-done
	if in := f.inode(ino); in.Version != 2 || in.Size != 5004 {
		t.Fatalf("shutdown flush: %+v", in)
	}
}

func sha256Of(b []byte) meta.Hash { return sha256Sum(b) }
