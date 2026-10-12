package meta

import (
	"bytes"
	"crypto/sha256"
	"reflect"
	"testing"

	"github.com/zombar/tailraid/internal/ec"
)

// shardSums hashes the data shards of a chunk encoded at (k, m): what the flusher
// records and the read path checks a shard against (I-201).
func shardSums(t *testing.T, data []byte, k, m int) []Hash {
	t.Helper()
	codec, err := ec.New(k, m)
	if err != nil {
		t.Fatal(err)
	}
	shards, err := codec.Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	sums := make([]Hash, k)
	for j := range k {
		sums[j] = sha256.Sum256(shards[j])
	}
	return sums
}

// SetShardSums records a chunk's data-shard hashes once: an unknown chunk, a list whose
// length is not k, and sums that differ from recorded ones are refused and change
// nothing; equal sums are a no-op; refs, placement and the tombstone are untouched.
// Invariant: I-201
func TestFSM_SetShardSums_Table(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a := sha("a")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
	before, _ := tf.f.GetChunk(a)
	if before.Sums != nil {
		t.Fatalf("a commit without sums recorded %v", before.Sums)
	}
	sums := []Hash{sha("s0"), sha("s1")} // k = 2
	tf.fails(Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: sha("nope"), Sums: sums}}, ErrUnknownChunk)
	for _, n := range []int{0, 1, 3} { // {0, 1, k−1, k+1}: only k is legal
		bad := make([]Hash, n)
		for i := range bad {
			bad[i] = sha(string(rune('A' + i)))
		}
		tf.fails(Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: a, Sums: bad}}, ErrBadCommand)
	}
	tf.ok(Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: a, Sums: sums}})
	c, _ := tf.f.GetChunk(a)
	if !reflect.DeepEqual(c.Sums, sums) {
		t.Fatalf("sums %v, want %v", c.Sums, sums)
	}
	if c.Refs != before.Refs || !reflect.DeepEqual(c.Placement, before.Placement) || c.TombstonedAt != before.TombstonedAt {
		t.Fatalf("SetShardSums changed more than the sums: %+v vs %+v", c, before)
	}
	tf.ok(Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: a, Sums: sums}}) // equal: a no-op
	tf.fails(Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: a, Sums: []Hash{sha("other"), sha("s1")}}}, ErrConflict)
}

// A Commit carries a chunk's sums (len k) or none; a later Commit of the same chunk (a
// dedup hit from another writer) fills sums the chunk lacked, equal sums are a no-op and
// different ones are a conflict; the sums are a pure function of the chunk's bytes, so two
// encodes and two replicas agree byte for byte.
// Invariant: I-201
func TestFSM_Commit_SumsPureFunctionOfChunk(t *testing.T) {
	data := make([]byte, testCS)
	for i := range data {
		data[i] = byte(i * 7)
	}
	a := sha256.Sum256(data)
	s1, s2 := shardSums(t, data, 2, 1), shardSums(t, data, 2, 1)
	if !reflect.DeepEqual(s1, s2) {
		t.Fatal("two encodes gave different data-shard sums")
	}
	withSums := func(d ChunkDesc, sums []Hash) ChunkDesc { d.Sums = sums; return d }
	run := func() *FSM {
		tf := newTFSM(t)
		f := tf.create(RootIno, "f")
		g := tf.create(RootIno, "g")
		tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{withSums(desc(a), s1[:1])}, Now: 1}}, ErrBadCommand)
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
		if c, _ := tf.f.GetChunk(a); c.Sums != nil {
			t.Fatalf("sums without any: %v", c.Sums)
		}
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: g, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{withSums(desc(a), s1)}, Now: 2}})
		if c, _ := tf.f.GetChunk(a); !reflect.DeepEqual(c.Sums, s1) || c.Refs != 2 {
			t.Fatalf("after the dedup commit with sums: %+v", c)
		}
		tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: g, BaseVersion: 1, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{withSums(desc(a), s2)}, Now: 3}})
		tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: g, BaseVersion: 2, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{withSums(desc(a), []Hash{sha("x"), sha("y")})}, Now: 4}}, ErrConflict)
		return tf.f
	}
	x, y := run(), run()
	bx, by := encodeState(t, x.state()), encodeState(t, y.state())
	if !bytes.Equal(bx, by) {
		t.Fatal("two replicas disagree after the same commits")
	}
	if c, _ := x.GetChunk(a); !reflect.DeepEqual(c.Sums, s1) {
		t.Fatalf("sums %v, want %v", c.Sums, s1)
	}
}

// A version-4 snapshot decodes as before with no sums; a version-5 one carries them.
// Invariant: I-201
func TestCodec_ReadsVersion4Snapshot(t *testing.T) {
	s := sampleState()
	var buf bytes.Buffer
	if err := encodeStateVersion(&buf, s, stateVersionV4); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want := withoutRewrites(s.Clone())
	for _, c := range want.Chunks {
		c.Sums = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v4 round trip\n got %+v\nwant %+v", got, want)
	}
	buf.Reset()
	if err := EncodeState(&buf, s); err != nil {
		t.Fatal(err)
	}
	got, err = DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("v5 round trip lost the sums\n got %+v\nwant %+v", got, s)
	}
}

// Codec version 5 is written only for a Commit whose chunks carry sums and for
// SetShardSums; a Commit without sums keeps its old version, and a version-5 entry of any
// other op, or a SetShardSums in an older version, is rejected on decode.
// Invariant: I-201
func TestEncodeCommand_V5Gating(t *testing.T) {
	a := sha("a")
	plain := Command{Op: OpCommit, Commit: &Commit{Ino: 2, NewSize: 1, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}}
	if v := CommandCodec(plain); v != commandVersion {
		t.Fatalf("a Commit without sums is version %d, want %d", v, commandVersion)
	}
	withSums := plain
	withSums.Commit = &Commit{Ino: 2, NewSize: 1, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{{Hash: a, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}, Sums: []Hash{sha("0"), sha("1")}}}, Now: 1}
	if v := CommandCodec(withSums); v != commandVersionV5 {
		t.Fatalf("a Commit with sums is version %d, want %d", v, commandVersionV5)
	}
	set := Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: a, Sums: []Hash{sha("0"), sha("1")}}}
	if v := CommandCodec(set); v != commandVersionV5 {
		t.Fatalf("SetShardSums is version %d, want %d", v, commandVersionV5)
	}
	for _, c := range []Command{withSums, set} {
		b := MustEncodeCommand(c)
		dec, err := DecodeCommand(b)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalizeCommand(dec), normalizeCommand(c)) {
			t.Fatalf("round trip\n got %+v\nwant %+v", dec, c)
		}
	}
	unlink := MustEncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}})
	unlink[0] = commandVersionV5
	if _, err := DecodeCommand(unlink); err == nil {
		t.Fatal("a version-5 Unlink was accepted")
	}
	old := MustEncodeCommand(set)
	old[0] = commandVersionV4
	if _, err := DecodeCommand(old); err == nil {
		t.Fatal("a version-4 SetShardSums was accepted")
	}
}

// A SetPlacement that changes (k, m) re-encodes the chunk: its recorded sums no longer
// describe its shards and are cleared for the backfill to record again; one that keeps
// the policy keeps them.
// Invariant: I-201
func TestFSM_SetPlacement_PolicyChangeClearsSums(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	a := sha("a")
	d := desc(a)
	d.Sums = []Hash{sha("s0"), sha("s1")}
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{d}, Now: 1}})
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: a, K: 2, M: 1, Placement: []NodeIdx{3, 2, 1}}})
	if c, _ := tf.f.GetChunk(a); !reflect.DeepEqual(c.Sums, d.Sums) {
		t.Fatalf("a move within the policy lost the sums: %v", c.Sums)
	}
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: a, K: 2, M: 2, Placement: []NodeIdx{1, 2, 3, 0}}})
	if c, _ := tf.f.GetChunk(a); c.Sums != nil {
		t.Fatalf("a re-encode kept the old sums: %v", c.Sums)
	}
}
