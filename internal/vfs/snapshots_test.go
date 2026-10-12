package vfs_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// snapRig is a volume with a file of two and a half chunks, a symlink and an empty
// folder, a mirror "host/m1" holding snapshot "s1" of it, and the gateway's filesystem
// reading the mirror through src.
type snapRig struct {
	f    *testharness.Fabric
	fs   *vfs.FS
	src  *testharness.Mirrors
	m    meta.NodeIdx
	sn   meta.Snapshot
	old  []byte // /docs/a.bin as s1 holds it
	link string
}

func newSnapRig(t *testing.T) *snapRig {
	t.Helper()
	f := testharness.NewFabric(t, 3, 2, 1)
	r := &snapRig{f: f, fs: f.FS(), src: testharness.NewMirrors(), link: "../elsewhere"}
	cs := int(f.FSM.Volume().ChunkSize)
	r.old = testharness.Pattern(2*cs+cs/2, 7)
	writeFile(t, r.fs, "/docs/a.bin", r.old)
	must(t, r.fs.Symlink(r.link, "/docs/link"))
	must(t, r.fs.MkdirAll("/empty", 0o755))
	must(t, f.Flusher().FlushAll(ctx))
	r.m = f.AddMirror(r.src, "host/m1")
	r.sn = f.TakeSnapshot(r.src, r.m, "s1")
	r.fs.SetMirrors(r.src)
	return r
}

func names(fis []os.FileInfo) []string {
	out := make([]string, len(fis))
	for i, fi := range fis {
		out[i] = fi.Name()
	}
	return out
}

func readAt(t *testing.T, fs *vfs.FS, name string, n int, off int64) ([]byte, error) {
	t.Helper()
	fh, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b := make([]byte, n)
	got, err := fh.ReadAt(b, off)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return b[:got], err
}

// /.snapshots is absent from the root's listing, resolves by name and lists each mirror
// and, in it, its complete and incomplete snapshots; a syncing one is not listed; the
// tree inside is the volume as of the snapshot, read-only (docs/16 §6).
//
// Invariant: I-185
func TestVFS_SnapshotsDir_HiddenResolvesLists(t *testing.T) {
	r := newSnapRig(t)
	root, err := r.fs.ReadDir("/")
	must(t, err)
	if got := names(root); !slices.Equal(got, []string{"docs", "empty"}) {
		t.Fatalf("root lists %v", got)
	}
	// a second snapshot left syncing: in the catalogue, not on the mounts
	r.f.MustApply(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.m, Name: "s2", Now: 5}})
	for _, c := range []struct {
		dir  string
		want []string
	}{
		{"/.snapshots", []string{"host-m1"}},
		{".snapshots/host-m1", []string{"s1"}},
		{"/.snapshots/host-m1/s1", []string{"docs", "empty"}},
		{"/.snapshots/host-m1/s1/docs", []string{"a.bin", "link"}},
		{"/.snapshots/host-m1/s1/empty", []string{}},
	} {
		fis, err := r.fs.ReadDir(c.dir)
		must(t, err)
		if got := names(fis); !slices.Equal(got, c.want) {
			t.Fatalf("%s lists %v, want %v", c.dir, got, c.want)
		}
		fi, err := r.fs.Stat(c.dir)
		must(t, err)
		if !fi.IsDir() || fi.Mode().Perm()&0o222 != 0 {
			t.Fatalf("%s: %v", c.dir, fi.Mode())
		}
	}
	fi, err := r.fs.Stat("/.snapshots/host-m1/s1/docs/a.bin")
	must(t, err)
	if fi.Size() != int64(len(r.old)) || fi.IsDir() || fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("a.bin: %d %v", fi.Size(), fi.Mode())
	}
	if target, err := r.fs.Readlink("/.snapshots/host-m1/s1/docs/link"); err != nil || target != r.link {
		t.Fatalf("readlink: %q %v", target, err)
	}
	if !fi.ModTime().Equal(time.Unix(0, r.f.Inode(r.f.InodeOf(r.fs, "/docs/a.bin")).Mtime)) {
		t.Fatalf("mtime %v", fi.ModTime())
	}
	for _, missing := range []string{"/.snapshots/nope", "/.snapshots/host-m1/s2", "/.snapshots/host-m1/s1/nope", "/.snapshots/host-m1/s1/docs/a.bin/x"} {
		if _, err := r.fs.Stat(missing); !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("%s: %v", missing, err)
		}
	}
}

// A snapshot's file reads exactly the bytes it had: from the pool while its chunks are
// there, from the mirror's objects once the pool collected them, at any offset and across
// chunk boundaries (docs/16 §6).
//
// Invariant: I-185
func TestVFS_SnapshotRead_PoolThenMirror(t *testing.T) {
	r := newSnapRig(t)
	const name = "/.snapshots/host-m1/s1/docs/a.bin"
	cs := int(r.f.FSM.Volume().ChunkSize)
	// the live file changes; the snapshot does not
	writeFile(t, r.fs, "/docs/a.bin", testharness.Pattern(cs, 9))
	must(t, r.f.Flusher().FlushAll(ctx))
	check := func(stage string) {
		t.Helper()
		for _, c := range []struct{ off, n int }{
			{0, len(r.old)}, {1, 10}, {cs - 3, 6}, {2*cs + 1, cs}, {len(r.old) - 1, 4}, {len(r.old), 4},
		} {
			got, err := readAt(t, r.fs, name, c.n, int64(c.off))
			must(t, err)
			want := r.old[min(c.off, len(r.old)):min(c.off+c.n, len(r.old))]
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: [%d,+%d) differs at %d", stage, c.off, c.n, testharness.FirstDiff(got, want))
			}
		}
	}
	check("pool")
	if n := r.src.Objects(); n != 0 {
		t.Fatalf("%d objects read from the mirror while the pool held them", n)
	}
	r.f.MustApply(meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: 1 << 62}})
	for _, h := range r.f.Inode(r.f.InodeOf(r.fs, "/docs/a.bin")).Chunks {
		if _, ok := r.f.FSM.GetChunk(h); !ok {
			t.Fatal("GC took the live file's chunk")
		}
	}
	check("mirror")
	if r.src.Objects() == 0 {
		t.Fatal("no object read from the mirror after the pool dropped them")
	}
}

// Inside an offline mirror's snapshot every read is EIO, cached trees included; the
// mirror's listing of snapshot names still works (I-185).
//
// Invariant: I-185
func TestVFS_SnapshotOfflineMirror_EIO(t *testing.T) {
	r := newSnapRig(t)
	if _, err := r.fs.ReadDir("/.snapshots/host-m1/s1/docs"); err != nil { // trees now cached
		t.Fatal(err)
	}
	r.src.SetOnline(r.m, false)
	fis, err := r.fs.ReadDir("/.snapshots/host-m1")
	if err != nil || !slices.Equal(names(fis), []string{"s1"}) {
		t.Fatalf("offline mirror's listing: %v %v", names(fis), err)
	}
	if _, err := r.fs.ReadDir("/.snapshots/host-m1/s1/docs"); !errors.Is(err, syscall.EIO) {
		t.Fatalf("readdir: %v", err)
	}
	if _, err := r.fs.Stat("/.snapshots/host-m1/s1/docs/a.bin"); !errors.Is(err, syscall.EIO) {
		t.Fatalf("stat: %v", err)
	}
	if _, err := readAt(t, r.fs, "/.snapshots/host-m1/s1/docs/a.bin", 10, 5); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read: %v", err)
	}
	r.src.SetOnline(r.m, true)
	if got, err := readAt(t, r.fs, "/.snapshots/host-m1/s1/docs/a.bin", 10, 5); err != nil || !bytes.Equal(got, r.old[5:15]) {
		t.Fatalf("back online: %v", err)
	}
}

// Every mutation under /.snapshots is EROFS, and so is creating, renaming onto or
// removing the reserved name at the root; nothing reaches the FSM (I-185).
//
// Invariant: I-185
func TestVFS_SnapshotMutation_IsROFS(t *testing.T) {
	r := newSnapRig(t)
	const in = "/.snapshots/host-m1/s1/docs/a.bin"
	ops := r.f.Ops()
	for _, c := range []struct {
		name string
		do   func() error
	}{
		{"create", func() error { _, err := r.fs.Create("/.snapshots/host-m1/s1/new"); return err }},
		{"open rdwr", func() error { _, err := r.fs.OpenFile(in, os.O_RDWR, 0); return err }},
		{"open trunc", func() error { _, err := r.fs.OpenFile(in, os.O_WRONLY|os.O_TRUNC, 0); return err }},
		{"remove", func() error { return r.fs.Remove(in) }},
		{"remove a snapshot", func() error { return r.fs.Remove("/.snapshots/host-m1/s1") }},
		{"trash", func() error { return r.fs.Trash(in) }},
		{"rename within", func() error { return r.fs.Rename(in, "/.snapshots/host-m1/s1/docs/b") }},
		{"rename out", func() error { return r.fs.Rename(in, "/out") }},
		{"rename in", func() error { return r.fs.Rename("/docs/a.bin", "/.snapshots/host-m1/s1/x") }},
		{"mkdir", func() error { return r.fs.MkdirAll("/.snapshots/host-m1/s1/d", 0o755) }},
		{"symlink", func() error { return r.fs.Symlink("x", "/.snapshots/host-m1/s1/l") }},
		{"chmod", func() error { return r.fs.Chmod(in, 0o777) }},
		{"chown", func() error { return r.fs.Lchown(in, 1, 1) }},
		{"chtimes", func() error { return r.fs.Chtimes(in, time.Now(), time.Now()) }},
		{"flags", func() error { return r.fs.SetFlags("/.snapshots/host-m1/s1/docs", meta.FlagProtected) }},
		{"create the reserved name", func() error { _, err := r.fs.Create("/.snapshots"); return err }},
		{"mkdir the reserved name", func() error { return r.fs.MkdirAll("/.snapshots", 0o755) }},
		{"symlink the reserved name", func() error { return r.fs.Symlink("x", "/.snapshots") }},
		{"rename onto the reserved name", func() error { return r.fs.Rename("/empty", "/.snapshots") }},
		{"remove the reserved name", func() error { return r.fs.Remove("/.snapshots") }},
	} {
		if err := c.do(); !errors.Is(err, syscall.EROFS) {
			t.Errorf("%s: %v, want EROFS", c.name, err)
		}
	}
	fh, err := r.fs.Open(in)
	must(t, err)
	if _, err := fh.Write([]byte("x")); err == nil {
		t.Error("a write through a read-only handle succeeded")
	}
	if err := fh.Truncate(0); !errors.Is(err, syscall.EROFS) {
		t.Errorf("truncate: %v", err)
	}
	if got := r.f.Ops(); len(got) != len(ops) {
		t.Fatalf("mutations proposed %v", got[len(ops):])
	}
}

// A snapshot handle names an entry by (snapshot, ino, gen) and finds its path again; a
// gateway that never handed it out finds it by walking the trees; it is stale once the
// row is gone, the mirror offline or removed, or the pair in no tree (docs/01, I-185).
//
// Invariant: I-185
func TestVFS_SnapshotHandle_StaleTable(t *testing.T) {
	r := newSnapRig(t)
	paths := [][]string{
		{".snapshots"},
		{".snapshots", "host-m1"},
		{".snapshots", "host-m1", "s1"},
		{".snapshots", "host-m1", "s1", "docs"},
		{".snapshots", "host-m1", "s1", "docs", "a.bin"},
	}
	handles := make([]meta.SnapHandle, len(paths))
	for i, p := range paths {
		h, ok := r.fs.SnapshotHandle(p)
		if !ok {
			t.Fatalf("%v: no handle", p)
		}
		handles[i] = h
		if got, ok := r.fs.SnapshotPath(h); !ok || !slices.Equal(got, p) {
			t.Fatalf("%v → %+v → %v %v", p, h, got, ok)
		}
	}
	live := r.f.Inode(r.f.InodeOf(r.fs, "/docs/a.bin"))
	if h := handles[4]; h.Snap != r.sn.ID || h.Ino != uint64(live.Ino) || h.Gen != live.Gen {
		t.Fatalf("file handle %+v, live inode %d gen %d", h, live.Ino, live.Gen)
	}
	if _, ok := r.fs.SnapshotHandle([]string{"docs"}); ok {
		t.Fatal("a live path has a snapshot handle")
	}
	// another gateway: an empty index, filled by a walk on the miss
	other := r.f.FS()
	other.SetMirrors(r.src)
	if got, ok := other.SnapshotPath(handles[4]); !ok || !slices.Equal(got, paths[4]) {
		t.Fatalf("fresh gateway: %v %v", got, ok)
	}
	stale := func(stage string, h meta.SnapHandle) {
		t.Helper()
		if got, ok := r.fs.SnapshotPath(h); ok {
			t.Fatalf("%s: %+v resolved to %v", stage, h, got)
		}
	}
	stale("unknown pair", meta.SnapHandle{Snap: r.sn.ID, Ino: 999, Gen: 1})
	stale("unknown snapshot", meta.SnapHandle{Snap: 77})
	stale("unknown mirror", meta.SnapHandle{Ino: 99, Gen: meta.SnapGenMirror})
	r.src.SetOnline(r.m, false)
	stale("offline", handles[4])
	stale("offline root", handles[2])
	r.src.SetOnline(r.m, true)
	r.f.MustApply(meta.Command{Op: meta.OpSnapshotDelete, SnapshotDelete: &meta.SnapshotDelete{ID: r.sn.ID, Now: 9}})
	stale("row deleting", handles[4])
	if _, err := r.fs.Stat("/.snapshots/host-m1/s1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleting snapshot still resolves: %v", err)
	}
}
