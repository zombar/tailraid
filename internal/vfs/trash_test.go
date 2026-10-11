package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// writeFile creates name with data through the billy API and leaves it dirty.
func writeFile(t *testing.T, fs *vfs.FS, name string, data []byte) {
	t.Helper()
	fh, err := fs.Create(name)
	must(t, err)
	_, err = fh.Write(data)
	must(t, err)
	must(t, fh.Close())
}

func readTrash(t *testing.T, fs *vfs.FS, root meta.Ino, path ...string) []byte {
	t.Helper()
	fh, err := fs.OpenTrash(root, path)
	must(t, err)
	defer fh.Close()
	b, err := io.ReadAll(fh)
	must(t, err)
	return b
}

// A delete through the gateway is a Trash: the name is gone from every read the mount
// makes, the file's bytes stay readable through the trash, and a dirty file keeps its
// WAL so the flush commits into the trash entry.
//
// Invariant: I-155
// Lifecycle: L-199, L-203
func TestVFS_Remove_TrashKeepsWAL_FlushCommits(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	data := testharness.Pattern(3*int(testharness.ChunkSize)+17, 7)
	writeFile(t, fs, "/d/a", data)
	ino := f.InodeOf(fs, "/d/a")
	dir := f.InodeOf(fs, "/d")
	must(t, fs.Remove("/d/a"))
	// gone from the share
	if _, err := fs.Stat("/d/a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after remove: %v", err)
	}
	if ents, _ := fs.ReadDir("/d"); len(ents) != 0 {
		t.Fatalf("listing after remove: %v", ents)
	}
	if _, ok := f.FSM.GetInode(ino); ok {
		t.Fatal("GetInode finds the deleted file")
	}
	// the WAL is kept: the overlay still holds the dirty bytes
	if _, dirty := f.Ov.Size(ino, 0); !dirty {
		t.Fatal("the overlay dropped a trashed file's WAL")
	}
	// the flush commits into the trash entry and the trash reads every byte
	must(t, f.Flusher().FlushAll(ctx))
	in, ok := f.FSM.GetInodeAny(ino)
	if !ok || in.DeletedAt == 0 || in.Size != uint64(len(data)) || len(in.Chunks) != 4 {
		t.Fatalf("trash entry after flush: %+v %v", in, ok)
	}
	if _, dirty := f.Ov.Size(ino, in.Size); dirty {
		t.Fatal("still dirty after the flush")
	}
	if got := readTrash(t, fs, ino); !bytes.Equal(got, data) {
		t.Fatalf("trash read differs at %d", testharness.FirstDiff(got, data))
	}
	if roots := f.FSM.TrashOf(dir); len(roots) != 1 || roots[0].Ino != ino {
		t.Fatalf("TrashOf: %+v", roots)
	}
	// a name is free again at once
	writeFile(t, fs, "/d/a", []byte("new"))
	if f.InodeOf(fs, "/d/a") == ino {
		t.Fatal("the new file reused the inode")
	}
	// the trash read of a committed file through its deleted directory
	data2 := testharness.Pattern(1000, 3)
	writeFile(t, fs, "/e/sub/b", data2)
	must(t, f.Flusher().FlushAll(ctx))
	e := f.InodeOf(fs, "/e")
	must(t, fs.Remove("/e/sub/b"))
	must(t, fs.Remove("/e/sub"))
	must(t, fs.Remove("/e"))                     // rm -rf lands as one root; the pieces fold back in
	if roots := f.FSM.Trash(); len(roots) != 2 { // a and e (b and sub folded into e)
		t.Fatalf("roots: %+v", roots)
	}
	if got := readTrash(t, fs, e, "sub", "b"); !bytes.Equal(got, data2) {
		t.Fatal("trash read through a deleted directory")
	}
	if _, err := fs.OpenTrash(e, []string{"sub"}); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("OpenTrash of a directory: %v", err)
	}
	if _, err := fs.OpenTrash(f.InodeOf(fs, "/d/a"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenTrash of a live file: %v", err)
	}
}

// An ephemeral name is unlinked, never trashed: its WAL goes with it.
//
// Invariant: I-157, I-195
// Lifecycle: L-208
func TestVFS_Remove_EphemeralIsImmediate(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	for _, name := range []string{"/d/._photo.jpg", "/d/.DS_Store", "/d/.photo.jpg.tailraid-upload", "/.tailraid/thumbs/7-1-2.jpg", "/.tailraid/stats/10s/pike/2026-10-11T14-pike.jsonl.gz"} {
		writeFile(t, fs, name, []byte("ephemeral"))
		ino := f.InodeOf(fs, name)
		must(t, fs.Remove(name))
		if _, ok := f.FSM.GetInodeAny(ino); ok {
			t.Fatalf("%s landed in the trash", name)
		}
		if _, dirty := f.Ov.Size(ino, 0); dirty {
			t.Fatalf("%s: the overlay kept the WAL of an unlinked file", name)
		}
	}
	if len(f.FSM.Trash()) != 0 {
		t.Fatalf("trash: %+v", f.FSM.Trash())
	}
	// the reserved folder itself goes the same way
	for _, dir := range []string{"/.tailraid/thumbs", "/.tailraid/stats/10s/pike", "/.tailraid/stats/10s", "/.tailraid/stats", "/.tailraid"} {
		must(t, fs.Remove(dir))
	}
	if len(f.FSM.Trash()) != 0 {
		t.Fatal("the reserved folder landed in the trash")
	}
}

// Rename over an existing file sends the previous version to the trash under its old
// name with its WAL; over an ephemeral name it unlinks.
//
// Lifecycle: L-205, L-208, L-17
func TestVFS_RenameOver_TrashesReplaced(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	writeFile(t, fs, "/d/old", []byte("previous version"))
	writeFile(t, fs, "/d/new", []byte("new version"))
	old := f.InodeOf(fs, "/d/old")
	must(t, fs.Rename("/d/new", "/d/old"))
	if got := f.ReadFile(f.InodeOf(fs, "/d/old")); string(got) != "new version" {
		t.Fatalf("%q", got)
	}
	if _, dirty := f.Ov.Size(old, 0); !dirty {
		t.Fatal("the replaced file lost its WAL")
	}
	must(t, f.Flusher().FlushAll(ctx))
	if got := readTrash(t, fs, old); string(got) != "previous version" {
		t.Fatalf("the previous version in the trash: %q", got)
	}
	if roots := f.FSM.TrashOf(f.InodeOf(fs, "/d")); len(roots) != 1 || roots[0].Name != "old" {
		t.Fatalf("TrashOf: %+v", roots)
	}
	// over an ephemeral name: unlinked (L-17, L-208)
	writeFile(t, fs, "/d/.x.tailraid-upload", []byte("temp"))
	tmp := f.InodeOf(fs, "/d/.x.tailraid-upload")
	writeFile(t, fs, "/d/y", []byte("y"))
	must(t, fs.Rename("/d/y", "/d/.x.tailraid-upload"))
	if _, ok := f.FSM.GetInodeAny(tmp); ok {
		t.Fatal("a replaced ephemeral entry landed in the trash")
	}
	if _, dirty := f.Ov.Size(tmp, 0); dirty {
		t.Fatal("the overlay kept the WAL of an unlinked temp")
	}
}

// Nothing under a protected directory leaves the namespace through the gateway, and the
// error is EPERM (os.ErrPermission), what the mounts turn into "Operation not permitted".
//
// Invariant: I-154
// Lifecycle: L-206
func TestVFS_Protected_EPERM(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	writeFile(t, fs, "/p/sub/f", []byte("keep"))
	writeFile(t, fs, "/e/out", []byte("out"))
	p := f.InodeOf(fs, "/p")
	f.MustApply(meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: p, Mask: meta.AttrFlags, Flags: meta.FlagProtected, Now: 1}})
	perm := func(err error) bool { return errors.Is(err, os.ErrPermission) && errors.Is(err, syscall.EPERM) }
	for name, err := range map[string]error{
		"remove file":   fs.Remove("/p/sub/f"),
		"rename out":    fs.Rename("/p/sub/f", "/e/f"),
		"rename folder": fs.Rename("/p", "/q"),
		"replace under": fs.Rename("/e/out", "/p/sub/f"),
	} {
		if !perm(err) {
			t.Errorf("%s: %v, want EPERM", name, err)
		}
	}
	if err := fs.Remove("/p/sub"); !perm(err) { // not empty: ENOTEMPTY would be the POSIX answer, but protection comes first
		t.Errorf("remove directory: %v, want EPERM", err)
	}
	if _, err := fs.Stat("/p/sub/f"); err != nil {
		t.Fatalf("the protected tree lost something: %v", err)
	}
	// within, into and new content are fine
	must(t, fs.Rename("/p/sub/f", "/p/f"))
	must(t, fs.Rename("/e/out", "/p/in"))
	writeFile(t, fs, "/p/new", []byte("new"))
	if _, err := fs.Stat("/p/f"); err != nil {
		t.Fatal(err)
	}
}

// A dirty file deleted and then purged before its flush: the Commit meets ErrNotExist
// and the WAL is dropped, as for an unlinked file.
//
// Lifecycle: L-204
func TestVFS_PurgeBeforeFlush_DropsWAL(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	writeFile(t, fs, "/a", []byte("never committed"))
	ino := f.InodeOf(fs, "/a")
	must(t, fs.Remove("/a"))
	f.MustApply(meta.Command{Op: meta.OpPurge, Purge: &meta.Purge{Ino: ino, Now: 2}})
	must(t, f.Flusher().FlushAll(ctx))
	if _, dirty := f.Ov.Size(ino, 0); dirty {
		t.Fatal("the overlay kept the WAL of a purged file")
	}
	if st := f.FSM.Stats(); st.Chunks != 0 || st.TrashInodes != 0 {
		t.Fatalf("chunks committed for a purged file: %+v", st)
	}
}

// codecOld is a proposer whose leader refuses the codec-4 commands (docs/12 §6).
type codecOld struct{ *testharness.Fabric }

func (c codecOld) Propose(ctx context.Context, cmd meta.Command) (meta.Result, uint64, error) {
	if cmd.Op == meta.OpTrash || cmd.Op == meta.OpRestore || cmd.Op == meta.OpPurge {
		return meta.Result{Err: meta.ErrCodec}, 0, nil
	}
	return c.Fabric.Propose(ctx, cmd)
}

// While a member is too old for the trash, a delete falls back to the plain unlink.
//
// Invariant: I-156
func TestVFS_Remove_FallsBackWhileCodecOld(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := vfs.New(f.FSM, codecOld{f}, f.Ov, f.Reader(), f.Clk, 0, 0)
	writeFile(t, fs, "/d/a", []byte("old cluster"))
	ino := f.InodeOf(fs, "/d/a")
	must(t, fs.Remove("/d/a"))
	must(t, fs.Remove("/d"))
	if _, ok := f.FSM.GetInodeAny(ino); ok {
		t.Fatal("the fallback trashed the file")
	}
	if _, dirty := f.Ov.Size(ino, 0); dirty {
		t.Fatal("the fallback kept the WAL")
	}
	if len(f.FSM.Trash()) != 0 || f.FSM.Stats().Inodes != 1 {
		t.Fatalf("after the fallback: %+v", f.FSM.Stats())
	}
}
