package meta

import (
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
	for i := 0; i < 3000; i++ {
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
