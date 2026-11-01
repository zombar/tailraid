package meta

import (
	"bytes"
	"reflect"
	"testing"
)

// A Commit whose chunks carry block roots and a SetShardSums with roots are codec 18 and
// round-trip with the kind, a Commit of a rewrite with attributes among them; a Commit
// with whole-shard sums keeps codec 5; a codec-18 entry of another op, a codec-18
// SetShardSums without roots and a codec-18 Commit without any are refused on decode.
// Invariant: I-422
func TestEncodeCommand_V18SumsKind(t *testing.T) {
	a := sha("a")
	roots := func() ChunkDesc {
		return ChunkDesc{Hash: a, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}, Sums: []Hash{sha("0"), sha("1")}, SumsKind: SumsBlocks}
	}
	flat := roots()
	flat.SumsKind = SumsShard
	commit := func(d ChunkDesc) Command {
		return Command{Op: OpCommit, Commit: &Commit{Ino: 2, NewSize: 1, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{d}, Now: 1}}
	}
	if v := CommandCodec(commit(flat)); v != commandVersionV5 {
		t.Fatalf("a Commit with whole-shard sums is version %d, want %d", v, commandVersionV5)
	}
	kindless := roots()
	kindless.Sums = nil // a kind without sums is nothing: the plain Commit of a chunk without sums
	if v := CommandCodec(commit(kindless)); v != commandVersion {
		t.Fatalf("a Commit with a kind and no sums is version %d, want %d", v, commandVersion)
	}
	withRoots := commit(roots())
	if v := CommandCodec(withRoots); v != commandVersionV18 {
		t.Fatalf("a Commit with roots is version %d, want %d", v, commandVersionV18)
	}
	rewrite := commit(roots())
	rewrite.Commit.Rewrite, rewrite.Commit.Attrs = 7, CommitAttrs{Mask: AttrMode, Mode: 0o600}
	set := Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: a, Sums: []Hash{sha("0"), sha("1")}, SumsKind: SumsBlocks}}
	if v := CommandCodec(set); v != commandVersionV18 {
		t.Fatalf("a SetShardSums with roots is version %d, want %d", v, commandVersionV18)
	}
	for _, c := range []Command{withRoots, rewrite, set, commit(flat)} {
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
	unlink[0] = commandVersionV18
	if _, err := DecodeCommand(unlink); err == nil {
		t.Fatal("a version-18 Unlink was accepted")
	}
	raw := MustEncodeCommand(set)
	raw[len(raw)-1] = SumsShard // the kind byte is the last
	if _, err := DecodeCommand(raw); err == nil {
		t.Fatal("a version-18 SetShardSums without roots was accepted")
	}
	raw[len(raw)-1] = SumsBlocks + 1
	if _, err := DecodeCommand(raw); err == nil {
		t.Fatal("a SetShardSums with an unknown kind was accepted")
	}
	old := MustEncodeCommand(commit(flat))
	old[0] = commandVersionV18 // the bytes of a codec-5 Commit, a kind byte short
	if _, err := DecodeCommand(old); err == nil {
		t.Fatal("a codec-5 Commit relabelled 18 was accepted")
	}
}

// SetShardSums fills missing sums of either kind, takes whole-shard sums to roots, keeps
// roots when whole-shard sums arrive, answers equal sums with nothing, refuses sums that
// differ within a kind and an unknown kind.
// Invariant: I-422
func TestFSM_SetShardSums_KindTable(t *testing.T) {
	flat, flat2 := []Hash{sha("s0"), sha("s1")}, []Hash{sha("t0"), sha("s1")}
	roots, roots2 := []Hash{sha("r0"), sha("r1")}, []Hash{sha("q0"), sha("r1")}
	type step struct {
		sums []Hash
		kind uint8
		err  error
	}
	type want struct {
		sums []Hash
		kind uint8
	}
	for _, tc := range []struct {
		name  string
		steps []step
		want  want
	}{
		{"missing to whole-shard", []step{{flat, SumsShard, nil}}, want{flat, SumsShard}},
		{"missing to roots", []step{{roots, SumsBlocks, nil}}, want{roots, SumsBlocks}},
		{"whole-shard to roots", []step{{flat, SumsShard, nil}, {roots, SumsBlocks, nil}}, want{roots, SumsBlocks}},
		{"roots stay over whole-shard", []step{{roots, SumsBlocks, nil}, {flat, SumsShard, nil}}, want{roots, SumsBlocks}},
		{"equal whole-shard is nothing", []step{{flat, SumsShard, nil}, {flat, SumsShard, nil}}, want{flat, SumsShard}},
		{"equal roots are nothing", []step{{roots, SumsBlocks, nil}, {roots, SumsBlocks, nil}}, want{roots, SumsBlocks}},
		{"different whole-shard conflict", []step{{flat, SumsShard, nil}, {flat2, SumsShard, ErrConflict}}, want{flat, SumsShard}},
		{"different roots conflict", []step{{roots, SumsBlocks, nil}, {roots2, SumsBlocks, ErrConflict}}, want{roots, SumsBlocks}},
		{"unknown kind", []step{{roots, SumsBlocks + 1, ErrBadCommand}}, want{nil, SumsShard}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tf := newTFSM(t)
			f := tf.create(RootIno, "f")
			a := sha("a")
			tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{desc(a)}, Now: 1}})
			for _, s := range tc.steps {
				cmd := Command{Op: OpSetShardSums, SetShardSums: &SetShardSums{Hash: a, Sums: s.sums, SumsKind: s.kind}}
				if s.err != nil {
					tf.fails(cmd, s.err)
				} else {
					tf.ok(cmd)
				}
			}
			c, _ := tf.f.GetChunk(a)
			if !reflect.DeepEqual(c.Sums, tc.want.sums) || c.SumsKind != tc.want.kind {
				t.Fatalf("sums %v kind %d, want %v kind %d", c.Sums, c.SumsKind, tc.want.sums, tc.want.kind)
			}
		})
	}
}

// A Commit registers a chunk with sums of either kind; a later Commit of the same chunk
// takes whole-shard sums to roots and never the reverse; an unknown kind is refused; a
// re-encode clears the kind with the sums.
// Invariant: I-422
func TestFSM_Commit_SumsKindUpgrade(t *testing.T) {
	tf := newTFSM(t)
	f, g, h, i, j := tf.create(RootIno, "f"), tf.create(RootIno, "g"), tf.create(RootIno, "h"), tf.create(RootIno, "i"), tf.create(RootIno, "j")
	a := sha("a")
	flat, roots := desc(a), desc(a)
	flat.Sums = []Hash{sha("s0"), sha("s1")}
	roots.Sums, roots.SumsKind = []Hash{sha("r0"), sha("r1")}, SumsBlocks
	commit := func(ino Ino, d ChunkDesc) Command {
		return Command{Op: OpCommit, Commit: &Commit{Ino: ino, NewSize: testCS, Writes: []ChunkWrite{{0, a}}, NewChunks: []ChunkDesc{d}, Now: 1}}
	}
	tf.ok(commit(f, flat))
	tf.ok(commit(g, roots)) // a dedup hit from a gateway recording roots upgrades
	if c, _ := tf.f.GetChunk(a); !reflect.DeepEqual(c.Sums, roots.Sums) || c.SumsKind != SumsBlocks {
		t.Fatalf("roots did not replace whole-shard sums: %v kind %d", c.Sums, c.SumsKind)
	}
	tf.ok(commit(h, flat)) // an older gateway's whole-shard sums leave the roots
	if c, _ := tf.f.GetChunk(a); !reflect.DeepEqual(c.Sums, roots.Sums) || c.SumsKind != SumsBlocks {
		t.Fatalf("whole-shard sums replaced roots: %v kind %d", c.Sums, c.SumsKind)
	}
	other := desc(a)
	other.Sums, other.SumsKind = []Hash{sha("x0"), sha("r1")}, SumsBlocks
	tf.fails(commit(i, other), ErrConflict)
	b := sha("b")
	bad := desc(b)
	bad.Sums, bad.SumsKind = []Hash{sha("y0"), sha("y1")}, SumsBlocks+1
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: j, NewSize: testCS, Writes: []ChunkWrite{{0, b}}, NewChunks: []ChunkDesc{bad}, Now: 1}}, ErrBadCommand)
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: a, K: 2, M: 2, Placement: []NodeIdx{1, 2, 3, 0}}})
	if c, _ := tf.f.GetChunk(a); c.Sums != nil || c.SumsKind != SumsShard {
		t.Fatalf("a re-encode kept sums %v kind %d", c.Sums, c.SumsKind)
	}
}

// A snapshot written before version 17 reads with every chunk's sums as whole-shard
// sums; the current version carries the kind.
// Invariant: I-422
func TestCodec_ReadsVersion16SnapshotAsShardSums(t *testing.T) {
	s := sampleState()
	var buf bytes.Buffer
	if err := encodeStateVersion(&buf, s, stateVersionV16); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want := s.Clone()
	for _, c := range want.Chunks {
		c.SumsKind = SumsShard
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v16 round trip\n got %+v\nwant %+v", got, want)
	}
	buf.Reset()
	if err := EncodeState(&buf, s); err != nil {
		t.Fatal(err)
	}
	if got, err = DecodeState(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("v17 round trip lost the kind\n got %+v\nwant %+v", got, s)
	}
}
