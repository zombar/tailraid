package meta

import (
	"bytes"
	"errors"
	"testing"
	"testing/iotest"

	"github.com/hashicorp/raft"
)

// A SetPlacement names the placement it was planned from; the FSM applies it only while
// that is still the chunk's placement, else ErrConflict and nothing changes.
//
// Invariant: I-102
// Lifecycle: L-197
func TestFSM_SetPlacement_ExpectConflict(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	h := sha("chunk")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 1}})
	c, _ := tf.f.GetChunk(h)
	was := append([]NodeIdx(nil), c.Placement...)
	moved := append([]NodeIdx(nil), was...)
	moved[0], moved[1] = moved[1], moved[0]
	stale := append([]NodeIdx(nil), was...)
	stale[2] = 0
	// planned from a placement the chunk no longer has: refused, placement untouched
	r := tf.apply(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: h, K: c.K, M: c.M, Placement: moved, Expect: stale}})
	if !errors.Is(r.Err, ErrConflict) {
		t.Fatalf("stale expectation: %v, want ErrConflict", r.Err)
	}
	if now, _ := tf.f.GetChunk(h); !equalIdx(now.Placement, was) {
		t.Fatalf("placement changed on a refused command: %v", now.Placement)
	}
	// planned from the current one: applied
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: h, K: c.K, M: c.M, Placement: moved, Expect: was}})
	if now, _ := tf.f.GetChunk(h); !equalIdx(now.Placement, moved) {
		t.Fatalf("placement after a matching expectation: %v, want %v", now.Placement, moved)
	}
	// no expectation: applied (the pre-I-102 form, still decoded)
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: h, K: c.K, M: c.M, Placement: was}})
	if now, _ := tf.f.GetChunk(h); !equalIdx(now.Placement, was) {
		t.Fatalf("placement after an unconditional command: %v", now.Placement)
	}
}

// GCDelete removes at most Limit entries, lowest hashes first; 0 is unbounded.
//
// Invariant: I-103
func TestFSM_GCDelete_LimitBoundary(t *testing.T) {
	const tombs = 5
	for _, c := range []struct {
		limit uint32
		want  int
	}{{0, tombs}, {1, 1}, {tombs - 1, tombs - 1}, {tombs, tombs}, {tombs + 1, tombs}} {
		tf := newTFSM(t)
		f := tf.create(RootIno, "f")
		var writes []ChunkWrite
		var news []ChunkDesc
		for i := range tombs {
			h := sha(string(rune('a' + i)))
			writes = append(writes, ChunkWrite{uint32(i), h})
			news = append(news, desc(h))
		}
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: tombs * testCS, Writes: writes, NewChunks: news, Now: 1}})
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, HasTruncate: true, TruncateTo: 0, NewSize: 0, Now: 10}}) // all five tombstoned at 10
		r := tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 11, Limit: c.limit}})
		if len(r.Victims) != c.want {
			t.Fatalf("limit %d: %d victims, want %d", c.limit, len(r.Victims), c.want)
		}
		for i := 1; i < len(r.Victims); i++ {
			if bytes.Compare(r.Victims[i-1].Hash[:], r.Victims[i].Hash[:]) >= 0 {
				t.Fatalf("limit %d: victims not in hash order", c.limit)
			}
		}
		if st := tf.f.Stats(); st.Tombstoned != tombs-c.want {
			t.Fatalf("limit %d: %d tombstones left, want %d", c.limit, st.Tombstoned, tombs-c.want)
		}
	}
}

// A snapshot is a deep copy: a Commit applied after it (which rewrites the chunk list in
// place when the length does not change) never alters what the snapshot will write.
//
// Invariant: I-104
func TestFSM_SnapshotUnaffectedByLaterCommit(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a, b := sha("a"), sha("b")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
	snap, err := tf.f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, NewSize: testCS, Writes: []ChunkWrite{{0, b}}, NewChunks: []ChunkDesc{desc(b)}, Now: 2}})
	st := snap.(interface{ State() *State }).State()
	if got := st.Inodes[f].Chunks[0]; got != a {
		t.Fatalf("the snapshot saw the later commit: chunk 0 is %x, want %x", got[:4], a[:4])
	}
	if live := tf.f.state().Inodes[f].Chunks[0]; live != b {
		t.Fatalf("live state: chunk 0 is %x, want %x", live[:4], b[:4])
	}
	var buf bytes.Buffer
	if err := EncodeState(&buf, st); err != nil {
		t.Fatal(err)
	}
	back, err := DecodeState(&buf)
	if err != nil || back.Inodes[f].Chunks[0] != a {
		t.Fatalf("snapshot round trip: %v", err)
	}
}

// A snapshot decodes from a reader of any granularity, a byte at a time included: the
// decoder streams instead of reading the whole snapshot first.
//
// Invariant: I-104
func TestDecodeState_StreamsFromAnyReader(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	for i := range 40 {
		f := tf.create(d, string(rune('a'+i%26))+string(rune('a'+i/26)))
		h := sha(string(rune('A' + i)))
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, h}}, NewChunks: []ChunkDesc{desc(h)}, Now: int64(i + 1)}})
	}
	var buf bytes.Buffer
	if err := EncodeState(&buf, tf.f.state().Clone()); err != nil {
		t.Fatal(err)
	}
	whole, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	byteWise, err := DecodeState(iotest.OneByteReader(bytes.NewReader(buf.Bytes())))
	if err != nil {
		t.Fatalf("one byte at a time: %v", err)
	}
	var w1, w2 bytes.Buffer
	if err := EncodeState(&w1, whole); err != nil {
		t.Fatal(err)
	}
	if err := EncodeState(&w2, byteWise); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w1.Bytes(), w2.Bytes()) || !bytes.Equal(w1.Bytes(), buf.Bytes()) {
		t.Fatal("the streamed decode differs from the whole-buffer decode")
	}
	// trailing bytes are still an error
	if _, err := DecodeState(iotest.OneByteReader(bytes.NewReader(append(buf.Bytes(), 0)))); err == nil {
		t.Fatal("trailing byte accepted")
	}
}

// An entry this binary cannot decode is counted and the index still advances.
//
// Invariant: I-105
func TestFSM_IncompatibleEntryCounted(t *testing.T) {
	f := NewFSM()
	r := f.Apply(&raft.Log{Index: 7, Type: raft.LogCommand, Data: []byte{9, 9, 9}}).(Result)
	if !errors.Is(r.Err, ErrBadCommand) {
		t.Fatalf("undecodable entry: %v", r.Err)
	}
	if f.Incompatible() != 1 || f.Applied() != 7 {
		t.Fatalf("incompatible %d applied %d, want 1 and 7", f.Incompatible(), f.Applied())
	}
}
