package meta

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// An entry whose path from the root would exceed MaxPathLen is refused by Create, Mkdir,
// Symlink and Rename with ErrNameTooLong; one byte under and exactly at the limit are
// applied. The table sits at the boundary with a 3941-byte directory and a final name of
// 154, 155 and 156 bytes.
//
// Invariant: I-115
func TestFSM_PathLengthBoundaries(t *testing.T) {
	tf := newTFSM(t)
	dir := RootIno
	for i := range 15 {
		dir = tf.mkdir(dir, strings.Repeat(string(rune('a'+i)), MaxNameLen))
	}
	dir = tf.mkdir(dir, strings.Repeat("p", 100))
	const base = 15*(1+MaxNameLen) + 1 + 100 // 3941
	for _, c := range []struct {
		total int
		want  error
	}{{MaxPathLen - 1, nil}, {MaxPathLen, nil}, {MaxPathLen + 1, ErrNameTooLong}} {
		for _, kind := range []string{"f", "d", "s", "r"} {
			name := kind + strings.Repeat("n", c.total-base-2)
			if got := 1 + len(name) + base; got != c.total {
				t.Fatalf("path length %d, want %d", got, c.total)
			}
			var cmd Command
			switch kind {
			case "f":
				cmd = Command{Op: OpCreate, Create: &Create{Parent: dir, Name: name, Type: TFile, Mode: 0o644, Now: 1}}
			case "d":
				cmd = Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: dir, Name: name, Mode: 0o755, Now: 1}}
			case "s":
				cmd = Command{Op: OpCreate, Create: &Create{Parent: dir, Name: name, Type: TSymlink, Target: "/x", Mode: 0o777, Now: 1}}
			case "r":
				src := fmt.Sprintf("src%d", c.total)
				tf.ok(Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: src, Type: TFile, Mode: 0o644, Now: 1}})
				cmd = Command{Op: OpRename, Rename: &Rename{OldParent: RootIno, OldName: src, NewParent: dir, NewName: name, Now: 1}}
			}
			if r := tf.apply(cmd); !errors.Is(r.Err, c.want) {
				t.Fatalf("%s at %d bytes: %v, want %v", kind, c.total, r.Err, c.want)
			}
		}
	}
	// a short name under the deep directory is fine: the bound is the path, not the depth
	tf.ok(Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: dir, Name: "ok", Mode: 0o755, Now: 1}})
}

// SetVolume accepts exactly the policies the codec accepts: k ≥ 1 and k+m ≤ 256.
//
// Invariant: I-117
func TestFSM_SetVolume_ShardCountBoundaries(t *testing.T) {
	for _, c := range []struct {
		k, m uint8
		want error
	}{
		{1, 0, nil},
		{0, 0, ErrPolicy},
		{0, 1, ErrPolicy},
		{255, 0, nil},
		{255, 1, nil},
		{255, 2, ErrPolicy},
		{2, 254, nil},
		{3, 254, ErrPolicy},
		{1, 255, nil},
	} {
		tf := newTFSM(t)
		r := tf.apply(Command{Op: OpSetVolume, SetVolume: &SetVolume{K: c.k, M: c.m, Now: 2}})
		if !errors.Is(r.Err, c.want) {
			t.Fatalf("%d+%d: %v, want %v", c.k, c.m, r.Err, c.want)
		}
		if v := tf.f.Volume(); c.want == nil && (v.K != c.k || v.M != c.m) {
			t.Fatalf("%d+%d: volume holds %d+%d", c.k, c.m, v.K, v.M)
		}
	}
}
