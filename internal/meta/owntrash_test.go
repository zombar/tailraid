package meta

import (
	"reflect"
	"testing"
)

// Codec version 8 is a Trash with Own and nothing else: the version is the flag, so a
// plain Trash stays at 4, the round trip keeps Own, and a version-8 entry of any other op
// is rejected on decode.
// Invariant: I-220
func TestEncodeCommand_OwnTrashIsCodec8(t *testing.T) {
	plain := Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "a", Now: 1}}
	if v := CommandCodec(plain); v != commandVersionV4 {
		t.Fatalf("a plain Trash is version %d, want 4", v)
	}
	own := Command{Op: OpTrash, Trash: &Trash{Parent: 1, Name: "a", Own: true, Now: 1}}
	if v := CommandCodec(own); v != commandVersionV8 {
		t.Fatalf("an own cleanup is version %d, want 8", v)
	}
	for _, c := range []Command{plain, own} {
		dec, err := DecodeCommand(MustEncodeCommand(c))
		if err != nil || !reflect.DeepEqual(dec, c) {
			t.Fatalf("round trip of %+v: %v %+v", c.Trash, err, dec.Trash)
		}
	}
	unlink := MustEncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a", Now: 2}})
	unlink[0] = commandVersionV8
	if _, err := DecodeCommand(unlink); err == nil {
		t.Fatal("a version-8 Unlink was accepted")
	}
	if CodecVersion < commandVersionV8 {
		t.Fatalf("CodecVersion %d does not write version 8", CodecVersion)
	}
}

// Under a protected folder an own cleanup trashes a file, written or not, and nothing
// else: a directory is still refused with Own, every delete without Own is refused, and
// outside protection Own changes nothing.
// Invariant: I-220
func TestFSM_Protected_OwnCleanup_Table(t *testing.T) {
	tf := newTFSM(t)
	p := tf.mkdir(RootIno, "p")
	sub := tf.mkdir(p, "sub")
	written := tf.create(sub, "written")
	tf.commitOne(written, sha("w"), true)
	tf.create(sub, "empty")
	tf.mkdir(sub, "dir")
	tf.create(RootIno, "outside")
	tf.flags(p, FlagProtected)
	trash := func(parent Ino, name string, own, recursive bool) Command {
		return Command{Op: OpTrash, Trash: &Trash{Parent: parent, Name: name, Own: own, Recursive: recursive, Now: 2}}
	}
	for _, c := range []struct {
		name string
		cmd  Command
		want error
	}{
		{"a written file without Own", trash(sub, "written", false, false), ErrProtected},
		{"an empty file without Own", trash(sub, "empty", false, false), ErrProtected},
		{"a directory with Own", trash(sub, "dir", true, false), ErrProtected},
		{"the protected folder with Own", trash(RootIno, "p", true, true), ErrProtected},
		{"a written file with Own", trash(sub, "written", true, false), nil},
		{"an empty file with Own", trash(sub, "empty", true, false), nil},
		{"outside protection with Own", trash(RootIno, "outside", true, false), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.want != nil {
				tf.fails(c.cmd, c.want)
				return
			}
			tf.ok(c.cmd)
			if _, live := tf.inode(c.cmd.Trash.Parent).Children[c.cmd.Trash.Name]; live {
				t.Fatal("the name is still live after the own cleanup")
			}
		})
	}
	if roots := len(tf.f.TrashOf(sub)); roots != 2 {
		t.Fatalf("trash roots under sub: %d, want the two own cleanups", roots)
	}
}
