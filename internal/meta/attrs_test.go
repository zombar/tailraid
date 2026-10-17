package meta

import (
	"errors"
	"reflect"
	"testing"
)

// A Commit carrying a client's attributes is codec 16 and only it: it round-trips, a
// Commit without them stays at its old version, a version-16 Commit without attributes
// or with an unknown mask bit is refused (canonical bytes, I-22), version 16 holds no
// other op, and a Commit with attributes and sums or a rewrite keeps every field.
// Invariant: I-401
func TestEncodeCommand_V16Attrs(t *testing.T) {
	attrs := CommitAttrs{Mask: AttrMode | AttrUid | AttrGid | AttrAtime | AttrMtime, Mode: 0o640, Uid: 502, Gid: 20, Atime: -5, Mtime: 1_700_000_000_123}
	plain := Command{Op: OpCommit, Commit: &Commit{Ino: 9, BaseVersion: 2, NewSize: 10, Writes: []ChunkWrite{{Idx: 0, Hash: sha("a")}}, Now: 7}}
	if v := CommandCodec(plain); v != commandVersion {
		t.Fatalf("a plain Commit is version %d", v)
	}
	with := plain
	c := *plain.Commit
	c.Attrs = attrs
	with.Commit = &c
	if v := CommandCodec(with); v != commandVersionV16 || CodecAttrs != commandVersionV16 || CodecVersion < commandVersionV16 {
		t.Fatalf("a Commit with attributes is version %d, want 16", v)
	}
	b := MustEncodeCommand(with)
	if dec, err := DecodeCommand(b); err != nil || !reflect.DeepEqual(dec, with) {
		t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, dec.Commit, with.Commit)
	}
	// every field of the older versions rides along
	full := with
	fc := c
	fc.NewChunks = []ChunkDesc{{Hash: sha("a"), Size: 10, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}, Sums: []Hash{sha("s1"), sha("s2")}}}
	fc.Rewrite = 4
	full.Commit = &fc
	if dec, err := DecodeCommand(MustEncodeCommand(full)); err != nil || !reflect.DeepEqual(dec, full) {
		t.Fatalf("round trip with sums and a rewrite: %v %+v", err, dec.Commit)
	}
	// and under the users header
	actor := with
	actor.Actor = 3
	if dec, err := DecodeCommand(MustEncodeCommand(actor)); err != nil || !reflect.DeepEqual(dec, actor) || CommandCodec(actor) != commandVersionV16 {
		t.Fatalf("with an actor: %v %+v (codec %d)", err, dec, CommandCodec(actor))
	}
	// a mask with a bit a Commit does not carry (flags, unknown) is a bad command
	for _, mask := range []uint8{AttrFlags, 1 << 7, AttrMode | AttrFlags} {
		cc := c
		cc.Attrs = CommitAttrs{Mask: mask}
		if _, err := EncodeCommand(Command{Op: OpCommit, Commit: &cc}); !errors.Is(err, ErrBadCommand) {
			t.Fatalf("mask %#x encoded: %v", mask, err)
		}
	}
	if CommandCodec(with) <= CodecHeal {
		t.Fatal("a codec-15 member could read a Commit with attributes")
	}
	stripped := MustEncodeCommand(plain)
	stripped[0] = commandVersionV16
	if _, err := DecodeCommand(stripped); err == nil {
		t.Fatal("a version-16 Commit without attributes was accepted")
	}
	u := MustEncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}})
	u[0] = commandVersionV16
	if _, err := DecodeCommand(u); err == nil {
		t.Fatal("a version-16 Unlink was accepted")
	}
}

// The FSM applies a Commit's attributes after the Commit's own stamp: the file's mode,
// owner and times are the client's, ctime the Commit's; a mask sets only its fields.
// Invariant: I-401
func TestFSM_Commit_AppliesAttrs(t *testing.T) {
	cases := []struct {
		name  string
		attrs CommitAttrs
		check func(t *testing.T, in Inode)
	}{
		{"every field", CommitAttrs{Mask: AttrMode | AttrUid | AttrGid | AttrAtime | AttrMtime, Mode: 0o600, Uid: 7, Gid: 8, Atime: 11, Mtime: 12}, func(t *testing.T, in Inode) {
			if in.Mode&0o777 != 0o600 || in.Uid != 7 || in.Gid != 8 || in.Atime != 11 || in.Mtime != 12 || in.Ctime != 100 {
				t.Fatalf("inode %+v", in)
			}
		}},
		{"times only", CommitAttrs{Mask: AttrAtime | AttrMtime, Atime: 21, Mtime: 22}, func(t *testing.T, in Inode) {
			if in.Mode&0o777 != 0o644 || in.Mtime != 22 || in.Atime != 21 || in.Ctime != 100 {
				t.Fatalf("inode %+v", in)
			}
		}},
		{"mode only: the Commit's mtime stays", CommitAttrs{Mask: AttrMode, Mode: 0o755}, func(t *testing.T, in Inode) {
			if in.Mode&0o777 != 0o755 || in.Mtime != 100 {
				t.Fatalf("inode %+v", in)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tf := newTFSM(t)
			ino := tf.create(RootIno, "f")
			h := sha("data")
			res := tf.apply(Command{Op: OpCommit, Commit: &Commit{Ino: ino, NewSize: 10, Writes: []ChunkWrite{{Idx: 0, Hash: h}}, NewChunks: []ChunkDesc{desc(h)}, Now: 100, Attrs: c.attrs}})
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			c.check(t, tf.inode(ino))
		})
	}
}
