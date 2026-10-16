package meta

import (
	"slices"
	"testing"
)

// appendOne commits one more chunk at the end of ino's list.
func (tf *tfsm) appendOne(ino Ino, h Hash) {
	tf.t.Helper()
	in, _ := tf.f.GetInode(ino)
	n := uint32(len(in.Chunks))
	tf.raw(Command{Op: OpCommit, Commit: &Commit{
		Ino: ino, BaseVersion: in.Version, NewSize: uint64(n+1) * testCS,
		Writes: []ChunkWrite{{Idx: n, Hash: h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 2,
	}})
}

// Five hundred one-chunk appends behind a twenty-thousand-chunk list copy no list: each
// costs its own write, amortized, not the file's length (I-385).
// Invariant: I-385
func TestFSM_Commit_AppendCostIndependentOfLength(t *testing.T) {
	const base, appends = 20_000, 500
	tf := newTFSM(t)
	ino := tf.create(RootIno, "stream")
	writes := make([]ChunkWrite, base)
	news := make([]ChunkDesc, base)
	for i := range base {
		h := hashN(uint64(i) + 1)
		writes[i], news[i] = ChunkWrite{Idx: uint32(i), Hash: h}, desc(h)
	}
	in0, _ := tf.f.GetInode(ino)
	tf.raw(Command{Op: OpCommit, Commit: &Commit{Ino: ino, BaseVersion: in0.Version, NewSize: base * testCS, Writes: writes, NewChunks: news, Now: 1}})
	n := allocBytes(func() {
		for i := range appends {
			tf.appendOne(ino, hashN(uint64(base+i)+1))
		}
	})
	if n > 16<<20 {
		t.Fatalf("%d one-chunk appends behind %d chunks allocated %d MiB", appends, base, n>>20)
	}
	t.Logf("%d one-chunk appends behind %d chunks: %d KiB", appends, base, n>>10)
	in, _ := tf.f.GetInode(ino)
	if len(in.Chunks) != base+appends || in.Chunks[base+appends-1] != hashN(base+appends) {
		t.Fatalf("list of %d after the appends", len(in.Chunks))
	}
	if v := CheckStats(tf.f.state(), tf.f.stats); v != nil {
		t.Fatal(v)
	}
}

// A list a reader holds never changes: not under appends that extend the same array,
// nor under a rewrite, a hole past the end or a truncate after them (I-385, I-104).
// Invariant: I-385, I-104
func TestFSM_Commit_AppendLeavesHeldListsAlone(t *testing.T) {
	tf := newTFSM(t)
	ino := tf.create(RootIno, "f")
	type held struct{ list, want []Hash }
	var views []held
	hold := func() {
		in, _ := tf.f.GetInode(ino)
		views = append(views, held{list: in.Chunks, want: slices.Clone(in.Chunks)})
	}
	hold()
	for i := range 40 {
		tf.appendOne(ino, hashN(uint64(i)+1))
		hold()
	}
	in, _ := tf.f.GetInode(ino)
	// a rewrite of the first chunk, then a write past the end leaving a hole
	tf.raw(Command{Op: OpCommit, Commit: &Commit{
		Ino: ino, BaseVersion: in.Version, NewSize: 40 * testCS,
		Writes: []ChunkWrite{{Idx: 0, Hash: hashN(100)}}, NewChunks: []ChunkDesc{desc(hashN(100))}, Now: 3,
	}})
	hold()
	in, _ = tf.f.GetInode(ino)
	tf.raw(Command{Op: OpCommit, Commit: &Commit{
		Ino: ino, BaseVersion: in.Version, NewSize: 50 * testCS,
		Writes: []ChunkWrite{{Idx: 49, Hash: hashN(101)}}, NewChunks: []ChunkDesc{desc(hashN(101))}, Now: 4,
	}})
	hold()
	in, _ = tf.f.GetInode(ino)
	tf.raw(Command{Op: OpCommit, Commit: &Commit{Ino: ino, BaseVersion: in.Version, HasTruncate: true, TruncateTo: 10 * testCS, NewSize: 10 * testCS, Now: 5}})
	hold()
	for i := range 5 {
		tf.appendOne(ino, hashN(uint64(200+i)))
		hold()
	}
	for i, v := range views {
		if !slices.Equal(v.list, v.want) {
			t.Fatalf("view %d changed under later commits", i)
		}
	}
	if in, _ := tf.f.GetInode(ino); len(in.Chunks) != 15 || in.Chunks[0] != hashN(100) || in.Chunks[14] != hashN(204) {
		t.Fatalf("final list %d long", len(in.Chunks))
	}
	if v := CheckInvariants(tf.f.state()); v != nil {
		t.Fatal(v)
	}
}

// A caller that appends to an inode's list from the read API gets its own array: the
// FSM's next append never shows through it, nor it through the FSM's list (I-385).
// Invariant: I-385
func TestFSM_Commit_ReaderAppendReallocates(t *testing.T) {
	tf := newTFSM(t)
	ino := tf.create(RootIno, "f")
	for i := range 3 {
		tf.appendOne(ino, hashN(uint64(i)+1))
	}
	in, _ := tf.f.GetInode(ino)
	mine := append(in.Chunks, hashN(99)) //nolint:gocritic // appending to the read API's list is the case under test
	tf.appendOne(ino, hashN(4))
	if mine[3] != hashN(99) {
		t.Fatal("the FSM's append wrote through a reader's list")
	}
	if now, _ := tf.f.GetInode(ino); now.Chunks[3] != hashN(4) {
		t.Fatal("a reader's append wrote into the FSM's list")
	}
}
