package meta

import (
	"errors"
	"reflect"
	"testing"
)

// Codec version 9 is a Commit of a rewrite, a Trash with Expect and a Rename with
// NoReplace, and nothing else: each round-trips with the fields of the versions it
// shares a branch with (a Commit's Sums, a Trash's Own and Recursive), plain commands
// stay at their old versions, and a version-9 entry that carries no codec-9 field, or
// of any other op, is refused on decode so the bytes stay canonical (I-22).
// Invariant: I-223, I-224, I-225
func TestEncodeCommand_RewriteIsCodec9(t *testing.T) {
	sums := []ChunkDesc{{Hash: Hash{0xbb}, Size: 64, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}, Sums: []Hash{{3}, {4}}}}
	for _, c := range []struct {
		name string
		cmd  Command
		want uint8
	}{
		{"plain commit", Command{Op: OpCommit, Commit: &Commit{Ino: 2, NewSize: 1, Now: 1}}, commandVersion},
		{"commit with sums", Command{Op: OpCommit, Commit: &Commit{Ino: 2, NewSize: 64, NewChunks: sums, Now: 1}}, commandVersionV5},
		{"rewrite start", Command{Op: OpCommit, Commit: &Commit{Ino: 2, HasTruncate: true, NewSize: 64, NewChunks: sums, Now: 1, RewriteStart: true}}, commandVersionV9},
		{"rewrite continued with sums", Command{Op: OpCommit, Commit: &Commit{Ino: 2, BaseVersion: 3, NewSize: 64, NewChunks: sums, Now: 1, Rewrite: 3}}, commandVersionV9},
		{"plain trash", Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "a", Recursive: true, Now: 1}}, commandVersionV4},
		{"own trash", Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "a", Own: true, Now: 1}}, commandVersionV8},
		{"trash expecting", Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "a", Recursive: true, Now: 1, Expect: 9}}, commandVersionV9},
		{"own trash expecting", Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "a", Own: true, Now: 1, Expect: 9}}, commandVersionV9},
		{"plain rename", Command{Op: OpRename, Rename: &Rename{OldParent: 1, OldName: "a", NewParent: 1, NewName: "b", Now: 1}}, commandVersion},
		{"rename without replace", Command{Op: OpRename, Rename: &Rename{OldParent: 1, OldName: "a", NewParent: 1, NewName: "b", Now: 1, NoReplace: true}}, commandVersionV9},
	} {
		if v := CommandCodec(c.cmd); v != c.want {
			t.Errorf("%s: version %d, want %d", c.name, v, c.want)
		}
		dec, err := DecodeCommand(MustEncodeCommand(c.cmd))
		if err != nil || !reflect.DeepEqual(dec, c.cmd) {
			t.Errorf("%s: round trip: %v\n got %+v\nwant %+v", c.name, err, dec, c.cmd)
		}
	}
	// a version-9 byte on an entry that needs none of its fields is refused
	for _, c := range []Command{
		{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}},
		{Op: OpCreate, Create: &Create{Parent: 1, Name: "a", Type: TFile, Now: 2}},
	} {
		b := MustEncodeCommand(c)
		b[0] = commandVersionV9
		if _, err := DecodeCommand(b); err == nil {
			t.Errorf("a version-9 %d was accepted", c.Op)
		}
	}
	// a version-9 Commit, Trash or Rename whose codec-9 field is empty is not canonical
	for _, c := range []Command{
		{Op: OpCommit, Commit: &Commit{Ino: 2, NewSize: 1, Now: 1, Rewrite: 1}},
		{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "a", Now: 1, Expect: 1}},
		{Op: OpRename, Rename: &Rename{OldParent: 1, OldName: "a", NewParent: 1, NewName: "b", Now: 1, NoReplace: true}},
	} {
		b := MustEncodeCommand(c)
		// clear the codec-9 field's bytes: the last 8 for Commit (RewriteStart is false)
		// and Trash, the last byte for Rename
		switch c.Op { //nolint:exhaustive // the three codec-9 ops
		case OpCommit:
			copy(b[len(b)-9:], make([]byte, 8))
		case OpTrash:
			copy(b[len(b)-8:], make([]byte, 8))
		case OpRename:
			b[len(b)-1] = 0
		}
		if _, err := DecodeCommand(b); err == nil {
			t.Errorf("a version-9 %d without its codec-9 field was accepted", c.Op)
		}
	}
	if CodecVersion < commandVersionV9 || CodecRewrite != commandVersionV9 {
		t.Fatalf("CodecVersion %d, CodecRewrite %d: version 9 is not written", CodecVersion, CodecRewrite)
	}
}

// A RewriteStart's result carries the version it produced, the gateway's rewrite id;
// a result without one is byte for byte what an older node writes and reads.
// Invariant: I-223
func TestResult_VersionRoundTrip(t *testing.T) {
	for _, res := range []Result{{Version: 9}, {SnapID: 2, Version: 9}, {SnapID: 7}, {Ino: 3, Gen: 4}, {Err: ErrSuperseded}} {
		got, err := DecodeResult(EncodeResult(res))
		if err != nil || got.Version != res.Version || got.SnapID != res.SnapID || got.Ino != res.Ino || !errors.Is(got.Err, res.Err) {
			t.Fatalf("result %+v round trip: %+v %v", res, got, err)
		}
	}
	if b := EncodeResult(Result{Ino: 3}); len(b) != 1+8+4+1 {
		t.Fatalf("result without a SnapID or a Version is %d bytes", len(b))
	}
}

// The rewrite rule (docs/02 §Apply rules, docs/04 §6): a RewriteStart applies over any
// version, truncating to 0, and becomes the file's rewrite; a Commit continuing a rewrite
// that another start has replaced is ErrSuperseded and changes nothing; a continuation
// of the current rewrite still checks BaseVersion; a plain Commit leaves the rewrite
// alone; a malformed start is ErrBadCommand.
// Invariant: I-223
func TestFSM_Commit_Rewrite_Table(t *testing.T) {
	tf := newTFSM(t)
	f := tf.create(RootIno, "f")
	tf.commitOne(f, sha("old"), true)
	version := func() uint64 { return tf.f.state().Inodes[f].Version }
	rewriteAt := func() uint64 { return tf.f.state().Inodes[f].RewriteAt }
	start := func(base uint64, h Hash) Command {
		return Command{Op: OpCommit, Commit: &Commit{
			Ino: f, BaseVersion: base, HasTruncate: true, NewSize: testCS,
			Writes: []ChunkWrite{{0, h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 2, RewriteStart: true,
		}}
	}
	more := func(base, rewrite uint64, h Hash) Command {
		return Command{Op: OpCommit, Commit: &Commit{
			Ino: f, BaseVersion: base, NewSize: 2 * testCS,
			Writes: []ChunkWrite{{1, h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 3, Rewrite: rewrite,
		}}
	}
	stale := version()

	// gateway A starts a rewrite from a version that is already current
	a := tf.ok(start(stale, sha("a0"))).Version
	if a == 0 || a != version() || rewriteAt() != a {
		t.Fatalf("A's start: result %d, version %d, rewrite at %d", a, version(), rewriteAt())
	}
	// gateway B starts its own from the version it planned on, which is stale now: it applies
	b := tf.ok(start(stale, sha("b0"))).Version
	if b <= a || rewriteAt() != b || tf.f.state().Inodes[f].Chunks[0] != sha("b0") || tf.f.state().Inodes[f].Size != testCS {
		t.Fatalf("B's start over A's: result %d, rewrite at %d, inode %+v", b, rewriteAt(), tf.f.state().Inodes[f])
	}
	// A continues its rewrite: superseded, nothing changes, even at the current version
	tf.fails(more(version(), a, sha("a1")), ErrSuperseded)
	// B continues its rewrite, at the current version
	tf.ok(more(version(), b, sha("b1")))
	if in := tf.f.state().Inodes[f]; in.Chunks[0] != sha("b0") || in.Chunks[1] != sha("b1") || in.RewriteAt != b {
		t.Fatalf("after B's continuation: %+v", in)
	}
	// B's continuation from a stale version is a conflict, as any Commit's
	tf.fails(more(version()-1, b, sha("b2")), ErrConflict)
	// a plain Commit (another writer, or a gateway before codec 9) leaves the rewrite alone
	tf.ok(Command{Op: OpCommit, Commit: &Commit{
		Ino: f, BaseVersion: version(), NewSize: 2 * testCS,
		Writes: []ChunkWrite{{1, sha("plain")}}, NewChunks: []ChunkDesc{desc(sha("plain"))}, Now: 4,
	}})
	if rewriteAt() != b {
		t.Fatalf("a plain Commit moved the rewrite to %d", rewriteAt())
	}
	// a continuation of a rewrite the file never had is superseded
	g := tf.create(RootIno, "g")
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: g, NewSize: 1, Writes: []ChunkWrite{{0, HashZero}}, Now: 5, Rewrite: 7}}, ErrSuperseded)
	// malformed starts
	for _, c := range []*Commit{
		{Ino: f, NewSize: testCS, Now: 5, RewriteStart: true},                                   // no truncate
		{Ino: f, HasTruncate: true, TruncateTo: 1, NewSize: testCS, Now: 5, RewriteStart: true}, // not to 0
		{Ino: f, HasTruncate: true, NewSize: testCS, Now: 5, RewriteStart: true, Rewrite: b},    // both
	} {
		tf.fails(Command{Op: OpCommit, Commit: c}, ErrBadCommand)
	}
}

// A Trash with Expect removes the file the proposer means or nothing: a name that now
// holds another inode is ErrConflict and changes nothing, under a protected folder too,
// where the own cleanup of the right file still goes through.
// Invariant: I-225
func TestFSM_Trash_Expect_Mismatch(t *testing.T) {
	tf := newTFSM(t)
	p := tf.mkdir(RootIno, "p")
	mine := tf.create(p, "copy.bin")
	// another client renames it away and creates a new file under its name
	tf.ok(Command{Op: OpRename, Rename: &Rename{OldParent: p, OldName: "copy.bin", NewParent: p, NewName: "kept.bin", Now: 2}})
	theirs := tf.create(p, "copy.bin")
	tf.flags(p, FlagProtected)
	tf.fails(Command{Op: OpTrash, Trash: &Trash{Parent: p, Name: "copy.bin", Own: true, Now: 3, Expect: mine}}, ErrConflict)
	tf.fails(Command{Op: OpTrash, Trash: &Trash{Parent: p, Name: "copy.bin", Now: 3, Expect: mine}}, ErrConflict)
	// the right file, under its new name, as its own cleanup
	tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: p, Name: "kept.bin", Own: true, Now: 3, Expect: mine}})
	if _, ok := tf.f.state().Inodes[theirs]; !ok || tf.child(p, "copy.bin") != theirs {
		t.Fatal("the other client's file did not survive")
	}
	if roots := tf.f.TrashOf(p); len(roots) != 1 || roots[0].Ino != mine {
		t.Fatalf("trash: %+v", roots)
	}
}

// A Rename with NoReplace onto an existing name is ErrExist and changes nothing, whatever
// a gateway's lookup saw; onto a free name, or onto itself, it is an ordinary rename.
// Invariant: I-224, I-226
func TestFSM_Rename_NoReplace_Exist(t *testing.T) {
	tf := newTFSM(t)
	a := tf.create(RootIno, "a")
	b := tf.create(RootIno, "b")
	tf.fails(Command{Op: OpRename, Rename: &Rename{OldParent: RootIno, OldName: "a", NewParent: RootIno, NewName: "b", Now: 2, NoReplace: true}}, ErrExist)
	tf.ok(Command{Op: OpRename, Rename: &Rename{OldParent: RootIno, OldName: "a", NewParent: RootIno, NewName: "a", Now: 2, NoReplace: true}})
	tf.ok(Command{Op: OpRename, Rename: &Rename{OldParent: RootIno, OldName: "a", NewParent: RootIno, NewName: "c", Now: 2, NoReplace: true}})
	if tf.child(RootIno, "c") != a || tf.child(RootIno, "b") != b {
		t.Fatal("rename without replace moved the wrong file")
	}
	if len(tf.f.Trash()) != 0 {
		t.Fatal("a rename without replace put something in the trash")
	}
}
