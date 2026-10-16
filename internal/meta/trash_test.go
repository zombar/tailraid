package meta

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/zombar/tailraid/internal/testseed"
)

// commitOne gives file ino one chunk h (registered when fresh).
func (tf *tfsm) commitOne(ino Ino, h Hash, fresh bool) {
	tf.t.Helper()
	c := &Commit{Ino: ino, NewSize: testCS, Writes: []ChunkWrite{{0, h}}, Now: 1}
	if fresh {
		c.NewChunks = []ChunkDesc{desc(h)}
	}
	tf.ok(Command{Op: OpCommit, Commit: c})
}

func (tf *tfsm) flags(dir Ino, flags uint8) {
	tf.t.Helper()
	tf.ok(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: dir, Mask: AttrFlags, Flags: flags, Now: 1}})
}

// A delete is a move out of the name table: the inode, its subtree and its references
// stay; the gateways' reads lose it, the trash reads find it.
//
// Invariant: I-152, I-155
// Lifecycle: L-199
func TestFSM_Trash_Table(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	sub := tf.mkdir(d, "sub")
	a := tf.create(d, "a")
	b := tf.create(sub, "b")
	x := sha("x")
	tf.commitOne(a, x, true)
	tf.commitOne(b, x, false)
	before := tf.f.Stats()

	tr := func(parent Ino, name string, recursive bool, now int64) Command {
		return Command{Op: OpTrash, Trash: &Trash{Parent: parent, Name: name, Recursive: recursive, Now: now}}
	}
	// errors leave the state alone
	tf.fails(tr(d, "nope", false, 5), ErrNotExist)
	tf.fails(tr(999, "a", false, 5), ErrNotExist)
	tf.fails(tr(a, "x", false, 5), ErrNotDir)
	tf.fails(tr(d, "", false, 5), ErrBadCommand)
	tf.fails(tr(RootIno, "d", false, 5), ErrNotEmpty) // a POSIX rmdir is not recursive

	// a file: refs untouched, gone from the live reads, found by the trash reads
	dVersion := tf.inode(d).Version
	tf.ok(tr(d, "a", false, 10))
	if c := tf.chunk(x); c.Refs != 2 || c.TombstonedAt != 0 {
		t.Fatalf("refs after trash: %+v", c)
	}
	if _, ok := tf.f.GetInode(a); ok {
		t.Fatal("GetInode finds a deleted root")
	}
	if _, ok := tf.f.Lookup(d, "a"); ok {
		t.Fatal("Lookup finds a deleted root")
	}
	if _, err := tf.f.Resolve([]string{"d", "a"}); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Resolve: %v", err)
	}
	if ents, _ := tf.f.ReadDir(d); len(ents) != 1 || ents[0].Name != "sub" {
		t.Fatalf("ReadDir lists a deleted root: %+v", ents)
	}
	in, ok := tf.f.GetInodeAny(a)
	if !ok || in.DeletedAt != 10 || in.Parent != d || in.Name != "a" || in.Ctime != 10 {
		t.Fatalf("GetInodeAny: %+v %v", in, ok)
	}
	if roots := tf.f.TrashOf(d); len(roots) != 1 || roots[0].Ino != a {
		t.Fatalf("TrashOf: %+v", roots)
	}
	if pd := tf.inode(d); pd.Version != dVersion+1 || pd.Mtime != 10 {
		t.Fatalf("parent after trash: %+v", pd)
	}
	st := tf.f.Stats()
	if st.TrashInodes != 1 || st.TrashBytes != testCS || st.LogicalBytes != before.LogicalBytes-testCS {
		t.Fatalf("stats after a file: %+v", st)
	}
	// the name is free again; the second delete is a second root
	a2 := tf.create(d, "a")
	tf.ok(tr(d, "a", false, 11))
	if roots := tf.f.TrashOf(d); len(roots) != 2 || roots[0].Ino != a2 || roots[1].Ino != a { // newest first
		t.Fatalf("two roots: %+v", roots)
	}
	// a directory with children needs Recursive; as one unit, with its subtree intact
	tf.fails(tr(RootIno, "d", false, 12), ErrNotEmpty)
	rootN := tf.inode(RootIno).Nlink
	tf.ok(tr(RootIno, "d", true, 12))
	if tf.inode(RootIno).Nlink != rootN-1 {
		t.Fatal("parent nlink after a directory delete")
	}
	if _, ok := tf.f.GetInode(b); ok {
		t.Fatal("GetInode finds a file under a deleted root")
	}
	if in, err := tf.f.ResolveFrom(d, []string{"sub", "b"}); err != nil || in.Ino != b {
		t.Fatalf("ResolveFrom: %+v %v", in, err)
	}
	if _, err := tf.f.ResolveFrom(sub, nil); !errors.Is(err, ErrNotExist) {
		t.Fatal("ResolveFrom accepts a non-root")
	}
	// the older "a" folded back into d (its name was free again); the newer stays a root
	if roots := tf.f.Trash(); len(roots) != 2 || roots[0].Ino != d || roots[1].Ino != a2 {
		t.Fatalf("Trash(): %+v", roots)
	}
	if in, err := tf.f.ResolveFrom(d, []string{"a"}); err != nil || in.Ino != a || in.DeletedAt != 0 {
		t.Fatalf("folded a: %+v %v", in, err)
	}
	// nothing can be deleted inside the trash
	tf.fails(tr(sub, "b", false, 13), ErrInTrash)
	if c := tf.chunk(x); c.Refs != 2 {
		t.Fatalf("refs after a directory delete: %+v", c)
	}
	st = tf.f.Stats()
	if st.TrashInodes != 5 || st.TrashBytes != 2*testCS || st.LogicalBytes != 0 {
		t.Fatalf("stats after the directory: %+v", st)
	}
	if tf.f.TrashDue(1) || !tf.f.TrashDue(12) || !tf.f.TrashDue(13) {
		t.Fatalf("TrashDue %v %v %v", tf.f.TrashDue(1), tf.f.TrashDue(12), tf.f.TrashDue(13))
	}
	// an ephemeral name is unlinked, never trashed (L-208)
	tmp := tf.create(RootIno, ".x.tailraid-upload")
	tf.commitOne(tmp, sha("tmp"), true)
	tf.ok(tr(RootIno, ".x.tailraid-upload", false, 14))
	if _, ok := tf.f.GetInodeAny(tmp); ok {
		t.Fatal("an upload temp landed in the trash")
	}
	if c := tf.chunk(sha("tmp")); c.Refs != 0 || c.TombstonedAt != 14 {
		t.Fatalf("temp's chunk: %+v", c)
	}
	res := tf.mkdir(RootIno, ReservedDir)
	th := tf.create(res, "7-1-2.jpg")
	tf.commitOne(th, sha("th"), true)
	tf.ok(tr(res, "7-1-2.jpg", false, 15))
	if _, ok := tf.f.GetInodeAny(th); ok {
		t.Fatal("a thumbnail landed in the trash")
	}
}

// rm -rf from a mount deletes entry by entry; when the folder itself goes, the roots
// deleted from inside it fold back in, so the tree is one entry that restores at once.
//
// Invariant: I-152
func TestFSM_Trash_FoldsEarlierRoots(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	sub := tf.mkdir(d, "sub")
	a := tf.create(d, "a")
	b := tf.create(sub, "b")
	tf.trash(sub, "b", false)
	subV := tf.inode(sub).Version
	// an empty dir with a root under it: trashed with it (fold); its name table changed,
	// so its times and version move (an NFS client's negative lookup of b expires)
	tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: d, Name: "sub", Now: 7}})
	if in, _ := tf.f.GetInodeAny(sub); in.Version <= subV || in.Mtime != 7 || in.Ctime != 7 {
		t.Fatalf("folded-into dir not touched: version %d (was %d), mtime %d, ctime %d", in.Version, subV, in.Mtime, in.Ctime)
	}
	tf.trash(d, "a", false)
	// a name reused after its delete: the older root stays separate
	a2 := tf.create(d, "a")
	tf.trash(RootIno, "d", true)
	roots := tf.f.Trash()
	if len(roots) != 2 || roots[0].Ino != d || roots[1].Ino != a {
		t.Fatalf("roots: %+v", roots)
	}
	if in, err := tf.f.ResolveFrom(d, []string{"a"}); err != nil || in.Ino != a2 {
		t.Fatalf("the live a: %+v %v", in, err)
	}
	if in, err := tf.f.ResolveFrom(d, []string{"sub", "b"}); err != nil || in.Ino != b || in.DeletedAt != 0 {
		t.Fatalf("folded b: %+v %v", in, err)
	}
	if in, _ := tf.f.GetInodeAny(sub); in.DeletedAt != 0 || tf.inode(RootIno).Nlink != 2 {
		t.Fatalf("folded sub: %+v, root %+v", in, tf.inode(RootIno))
	}
	if din, _ := tf.f.GetInodeAny(d); din.Nlink != 3 {
		t.Fatalf("d's nlink after the fold: %d", din.Nlink)
	}
	// one restore brings the tree back, the separate root stays
	tf.ok(Command{Op: OpRestore, Restore: &Restore{Ino: d, Now: 20}})
	if in, err := tf.f.Resolve([]string{"d", "sub", "b"}); err != nil || in.Ino != b {
		t.Fatalf("after restore: %v", err)
	}
	if roots := tf.f.TrashOf(d); len(roots) != 1 || roots[0].Ino != a {
		t.Fatalf("after restore: %+v", roots)
	}
	if st := tf.f.Stats(); st.TrashInodes != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

// Invariant: I-152
// Lifecycle: L-200
func TestFSM_Restore_Table(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	e := tf.mkdir(RootIno, "e")
	a := tf.create(d, "a")
	gen := tf.inode(a).Gen
	tf.commitOne(a, sha("x"), true)
	tf.trash(d, "a", false)
	rs := func(ino, parent Ino, name string) Command {
		return Command{Op: OpRestore, Restore: &Restore{Ino: ino, NewParent: parent, NewName: name, Now: 30}}
	}
	tf.fails(rs(999, 0, ""), ErrNotExist)
	tf.fails(rs(d, 0, ""), ErrNotExist) // not a root
	tf.fails(rs(a, 999, ""), ErrNotExist)
	tf.fails(rs(a, 0, "a/b"), ErrBadCommand)
	tf.fails(rs(a, 0, string(bytes.Repeat([]byte("n"), MaxNameLen+1))), ErrNameTooLong)
	// taken name: ErrExist; another name or another live folder works
	a2 := tf.create(d, "a")
	tf.fails(rs(a, 0, ""), ErrExist)
	tf.ok(rs(a, 0, "a (restored)"))
	in := tf.inode(a)
	if in.Gen != gen || in.Parent != d || in.Name != "a (restored)" || in.DeletedAt != 0 || in.Ctime != 30 || in.Size != testCS {
		t.Fatalf("restored: %+v", in)
	}
	if len(tf.f.Trash()) != 0 || tf.f.Stats().TrashInodes != 0 || tf.f.Stats().LogicalBytes != testCS {
		t.Fatalf("after restore: %d roots, %+v", len(tf.f.Trash()), tf.f.Stats())
	}
	tf.trash(d, "a (restored)", false)
	tf.ok(rs(a, e, ""))
	if in := tf.inode(a); in.Parent != e || in.Name != "a (restored)" {
		t.Fatalf("restored elsewhere: %+v", in)
	}
	_ = a2
	// a parent in the trash refuses; a nested root restores once its parent is back
	sub := tf.mkdir(d, "sub")
	f := tf.create(sub, "f")
	tf.trash(sub, "f", false)
	tf.create(sub, "f") // the name is taken, so trashing sub keeps f a separate root
	tf.trash(d, "sub", true)
	tf.fails(rs(f, 0, ""), ErrInTrash)
	tf.fails(rs(f, sub, "g"), ErrInTrash)
	rootN := tf.inode(d).Nlink
	tf.ok(rs(sub, 0, ""))
	if tf.inode(d).Nlink != rootN+1 {
		t.Fatal("nlink after restoring a directory")
	}
	tf.fails(rs(f, 0, ""), ErrExist)
	tf.ok(rs(f, 0, "f2"))
	if in, err := tf.f.Resolve([]string{"d", "sub", "f2"}); err != nil || in.Ino != f {
		t.Fatalf("nested root restored: %v", err)
	}
}

// Invariant: I-153
// Lifecycle: L-201, L-202
func TestFSM_Purge_Table(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	a := tf.create(d, "a")
	b := tf.create(d, "b")
	shared, sole := sha("shared"), sha("sole")
	tf.commitOne(a, shared, true)
	tf.commitOne(b, shared, false)
	c := tf.create(d, "c")
	tf.commitOne(c, sole, true)
	tf.trash(d, "a", false)
	tf.trash(d, "c", false)
	pg := func(ino Ino, before int64, limit uint32) Command {
		return Command{Op: OpPurge, Purge: &Purge{Ino: ino, Before: before, Limit: limit, Now: 40}}
	}
	tf.fails(pg(999, 0, 0), ErrNotExist)
	tf.fails(pg(b, 0, 0), ErrNotExist) // live
	// one root: the shared chunk keeps its other reference (L-202)
	tf.ok(pg(a, 0, 0))
	if _, ok := tf.f.GetInodeAny(a); ok {
		t.Fatal("purged inode remains")
	}
	if ch := tf.chunk(shared); ch.Refs != 1 || ch.TombstonedAt != 0 {
		t.Fatalf("shared chunk: %+v", ch)
	}
	// the sole reference: tombstoned at the FSM clock (L-201)
	tf.ok(pg(c, 0, 0))
	if ch := tf.chunk(sole); ch.Refs != 0 || ch.TombstonedAt != 40 {
		t.Fatalf("sole chunk: %+v", ch)
	}
	if _, tomb := tf.f.state().Tombs[sole]; !tomb {
		t.Fatal("not in Tombs")
	}
	if st := tf.f.Stats(); st.TrashInodes != 0 || st.TrashBytes != 0 || len(tf.f.Trash()) != 0 {
		t.Fatalf("after purges: %+v", st)
	}
	// nothing due: a no-op that changes nothing
	tf.ok(pg(0, 1000, 0))
	// a user's purge obeys protection; the retention purge does not
	p := tf.mkdir(RootIno, "p")
	q := tf.create(p, "q")
	tf.flags(p, FlagProtected)
	tf.fails(Command{Op: OpTrash, Trash: &Trash{Parent: p, Name: "q", Now: 41}}, ErrProtected)
	tf.flags(p, 0)
	tf.trash(p, "q", false)
	tf.flags(p, FlagProtected)
	tf.fails(pg(q, 0, 0), ErrProtected)
	tf.ok(pg(0, 1000, 0))
	if _, ok := tf.f.GetInodeAny(q); ok {
		t.Fatal("the retention purge skipped a root under a protected folder")
	}
}

// Purge takes the roots oldest first, deletes post-order, and at most Limit inodes per
// entry: a cut leaves a valid tree and the root still due.
//
// Invariant: I-153
// Lifecycle: L-207
func TestFSM_Purge_Boundary(t *testing.T) {
	const n = 5 // d, d/sub, d/sub/f, d/g and the nested root d/sub/h
	build := func(t *testing.T) (*tfsm, Ino) {
		tf := newTFSM(t)
		d := tf.mkdir(RootIno, "d")
		sub := tf.mkdir(d, "sub")
		f := tf.create(sub, "f")
		tf.commitOne(f, sha("f"), true)
		h := tf.create(sub, "h")
		tf.commitOne(h, sha("h"), true)
		tf.trash(sub, "h", false)
		tf.create(sub, "h") // keeps the first h a separate, nested root
		tf.create(d, "g")
		tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: RootIno, Name: "d", Recursive: true, Now: 100}})
		return tf, d
	}
	for _, tc := range []struct {
		limit uint32
		want  int // inodes left in the trash
	}{{0, 0}, {1, n}, {n, 1}, {n + 1, 0}, {n + 2, 0}} {
		tf, d := build(t)
		total := tf.f.Stats().TrashInodes // 6: the five under d plus the live h that came along
		tf.ok(Command{Op: OpPurge, Purge: &Purge{Before: 101, Limit: tc.limit, Now: 200}})
		left := tf.f.Stats().TrashInodes
		wantLeft := tc.want
		if tc.limit != 0 && int(tc.limit) < total {
			wantLeft = total - int(tc.limit)
		} else if tc.limit >= uint32(total) || tc.limit == 0 {
			wantLeft = 0
		}
		if left != wantLeft {
			t.Fatalf("limit %d: %d inodes left, want %d", tc.limit, left, wantLeft)
		}
		if left > 0 {
			if _, ok := tf.f.GetInodeAny(d); !ok {
				t.Fatalf("limit %d: the root went before its children", tc.limit)
			}
			if !tf.f.TrashDue(101) {
				t.Fatalf("limit %d: the cut root is no longer due", tc.limit)
			}
			tf.ok(Command{Op: OpPurge, Purge: &Purge{Before: 101, Now: 201}}) // the next entry finishes it
		}
		if st := tf.f.Stats(); st.TrashInodes != 0 || len(tf.f.Trash()) != 0 {
			t.Fatalf("limit %d: %+v", tc.limit, st)
		}
		for _, h := range []Hash{sha("f"), sha("h")} {
			if ch := tf.chunk(h); ch.Refs != 0 {
				t.Fatalf("limit %d: chunk %x refs %d", tc.limit, h[:2], ch.Refs)
			}
		}
	}
	// Before is strict and on the FSM clock: a root deleted at 100 is not due at 100 (L-207)
	tf, _ := build(t)
	tf.ok(Command{Op: OpPurge, Purge: &Purge{Before: 100, Now: 200}})
	if tf.f.Stats().TrashInodes == 0 {
		t.Fatal("a root deleted at Before was purged")
	}
	// oldest first across roots
	tf = newTFSM(t)
	for i, now := range []int64{300, 100, 200} {
		name := string(rune('a' + i))
		tf.create(RootIno, name)
		tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: RootIno, Name: name, Now: now}})
	}
	tf.ok(Command{Op: OpPurge, Purge: &Purge{Before: 400, Limit: 2, Now: 500}})
	if roots := tf.f.Trash(); len(roots) != 1 || roots[0].DeletedAt != 300 {
		t.Fatalf("the youngest root should remain: %+v", roots)
	}
}

// Invariant: I-153
func TestFSM_Purge_CascadesNestedRoots(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	sub := tf.mkdir(d, "sub")
	f := tf.create(sub, "f")
	tf.commitOne(f, sha("f"), true)
	tf.trash(sub, "f", false)
	tf.create(sub, "f") // the first f stays a nested root when d goes
	tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: RootIno, Name: "d", Recursive: true, Now: 50}})
	if roots := tf.f.Trash(); len(roots) != 2 {
		t.Fatalf("roots: %+v", roots)
	}
	tf.ok(Command{Op: OpPurge, Purge: &Purge{Ino: d, Now: 60}})
	if len(tf.f.Trash()) != 0 || len(tf.f.state().Trash) != 0 {
		t.Fatalf("nested root survived its directory: %+v", tf.f.Trash())
	}
	if _, ok := tf.f.GetInodeAny(f); ok {
		t.Fatal("nested root's inode remains")
	}
	if ch := tf.chunk(sha("f")); ch.Refs != 0 {
		t.Fatalf("nested root's chunk: %+v", ch)
	}
}

// Invariant: I-154
// Lifecycle: L-206
func TestFSM_Protected_Table(t *testing.T) {
	tf := newTFSM(t)
	p := tf.mkdir(RootIno, "p")
	sub := tf.mkdir(p, "sub")
	f := tf.create(sub, "f")
	tf.create(p, "g")
	e := tf.mkdir(RootIno, "e")
	tf.create(e, "out")
	fl := tf.create(RootIno, "file")
	// flags are a directory's, with known bits only
	tf.fails(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: fl, Mask: AttrFlags, Flags: FlagProtected, Now: 1}}, ErrNotDir)
	tf.fails(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: p, Mask: AttrFlags, Flags: 1 << 5, Now: 1}}, ErrBadCommand)
	tf.flags(p, FlagProtected|FlagNSFW)
	if in := tf.inode(p); in.Flags != FlagProtected|FlagNSFW {
		t.Fatalf("flags: %+v", in)
	}
	rn := func(op Ino, on string, np Ino, nn string) Command {
		return Command{Op: OpRename, Rename: &Rename{OldParent: op, OldName: on, NewParent: np, NewName: nn, Now: 2}}
	}
	refused := []Command{
		{Op: OpUnlink, Unlink: &Unlink{Parent: sub, Name: "f", Now: 2}},
		{Op: OpUnlink, Unlink: &Unlink{Parent: p, Name: "g", Now: 2}},
		{Op: OpRmdir, Rmdir: &Rmdir{Parent: RootIno, Name: "p", Now: 2}},
		{Op: OpTrash, Trash: &Trash{Parent: sub, Name: "f", Now: 2}},
		{Op: OpTrash, Trash: &Trash{Parent: RootIno, Name: "p", Recursive: true, Now: 2}},
		rn(RootIno, "p", RootIno, "renamed"), // the protected folder itself
		rn(RootIno, "p", e, "p"),
		rn(sub, "f", e, "f"),       // out of the subtree
		rn(p, "sub", RootIno, "s"), // a subtree out of the subtree
		rn(e, "out", p, "g"),       // replacing an entry under protection
	}
	for i, c := range refused {
		tf.fails(c, ErrProtected)
		_ = i
	}
	// allowed: within the subtree, into it, new content, and the nsfw bit alone
	tf.ok(rn(sub, "f", p, "f"))
	tf.ok(rn(p, "f", sub, "f"))
	tf.ok(rn(e, "out", p, "in"))
	tf.create(sub, "new")
	tf.commitOne(f, sha("f"), true)
	// an empty subdir of a protected folder cannot go either; a sibling protected folder
	// does not guard the root
	tf.mkdir(p, "empty")
	tf.fails(Command{Op: OpRmdir, Rmdir: &Rmdir{Parent: p, Name: "empty", Now: 3}}, ErrProtected)
	tf.ok(Command{Op: OpRmdir, Rmdir: &Rmdir{Parent: RootIno, Name: "e", Now: 3}})
	// nsfw alone protects nothing
	tf.flags(p, FlagNSFW)
	tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: RootIno, Name: "p", Recursive: true, Now: 4}})
	// flags cannot change inside the trash
	tf.fails(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: sub, Mask: AttrFlags, Flags: FlagProtected, Now: 5}}, ErrInTrash)
}

// Invariant: I-154
func TestFSM_SetAttr_Flags(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	tf.ok(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: d, Mask: AttrFlags | AttrMode, Flags: FlagNSFW, Mode: 0o700, Now: 7}})
	if in := tf.inode(d); in.Flags != FlagNSFW || in.Mode != 0o700 || in.Ctime != 7 {
		t.Fatalf("%+v", in)
	}
	// a SetAttr without AttrFlags leaves the flags alone
	tf.ok(Command{Op: OpSetAttr, SetAttr: &SetAttr{Ino: d, Mask: AttrMode, Mode: 0o755, Now: 8}})
	if in := tf.inode(d); in.Flags != FlagNSFW {
		t.Fatalf("%+v", in)
	}
}

// The applier's running totals equal a recount after random trash, restore and purge
// sequences (the seed is printed on failure).
//
// Invariant: I-153, I-156
func TestFSM_TrashStats_MatchComputed(t *testing.T) {
	rng := testseed.Seed(t)
	f := NewFSM()
	gen := &cmdGen{rng: rng, f: f, now: 1000}
	tf := &tfsm{t: t, f: f}
	tf.ok(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})
	sawTrash := false
	for range 3000 {
		tf.apply(gen.next()) // apply recounts the stats after every command
		if len(f.state().Trash) > 0 {
			sawTrash = true
		}
	}
	if !sawTrash {
		t.Fatal("generator never filled the trash")
	}
}

// Invariant: I-155
func TestStore_TrashReadAPI(t *testing.T) {
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	a := tf.create(d, "a")
	b := tf.create(d, "b")
	tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: d, Name: "a", Now: 10}})
	tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: d, Name: "b", Now: 20}})
	tf.ok(Command{Op: OpTrash, Trash: &Trash{Parent: RootIno, Name: "d", Recursive: true, Now: 30}}) // folds a and b
	if roots := tf.f.Trash(); len(roots) != 1 || roots[0].Ino != d || roots[0].Children != nil {
		t.Fatalf("Trash(): %+v", roots)
	}
	if p, err := tf.f.PathOf(a); err != nil || len(p) != 2 || p[0] != "d" || p[1] != "a" {
		t.Fatalf("PathOf inside the trash: %v %v", p, err)
	}
	if _, ok := tf.f.GetInode(d); ok {
		t.Fatal("GetInode finds a deleted root")
	}
	if _, ok := tf.f.GetInode(b); ok {
		t.Fatal("GetInode finds a folded entry")
	}
	if ents, err := tf.f.ReadDir(d); err != nil || len(ents) != 2 {
		t.Fatalf("ReadDir of a deleted root (the browser's listing): %v %v", ents, err)
	}
	if in, err := tf.f.ResolveFrom(d, []string{"b"}); err != nil || in.Ino != b {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if _, err := tf.f.ResolveFrom(d, []string{"zz"}); !errors.Is(err, ErrNotExist) {
		t.Fatalf("ResolveFrom missing: %v", err)
	}
	if _, err := tf.f.ResolveFrom(d, []string{"b", "x"}); !errors.Is(err, ErrNotDir) {
		t.Fatalf("ResolveFrom through a file: %v", err)
	}
	if tf.f.TrashOf(RootIno)[0].Ino != d || len(tf.f.TrashOf(d)) != 0 {
		t.Fatal("TrashOf")
	}
	tf.ok(Command{Op: OpRestore, Restore: &Restore{Ino: d, Now: 40}})
	if in, ok := tf.f.GetInode(b); !ok || in.Parent != d {
		t.Fatal("GetInode after restore")
	}
}

// A version-3 snapshot reads with zero flags and an empty trash; a version-4 one round
// trips a state with deleted roots and flags, the index rebuilt.
//
// Invariant: I-156
func TestCodec_ReadsVersion3Snapshot(t *testing.T) {
	s := sampleState()
	var buf bytes.Buffer
	if err := encodeStateVersion(&buf, s, stateVersionV3); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want := withoutRewrites(s.Clone())
	for _, c := range want.Chunks {
		c.Sums = nil // version 5 carries the sums (I-201); older versions never did
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v3 round trip\n got %+v\nwant %+v", got, want)
	}
	// a state with trash and flags survives version 4 and rebuilds its index
	tf := newTFSM(t)
	d := tf.mkdir(RootIno, "d")
	tf.flags(d, FlagProtected|FlagNSFW)
	e := tf.mkdir(RootIno, "e")
	x := tf.create(e, "x")
	tf.commitOne(x, sha("x"), true)
	tf.trash(e, "x", false)
	tf.trash(RootIno, "e", true)
	st := tf.f.state()
	buf.Reset()
	if err := EncodeState(&buf, st); err != nil {
		t.Fatal(err)
	}
	got, err = DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, st) {
		t.Fatalf("v4 round trip\n got %+v\nwant %+v", got, st)
	}
	if got.Inodes[d].Flags != FlagProtected|FlagNSFW || got.Inodes[e].DeletedAt == 0 || len(got.Trash[RootIno]) != 1 {
		t.Fatalf("decoded: %+v", got.Inodes[e])
	}
	if v := CheckStats(got, computeStats(got)); len(v) != 0 || CheckStats(got, tf.f.stats) != nil {
		t.Fatalf("stats after decode: %v", v)
	}
	// written at version 3, the same state loses the trash fields and the decoder must
	// still reject nothing: the roots read as live-looking inodes out of every name table
	buf.Reset()
	if err := encodeStateVersion(&buf, st, stateVersionV3); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeState(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("v3 encoding of a trashed state: %v", err)
	}
}
