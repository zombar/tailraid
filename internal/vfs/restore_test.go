package vfs_test

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

const snapFile = "/.snapshots/host-m1/s1/docs/a.bin"

func (r *snapRig) withFlusher() *snapRig {
	r.fs.SetFlusher(r.f.Flusher())
	return r
}

func (r *snapRig) content(t *testing.T, name string) []byte {
	t.Helper()
	fi, err := r.fs.Stat(name)
	must(t, err)
	b, err := readAt(t, r.fs, name, int(fi.Size())+1, 0)
	must(t, err)
	return b
}

func (r *snapRig) change(t *testing.T, seed byte) {
	t.Helper()
	cs := int(r.f.FSM.Volume().ChunkSize)
	writeFile(t, r.fs, "/docs/a.bin", testharness.Pattern(cs+cs/3, seed))
	must(t, r.f.Flusher().FlushAll(ctx))
}

// A revert commits by hash every chunk the pool still holds: no upload, nothing read from
// the mirror, the same inode (L-217); once GC collected them, it reads the mirror's
// objects, verifies and uploads them (L-216). Either way the file holds exactly the
// snapshot's bytes (docs/16 §6).
//
// Invariant: I-186
// Lifecycle: L-216, L-217
func TestVFS_Revert_FromPoolAndMirror(t *testing.T) {
	r := newSnapRig(t).withFlusher()
	ino := r.f.InodeOf(r.fs, "/docs/a.bin")
	r.change(t, 9)
	puts := r.f.Puts()
	if _, err := r.fs.RevertFile(ctx, snapFile, "/docs/a.bin"); err != nil {
		t.Fatal(err)
	}
	if got := r.content(t, "/docs/a.bin"); !bytes.Equal(got, r.old) {
		t.Fatalf("revert from the pool differs at %d", testharness.FirstDiff(got, r.old))
	}
	if r.f.Puts() != puts || r.src.Objects() != 0 {
		t.Fatalf("revert from the pool uploaded %d shards, read %d objects", r.f.Puts()-puts, r.src.Objects())
	}
	if r.f.InodeOf(r.fs, "/docs/a.bin") != ino {
		t.Fatal("the revert replaced the inode")
	}

	r.change(t, 11)
	r.f.MustApply(meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: 1 << 62}})
	for _, h := range r.f.Inode(ino).Chunks {
		if _, ok := r.f.FSM.GetChunk(h); !ok {
			t.Fatal("GC took a live chunk")
		}
	}
	puts = r.f.Puts()
	if _, err := r.fs.RevertFile(ctx, snapFile, "/docs/a.bin"); err != nil {
		t.Fatal(err)
	}
	if got := r.content(t, "/docs/a.bin"); !bytes.Equal(got, r.old) {
		t.Fatalf("revert from the mirror differs at %d", testharness.FirstDiff(got, r.old))
	}
	if r.f.Puts() == puts || r.src.Objects() == 0 {
		t.Fatal("the collected chunks were not read from the mirror and uploaded")
	}
}

// A revert lands the file's dirty bytes first, so nothing written before it survives
// over the snapshot's bytes afterwards (I-186).
//
// Invariant: I-186
func TestVFS_Revert_FlushesDirtyFirst(t *testing.T) {
	r := newSnapRig(t).withFlusher()
	fh, err := r.fs.OpenFile("/docs/a.bin", os.O_RDWR, 0)
	must(t, err)
	_, err = fh.(*vfs.File).WriteAt([]byte("dirty, never flushed"), 4097)
	must(t, err)
	must(t, fh.Close())
	if _, err := r.fs.RevertFile(ctx, snapFile, "/docs/a.bin"); err != nil {
		t.Fatal(err)
	}
	must(t, r.f.Flusher().FlushAll(ctx))
	if got := r.content(t, "/docs/a.bin"); !bytes.Equal(got, r.old) {
		t.Fatalf("after revert and flush: differs at %d", testharness.FirstDiff(got, r.old))
	}
}

// A copy restores a snapshot's file beside the live one under a new name; it never
// replaces an existing name and never writes into /.snapshots or /.tailraid (I-186).
//
// Invariant: I-186
func TestVFS_SaveCopy(t *testing.T) {
	r := newSnapRig(t).withFlusher()
	r.change(t, 5)
	rep, err := r.fs.RestoreSnapshot(ctx, snapFile, "/docs/a (s1).bin")
	must(t, err)
	if rep.Files != 1 || len(rep.Skipped) != 0 {
		t.Fatalf("copy report %+v", rep)
	}
	if got := r.content(t, "/docs/a (s1).bin"); !bytes.Equal(got, r.old) {
		t.Fatalf("copy differs at %d", testharness.FirstDiff(got, r.old))
	}
	if _, err := r.fs.RestoreSnapshot(ctx, snapFile, "/docs/a.bin"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("a copy over an existing name: %v", err)
	}
	for _, dst := range []string{"/.snapshots/host-m1/s1/x", "/.tailraid/x"} {
		if _, err := r.fs.RestoreSnapshot(ctx, snapFile, dst); !errors.Is(err, syscall.EROFS) {
			t.Fatalf("copy to %s: %v", dst, err)
		}
	}
	if _, err := r.fs.RestoreSnapshot(ctx, "/docs/a.bin", "/y"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a live source: %v", err)
	}
}

// A tree restore recreates the snapshot's folder entry by entry: a new target gets all of
// it; over the live tree an existing folder is descended and an existing file or link is
// skipped and reported, while a missing one comes back (I-186).
//
// Invariant: I-186
func TestVFS_RestoreTree_SkipsExisting(t *testing.T) {
	r := newSnapRig(t).withFlusher()
	r.change(t, 6)
	must(t, r.fs.Remove("/docs/link"))

	rep, err := r.fs.RestoreSnapshot(ctx, "/.snapshots/host-m1/s1", "/restored")
	must(t, err)
	if rep.Files != 1 || rep.Links != 1 || rep.Dirs != 3 || len(rep.Skipped) != 0 {
		t.Fatalf("restore to a new folder %+v", rep)
	}
	if got := r.content(t, "/restored/docs/a.bin"); !bytes.Equal(got, r.old) {
		t.Fatal("restored file differs")
	}
	if target, err := r.fs.Readlink("/restored/docs/link"); err != nil || target != r.link {
		t.Fatalf("restored link %q %v", target, err)
	}

	rep, err = r.fs.RestoreSnapshot(ctx, "/.snapshots/host-m1/s1", "/")
	must(t, err)
	if rep.Files != 0 || rep.Links != 1 || rep.Dirs != 0 || !slices.Equal(rep.Skipped, []string{"/docs/a.bin"}) {
		t.Fatalf("restore over the live tree %+v", rep)
	}
	if got := r.content(t, "/docs/a.bin"); bytes.Equal(got, r.old) {
		t.Fatal("an existing file was overwritten")
	}
	if _, err := r.fs.Readlink("/docs/link"); err != nil {
		t.Fatalf("the missing link did not come back: %v", err)
	}
}

// The versions of a file are its distinct contents across the online mirrors' snapshots,
// the live one first with the snapshots that hold it, the rest newest first; an offline
// mirror is counted, not read (docs/16 §6).
//
// Invariant: I-186
func TestVFS_Versions(t *testing.T) {
	r := newSnapRig(t).withFlusher()
	vs, off, err := r.fs.Versions(ctx, "/docs/a.bin")
	must(t, err)
	if len(vs) != 1 || !vs[0].Current || len(vs[0].Snapshots) != 1 || vs[0].Snapshots[0].Name != "s1" || len(off) != 0 {
		t.Fatalf("unchanged file: %+v %+v", vs, off)
	}
	r.change(t, 21)
	r.f.Clk.Advance(3600e9)
	r.f.TakeSnapshot(r.src, r.m, "s2")
	r.f.Clk.Advance(3600e9)
	r.f.TakeSnapshot(r.src, r.m, "s3") // the same content as s2
	r.change(t, 22)
	vs, _, err = r.fs.Versions(ctx, "/docs/a.bin")
	must(t, err)
	var got [][]string
	for _, v := range vs {
		var ns []string
		for _, s := range v.Snapshots {
			ns = append(ns, s.Name)
		}
		got = append(got, ns)
	}
	if len(vs) != 3 || !vs[0].Current || !slices.Equal(got[0], nil) || !slices.Equal(got[1], []string{"s3", "s2"}) || !slices.Equal(got[2], []string{"s1"}) {
		t.Fatalf("versions %v (%+v)", got, vs)
	}
	if vs[2].Size != uint64(len(r.old)) {
		t.Fatalf("s1's version size %d", vs[2].Size)
	}
	r.src.SetOnline(r.m, false)
	vs, off, err = r.fs.Versions(ctx, "/docs/a.bin")
	must(t, err)
	if len(vs) != 1 || len(off) != 1 || off[0].Mirror != "host-m1" || off[0].Snapshots != 3 {
		t.Fatalf("offline: %+v %+v", vs, off)
	}
}

// A restored file appears under its name only whole: its data lands under a temporary
// name in the same folder, renamed into place once committed. A restore that fails
// between (a mirror lost, the CLI interrupted) leaves no entry under the name, so a
// rerun restores the file instead of skipping an empty one; a temporary file a stopped
// node left behind is removed by the rerun (docs/16 §6).
//
// Invariant: I-186
func TestVFS_RestoreTree_InterruptedFileIsRestoredByRerun(t *testing.T) {
	r := newSnapRig(t).withFlusher()
	r.change(t, 12)
	r.f.MustApply(meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: 1 << 62}}) // the snapshot's chunks now come from the mirror
	r.src.FailObjects(errors.New("mirror unplugged"))
	if _, err := r.fs.RestoreSnapshot(ctx, "/.snapshots/host-m1/s1", "/restored"); err == nil {
		t.Fatal("a restore whose mirror objects fail succeeded")
	}
	if _, err := r.fs.Stat("/restored/docs/a.bin"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after the failed restore the file's name exists: %v", err)
	}

	// a node stopped mid-file leaves the temporary file: the rerun removes it
	tmp := "/restored/docs/" + vfs.RestoreTempName("a.bin")
	writeFile(t, r.fs, tmp, []byte("left by a stopped node"))
	r.src.FailObjects(nil)
	rep, err := r.fs.RestoreSnapshot(ctx, "/.snapshots/host-m1/s1", "/restored")
	must(t, err)
	if rep.Files != 1 || slices.Contains(rep.Skipped, "/restored/docs/a.bin") {
		t.Fatalf("the rerun %+v", rep)
	}
	if got := r.content(t, "/restored/docs/a.bin"); !bytes.Equal(got, r.old) {
		t.Fatalf("the rerun's file differs at %d", testharness.FirstDiff(got, r.old))
	}
	fis, err := r.fs.ReadDir("/restored/docs")
	must(t, err)
	if got := names(fis); !slices.Equal(got, []string{"a.bin", "link"}) {
		t.Fatalf("/restored/docs lists %v: a temporary file is left", got)
	}
}
