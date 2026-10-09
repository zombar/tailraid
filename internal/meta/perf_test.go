package meta

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/testseed"
)

// Commit computes reference deltas only for the indices it changes; after every
// successful random command the counts still equal a full recount over every inode's
// chunk list, with truncates, holes, shared hashes and re-registered chunks in the mix.
// Invariant: I-27
func TestFSM_Commit_RefDeltaMatchesFullRecompute(t *testing.T) {
	f := NewFSM()
	gen := &cmdGen{rng: testseed.Seed(t), f: f, now: 5}
	var idx uint64
	apply := func(c Command) Result {
		idx++
		return f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: EncodeCommand(c)}).(Result)
	}
	if res := apply(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}}); res.Err != nil {
		t.Fatal(res.Err)
	}
	commits := 0
	for i := range 3000 {
		c := gen.next()
		res := apply(c)
		if c.Op == OpCommit && res.Err == nil {
			commits++
		}
		want := map[Hash]uint32{}
		f.mu.RLock()
		for _, in := range f.st.Inodes {
			for _, h := range in.Chunks {
				if h != HashZero {
					want[h]++
				}
			}
		}
		for h, ch := range f.st.Chunks {
			if ch.Refs != want[h] {
				f.mu.RUnlock()
				t.Fatalf("after command %d (op %d): chunk %x has Refs %d, recount %d", i, c.Op, h[:4], ch.Refs, want[h])
			}
			if (ch.Refs == 0) != (ch.TombstonedAt != 0) {
				f.mu.RUnlock()
				t.Fatalf("after command %d: chunk %x Refs %d with TombstonedAt %d", i, h[:4], ch.Refs, ch.TombstonedAt)
			}
		}
		f.mu.RUnlock()
	}
	if commits < 200 {
		t.Fatalf("only %d commits applied", commits)
	}
}

// The read API's inode copies carry no Children map: a LOOKUP or GETATTR on a directory
// does not copy its name table, and names are read through Lookup and ReadDir.
func TestFSM_InodeCopies_OmitChildren(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	tf.create(d, "f")
	for name, in := range map[string]Inode{"GetInode": tf.inode(d)} {
		if in.Children != nil {
			t.Fatalf("%s copy carries Children", name)
		}
	}
	if in, ok := tf.f.Lookup(RootIno, "d"); !ok || in.Children != nil || in.Ino != d {
		t.Fatalf("Lookup: %+v", in)
	}
	if in, err := tf.f.Resolve([]string{"d"}); err != nil || in.Children != nil {
		t.Fatalf("Resolve: %+v %v", in, err)
	}
	ents, err := tf.f.ReadDir(RootIno)
	if err != nil || len(ents) != 1 || ents[0].Name != "d" || ents[0].Children != nil {
		t.Fatalf("ReadDir: %+v %v", ents, err)
	}
	if tf.child(d, "f") == 0 {
		t.Fatal("Lookup of the file failed")
	}
}

// The volume statistics kept by the applier equal a full walk after every random command
// (commits with truncates, holes, dedup and re-registration; unlinks; renames over files;
// placement and policy changes; GC) and after a snapshot/restore round trip.
func TestFSM_Stats_IncrementalMatchesRecompute(t *testing.T) {
	f := NewFSM()
	gen := &cmdGen{rng: testseed.Seed(t), f: f, now: 5}
	var idx uint64
	apply := func(c Command) Result {
		idx++
		return f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: EncodeCommand(c)}).(Result)
	}
	if res := apply(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}}); res.Err != nil {
		t.Fatal(res.Err)
	}
	for i := range 3000 {
		c := gen.next()
		apply(c)
		got := f.Stats()
		f.mu.RLock()
		want := computeStats(f.st)
		f.mu.RUnlock()
		if got != want {
			t.Fatalf("after command %d (op %d): stats %+v, recount %+v", i, c.Op, got, want)
		}
	}
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := EncodeState(&buf, snap.(*snapshot).st); err != nil {
		t.Fatal(err)
	}
	g := NewFSM()
	if err := g.Restore(io.NopCloser(&buf)); err != nil {
		t.Fatal(err)
	}
	if got, want := g.Stats(), f.Stats(); got != want {
		t.Fatalf("after restore: %+v, want %+v", got, want)
	}
	if got := f.Stats(); got.Chunks == 0 || got.LogicalBytes == 0 {
		t.Fatalf("workload too thin: %+v", got)
	}
}

// One usage formula for FSSTAT and status (X5): only Active capacity counts, scaled by
// k/(k+m); free never underflows; a volume without a policy is treated as 1+0.
func TestUsage_Formula(t *testing.T) {
	nodes := []Node{{State: Active, Capacity: 300}, {State: Draining, Capacity: 1000}, {State: Active, Capacity: 300}, {State: Removed, Capacity: 5000}}
	cases := []struct {
		vol               Volume
		stored            uint64
		total, used, free uint64
	}{
		{Volume{K: 2, M: 1}, 0, 400, 0, 400},
		{Volume{K: 2, M: 1}, 300, 400, 200, 200},
		{Volume{K: 2, M: 1}, 900, 400, 600, 0},
		{Volume{K: 1, M: 0}, 100, 600, 100, 500},
		{Volume{}, 100, 600, 100, 500},
	}
	for _, c := range cases {
		total, used, free := Usage(c.vol, nodes, Stats{StoredBytes: c.stored})
		if total != c.total || used != c.used || free != c.free {
			t.Errorf("%d+%d stored %d: got %d/%d/%d, want %d/%d/%d", c.vol.K, c.vol.M, c.stored, total, used, free, c.total, c.used, c.free)
		}
	}
}

// The chunk walk visits every entry present at the start exactly once, in hash order,
// and stops when told to.
func TestFSM_ForEachChunk_OrderAndStop(t *testing.T) {
	bf := newTFSM(t)
	var hs []Hash
	for i := range 2500 {
		h := hashN(uint64(i) + 1)
		hs = append(hs, h)
		ino := bf.create(RootIno, fmt.Sprintf("f%d", i))
		bf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: ino, NewSize: testCS, Writes: []ChunkWrite{{0, h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 2}})
	}
	var seen []Hash
	bf.f.ForEachChunk(func(c Chunk) bool { seen = append(seen, c.Hash); return true })
	if len(seen) != len(hs) {
		t.Fatalf("visited %d of %d", len(seen), len(hs))
	}
	for i := 1; i < len(seen); i++ {
		if bytes.Compare(seen[i-1][:], seen[i][:]) >= 0 {
			t.Fatalf("out of order at %d", i)
		}
	}
	n := 0
	bf.f.ForEachChunk(func(Chunk) bool { n++; return n < 1500 })
	if n != 1500 {
		t.Fatalf("stopped after %d", n)
	}
}

// The indexed node lookups agree with the sorted table: by index, by raft ID and by
// address (devices that are not Removed only), and ForEachNode visits every record once.
func TestFSM_NodeLookups(t *testing.T) {
	tf := newTFSM(t)
	for i := 1; i <= 4; i++ {
		tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: fmt.Sprintf("id%d", i), Name: fmt.Sprintf("n%d", i), Kind: KindDevice, State: Active, Addr: fmt.Sprintf("10.0.0.%d", i), RPCPort: 20481, RaftPort: 20480}}})
	}
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 4, State: Removed, Now: 5}})
	nodes := tf.f.Nodes()
	if len(nodes) != 4 {
		t.Fatalf("%d nodes", len(nodes))
	}
	for _, want := range nodes {
		got, ok := tf.f.Node(want.Idx)
		if !ok || got.ID != want.ID {
			t.Fatalf("Node(%d) = %+v %v", want.Idx, got, ok)
		}
		if want.State == Removed {
			if _, ok := tf.f.NodeByID(want.ID); ok {
				t.Fatal("NodeByID returned a removed record")
			}
			if _, ok := tf.f.NodeByAddr(want.Addr); ok {
				t.Fatal("NodeByAddr returned a removed record")
			}
			continue
		}
		if got, ok := tf.f.NodeByID(want.ID); !ok || got.Idx != want.Idx {
			t.Fatalf("NodeByID(%s) = %+v %v", want.ID, got, ok)
		}
		if got, ok := tf.f.NodeByAddr(want.Addr); !ok || got.Idx != want.Idx {
			t.Fatalf("NodeByAddr(%s) = %+v %v", want.Addr, got, ok)
		}
		if got.RPCAddr() != want.Addr+":20481" || got.RaftAddr() != want.Addr+":20480" {
			t.Fatalf("addresses %s %s", got.RPCAddr(), got.RaftAddr())
		}
	}
	if _, ok := tf.f.Node(99); ok {
		t.Fatal("Node(99)")
	}
	if _, ok := tf.f.NodeByAddr("10.9.9.9"); ok {
		t.Fatal("NodeByAddr unknown")
	}
	seen := map[NodeIdx]int{}
	tf.f.ForEachNode(func(n Node) bool { seen[n.Idx]++; return true })
	if len(seen) != 4 {
		t.Fatalf("ForEachNode saw %v", seen)
	}
	n := 0
	tf.f.ForEachNode(func(Node) bool { n++; return false })
	if n != 1 {
		t.Fatalf("ForEachNode did not stop: %d", n)
	}
}

// The read API never runs a caller's callback under the FSM lock: ForEachNode and
// ForEachChunk hand out copies and call fn with the lock free, so fn may read the FSM
// again. A queued Apply (writer) between an outer and a nested read lock would
// otherwise block the node for good; this is what the liveness scan did once.
func TestFSM_Callbacks_RunWithLockFree(t *testing.T) {
	tf := newTFSM(t)
	tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "n1", Name: "n1", Kind: KindDevice, State: Active}}})
	h := hashN(1)
	ino := tf.create(RootIno, "f")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: ino, NewSize: testCS, Writes: []ChunkWrite{{0, h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 2}})
	calls := 0
	tf.f.ForEachNode(func(n Node) bool {
		calls++
		if !tf.f.mu.TryLock() { // a writer could not get in: the callback holds the lock
			t.Fatal("ForEachNode runs its callback under the FSM lock")
		}
		tf.f.mu.Unlock()
		if _, ok := tf.f.NodeByID(n.ID); !ok { // a nested read must work
			t.Fatal("nested read failed")
		}
		return true
	})
	tf.f.ForEachChunk(func(c Chunk) bool {
		calls++
		if !tf.f.mu.TryLock() {
			t.Fatal("ForEachChunk runs its callback under the FSM lock")
		}
		tf.f.mu.Unlock()
		if _, ok := tf.f.GetChunk(c.Hash); !ok {
			t.Fatal("nested read failed")
		}
		return true
	})
	if calls != 2 {
		t.Fatalf("%d callbacks", calls)
	}
}
