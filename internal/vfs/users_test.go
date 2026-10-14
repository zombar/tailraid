package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

func mkdirAs(t *testing.T, fs *vfs.FS, path string) {
	t.Helper()
	if err := fs.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeAs(t *testing.T, fs *vfs.FS, path string, data []byte) error {
	t.Helper()
	fh, err := fs.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		return err
	}
	return fh.Close()
}

// A view stamps its actor on every proposal, and the actor rides the WAL into the
// flush's Commit: lee rewrites a file under a folder they hold Write on, the grant is
// revoked before the flush, and the FSM refuses the rewrite's Commit for lee; kim's
// flush of their own rewrite lands. The node's own writer is refused outside /.tailraid
// once users exist, and nothing is stamped before the first user.
// Invariant: I-353
func TestVFS_As_StampsActor(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	base := f.FS()
	fl := f.Flusher()
	base.SetFlusher(fl)
	mkdirAs(t, base, "/d") // before users: the plain command, nothing stamped
	if ops := f.Ops(); ops[len(ops)-1] != meta.OpMkdir {
		t.Fatalf("ops %v", ops)
	}
	kim, lee := f.Users()
	kimFS, leeFS := base.As(kim), base.As(lee)
	if err := base.MkdirAll("/system", 0o755); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("the node's own writer outside /.tailraid: %v", err)
	}
	mkdirAs(t, base, "/"+meta.ReservedDir+"/thumbs")
	// the node's own write under /.tailraid flushes as System: a stats or audit segment
	// after the first user (the Commit of a truncating write needs Write otherwise)
	if err := writeAs(t, base, "/"+meta.ReservedDir+"/thumbs/seg", []byte("segment")); err != nil {
		t.Fatal(err)
	}
	if err := fl.FlushAll(context.Background()); err != nil {
		t.Fatalf("the node's own segment did not flush: %v", err)
	}
	if in, err := f.FSM.Resolve([]string{meta.ReservedDir, "thumbs", "seg"}); err != nil || in.Size != 7 {
		t.Fatalf("the node's own segment: %v size %d", err, in.Size)
	}
	mkdirAs(t, kimFS, "/d/w")
	f.Grant(kim, "/d/w", lee, meta.RoleWrite)
	if err := writeAs(t, leeFS, "/d/w/f", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := fl.FlushAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a rewrite by lee, then the grant goes before the flush: the Commit is lee's and is refused
	fh, err := leeFS.OpenFile("/d/w/f", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("rewritten by lee")); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	in, _ := f.FSM.Resolve([]string{"d", "w", "f"})
	f.Grant(kim, "/d/w", lee, meta.RoleRead)
	if err := fl.FlushInode(context.Background(), in.Ino); err == nil || !errors.Is(err, meta.ErrAccess) {
		t.Fatalf("lee's rewrite after the revoke: %v, want ErrAccess", err)
	}
	if got, _ := f.FSM.GetInode(in.Ino); got.Size != 5 {
		t.Fatalf("the refused rewrite changed the file: size %d", got.Size)
	}
	// kim's rewrite of the same file lands, through the same overlay (the last writer wins)
	fh, err = kimFS.OpenFile("/d/w/f", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("kim")); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	if err := fl.FlushInode(context.Background(), in.Ino); err != nil {
		t.Fatalf("kim's rewrite: %v", err)
	}
	if got, _ := f.FSM.GetInode(in.Ino); got.Size != 3 {
		t.Fatalf("kim's rewrite did not land: size %d", got.Size)
	}
	if kimFS.Actor() != kim || base.Actor() != 0 || !base.System() || kimFS.System() {
		t.Fatal("view identities")
	}
}

// The read side of a view: open for read needs Read, for write Write; stat of an
// ancestor is allowed on the way to a role; nobody and a role-less path are refused;
// the mode bits follow the role.
// Invariant: I-353, I-351
func TestVFS_As_ReadCheck(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	base := f.FS()
	kim, lee := f.Users()
	kimFS, leeFS, nobody := base.As(kim), base.As(lee), base.As(0)
	for _, d := range []string{"/r", "/w", "/other"} {
		mkdirAs(t, kimFS, d)
		if err := writeAs(t, kimFS, d+"/f", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	f.Grant(kim, "/r", lee, meta.RoleRead)
	f.Grant(kim, "/w", lee, meta.RoleWrite)
	for _, tc := range []struct {
		name string
		do   func() error
		want error
	}{
		{"read under read", func() error { _, err := leeFS.Open("/r/f"); return err }, nil},
		{"write under read", func() error { _, err := leeFS.OpenFile("/r/f", os.O_WRONLY, 0); return err }, syscall.EACCES},
		{"write under write", func() error { _, err := leeFS.OpenFile("/w/f", os.O_WRONLY, 0); return err }, nil},
		{"create under read", func() error { return writeAs(t, leeFS, "/r/g", []byte("y")) }, syscall.EACCES},
		{"read elsewhere", func() error { _, err := leeFS.Open("/other/f"); return err }, syscall.EACCES},
		{"stat elsewhere", func() error { _, err := leeFS.Stat("/other"); return err }, syscall.EACCES},
		{"stat the root", func() error { _, err := leeFS.Stat("/"); return err }, nil},
		{"nobody stats the root", func() error { _, err := nobody.Stat("/"); return err }, nil},
		{"nobody reads", func() error { _, err := nobody.Open("/r/f"); return err }, syscall.EACCES},
		{"nobody creates", func() error { return writeAs(t, nobody, "/w/n", []byte("n")) }, syscall.EACCES},
		{"the admin anywhere", func() error { _, err := kimFS.OpenFile("/other/f", os.O_WRONLY, 0); return err }, nil},
	} {
		err := tc.do()
		if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	ents, err := nobody.ReadDir("/")
	if err != nil || len(ents) != 0 {
		t.Fatalf("nobody's root listing: %v %d entries", err, len(ents))
	}
	for path, want := range map[string]os.FileMode{"/r": 0o500, "/w": 0o700, "/r/f": 0o400, "/w/f": 0o600} {
		fi, err := leeFS.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %04o, want %04o", path, got, want)
		}
	}
	if fi, _ := kimFS.Stat("/r/f"); fi.Mode().Perm() != 0o600 {
		t.Errorf("the admin's mode: %04o", fi.Mode().Perm())
	}
	if fi, _ := base.Stat("/r/f"); fi.Mode().Perm() != 0o644 {
		t.Errorf("the node's own view keeps the stored mode: %04o", fi.Mode().Perm())
	}
}

// A listing of a folder the user holds no role on shows exactly the children on the way
// to something they hold one on; the folder they hold the role on lists in full; a
// folder with nothing below for them is refused, except the root, which is empty.
// Invariant: I-348
func TestAccess_ListingFilter(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	base := f.FS()
	kim, lee := f.Users()
	kimFS, leeFS := base.As(kim), base.As(lee)
	for _, d := range []string{"/a/b/c", "/a/other", "/x"} {
		mkdirAs(t, kimFS, d)
	}
	for _, p := range []string{"/a/b/c/f1", "/a/b/c/f2", "/a/b/g", "/x/h"} {
		if err := writeAs(t, kimFS, p, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	f.Grant(kim, "/a/b/c", lee, meta.RoleWrite)
	names := func(ents []os.FileInfo) []string {
		out := []string{}
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}
	for path, want := range map[string][]string{"/": {"a"}, "/a": {"b"}, "/a/b": {"c"}, "/a/b/c": {"f1", "f2"}} {
		ents, err := leeFS.ReadDir(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := names(ents); len(got) != len(want) || (len(got) > 0 && (got[0] != want[0] || got[len(got)-1] != want[len(want)-1])) {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
	if _, err := leeFS.ReadDir("/x"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("a folder with nothing for lee: %v", err)
	}
	if _, err := leeFS.ReadDir("/a/other"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("a sibling folder: %v", err)
	}
	if ents, _ := kimFS.ReadDir("/"); len(ents) != 2 {
		t.Fatalf("the admin's root: %d entries", len(ents))
	}
}

// The WAL carries the writer: a reopened overlay remembers the last writer of an inode,
// a change of writer is recorded, and a compaction keeps it.
// Invariant: I-353
func TestWAL_RecordsActor(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	open := func() *vfs.Overlay {
		ov, err := vfs.OpenOverlay(dir, 1<<20, clk)
		if err != nil {
			t.Fatal(err)
		}
		return ov
	}
	ov := open()
	if err := ov.WriteAs(7, 0, 100, []byte("five"), 5); err != nil {
		t.Fatal(err)
	}
	if ov.Actor(7) != 5 || ov.Actor(8) != 0 {
		t.Fatalf("actor %d after the write", ov.Actor(7))
	}
	ov.Close()
	ov = open()
	if ov.Actor(7) != 5 {
		t.Fatalf("actor %d after a reopen", ov.Actor(7))
	}
	if err := ov.TruncateAs(7, 0, 50, 6); err != nil {
		t.Fatal(err)
	}
	ov.Close()
	ov = open()
	if ov.Actor(7) != 6 {
		t.Fatalf("actor %d after a second writer and a reopen", ov.Actor(7))
	}
	plan, ok := ov.SnapshotForFlush(7)
	if !ok || plan.Actor != 6 {
		t.Fatalf("plan %+v %v", plan, ok)
	}
	if err := ov.WriteAs(7, 0, 200, []byte("fresh"), 6); err != nil { // fresh data survives the compaction
		t.Fatal(err)
	}
	if err := ov.CommitFlushed(plan, 50); err != nil {
		t.Fatal(err)
	}
	ov.Close()
	ov = open()
	defer ov.Close()
	if ov.Actor(7) != 6 {
		t.Fatalf("actor %d after a compaction and a reopen", ov.Actor(7))
	}
}

// A snapshot entry is read with the live grants of the inode it was captured from: a
// user with no role on /docs cannot stat or read the snapshot's copy of a file under
// it, Read on the live folder opens it, and once the inode is purged from the live tree
// only an admin reads the copy. The directories above the entries are navigation.
// Invariant: I-348
func TestAccess_SnapshotPaths(t *testing.T) {
	r := newSnapRig(t)
	kim, lee := r.f.Users()
	kimFS, leeFS := r.fs.As(kim), r.fs.As(lee)
	const snap = "/.snapshots/host-m1/s1"
	for _, p := range []string{"/.snapshots", "/.snapshots/host-m1", snap} {
		if _, err := leeFS.ReadDir(p); err != nil {
			t.Fatalf("navigation %s: %v", p, err)
		}
	}
	if _, err := leeFS.Stat(snap + "/docs/a.bin"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("no role on /docs: stat %v", err)
	}
	if _, err := readAt(t, leeFS, snap+"/docs/a.bin", 16, 0); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("no role on /docs: read %v", err)
	}
	r.f.Grant(kim, "/docs", lee, meta.RoleRead)
	got, err := readAt(t, leeFS, snap+"/docs/a.bin", 16, 4096)
	if err != nil || !bytes.Equal(got, r.old[4096:4096+16]) {
		t.Fatalf("read with Read on the live folder: %v", err)
	}
	// the live file goes: the copy is the admin's alone
	ino := r.f.InodeOf(r.fs, "/docs/a.bin")
	if err := kimFS.Remove("/docs/a.bin"); err != nil {
		t.Fatal(err)
	}
	r.f.MustApply(meta.Command{Op: meta.OpPurge, System: true, Purge: &meta.Purge{Ino: ino, Now: r.f.Clk.Now().UnixNano()}})
	if _, err := leeFS.Stat(snap + "/docs/a.bin"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("after the purge, lee: %v", err)
	}
	if _, err := kimFS.Stat(snap + "/docs/a.bin"); err != nil {
		t.Fatalf("after the purge, the admin: %v", err)
	}
}
