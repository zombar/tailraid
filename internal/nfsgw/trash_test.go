package nfsgw_test

import (
	"errors"
	"os"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"

	"github.com/zombar/tailraid/internal/meta"
)

// A REMOVE on the mount is a delete to the share (LOOKUP NOENT, READDIR omits it, the
// handle is stale, the name is free) while the entry waits in the trash.
//
// Invariant: I-155
func TestNFS_TrashedIsGoneFromShare(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/d/f", 0o644); err != nil {
		t.Fatal(err)
	}
	g.writeAt("/d/f", 0, []byte("deleted on the share"))
	_, fh, err := g.tgt.Lookup("/d/f")
	if err != nil {
		t.Fatal(err)
	}
	ino := g.f.InodeOf(g.f.FS(), "/d/f")
	must(t, g.tgt.Remove("/d/f"))
	if _, _, err := g.tgt.Lookup("/d/f"); !errors.Is(err, os.ErrNotExist) && wireStatus(err) != int(nfsc.NFS3ErrNoEnt) {
		t.Fatalf("LOOKUP after remove: %v", err)
	}
	ents, err := g.tgt.ReadDirPlus("/d")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.FileName == "f" {
			t.Fatal("READDIR lists the deleted file")
		}
	}
	if _, err := g.tgt.GetAttr(fh); !isStale(err) {
		t.Fatalf("handle after remove: %v", err)
	}
	if _, err := g.tgt.Create("/d/f", 0o644); err != nil {
		t.Fatalf("the name is not free: %v", err)
	}
	// the entry is in the trash with its bytes
	if roots := g.f.FSM.TrashOf(g.f.InodeOf(g.f.FS(), "/d")); len(roots) != 1 || roots[0].Ino != ino {
		t.Fatalf("trash: %+v", roots)
	}
	must(t, g.f.Flusher().FlushAll(ctx))
	if in, _ := g.f.FSM.GetInodeAny(ino); in.Size != 20 {
		t.Fatalf("the deleted file's bytes: %+v", in)
	}
}

// A handle that went stale at the delete resolves again after a restore (I-16's
// identity never changed).
//
// Invariant: I-155
// Lifecycle: L-200
func TestNFS_TrashedHandleStale_RestoredValid(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Create("/f", 0o644); err != nil {
		t.Fatal(err)
	}
	g.writeAt("/f", 0, []byte("hello"))
	_, fh, _ := g.tgt.Lookup("/f")
	ino := g.f.InodeOf(g.f.FS(), "/f")
	must(t, g.tgt.Remove("/f"))
	if _, err := g.tgt.GetAttr(fh); !isStale(err) {
		t.Fatalf("handle after remove: %v", err)
	}
	g.f.MustApply(meta.Command{Op: meta.OpRestore, Restore: &meta.Restore{Ino: ino, Now: 5}})
	attr, err := g.tgt.GetAttr(fh)
	if err != nil {
		t.Fatalf("handle after restore: %v", err)
	}
	if attr.Size() != 5 {
		t.Fatalf("size via the restored handle %d", attr.Size())
	}
	if got := g.readAll("/f", 10); string(got) != "hello" {
		t.Fatalf("%q", got)
	}
}

// Under a protected directory REMOVE, RMDIR and a RENAME out answer NFS3ERR_ACCES.
//
// Invariant: I-154
func TestNFS_Protected_IsACCES(t *testing.T) {
	g := startGateway(t)
	for _, d := range []string{"/p", "/p/sub", "/e"} {
		if _, err := g.tgt.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.tgt.Create("/p/sub/f", 0o644); err != nil {
		t.Fatal(err)
	}
	p := g.f.InodeOf(g.f.FS(), "/p")
	g.f.MustApply(meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: p, Mask: meta.AttrFlags, Flags: meta.FlagProtected, Now: 1}})
	acces := int(nfsc.NFS3ErrAcces)
	if st := wireStatus(g.tgt.Remove("/p/sub/f")); st != acces {
		t.Fatalf("REMOVE under protection: status %d, want ACCES", st)
	}
	if st := wireStatus(g.tgt.RemoveAll("/p/sub")); st != acces && st != -1 {
		t.Fatalf("RMDIR under protection: status %d", st)
	}
	if st := wireStatus(g.rename("/p/sub/f", "/e/f")); st != acces {
		t.Fatalf("RENAME out of protection: status %d, want ACCES", st)
	}
	if _, _, err := g.tgt.Lookup("/p/sub/f"); err != nil {
		t.Fatalf("the protected file is gone: %v", err)
	}
	// into, and new content: fine
	if _, err := g.tgt.Create("/p/new", 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, g.rename("/p/new", "/p/sub/new"))
}
