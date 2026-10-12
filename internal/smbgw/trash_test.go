package smbgw_test

import (
	"errors"
	"os"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/testharness"
)

// A delete on the share is a delete to the share: the name is not found, the listing
// omits it, a create of the name succeeds, while the entry waits in the trash.
//
// Invariant: I-155
func TestSMB_DeletedIsGoneFromShare(t *testing.T) {
	g := startGateway(t)
	must(t, g.sh.Mkdir("d", 0o755))
	g.create("d/f")
	must(t, g.writeAt("/d/f", 0, []byte("deleted on the share")))
	ino := g.f.InodeOf(g.f.FS(), "/d/f")
	must(t, g.sh.Remove("d/f"))
	if _, err := g.sh.Stat("d/f"); ntStatus(err) != statusObjectNotFound {
		t.Fatalf("stat after delete: %v", err)
	}
	ents, err := g.sh.ReadDir("d")
	must(t, err)
	if len(ents) != 0 {
		t.Fatalf("listing after delete: %v", ents)
	}
	g.create("d/f")
	if roots := g.f.FSM.TrashOf(g.f.InodeOf(g.f.FS(), "/d")); len(roots) != 1 || roots[0].Ino != ino {
		t.Fatalf("trash: %+v", roots)
	}
}

// A delete under a protected directory is refused where SMB can say so: SET_INFO
// disposition answers STATUS_ACCESS_DENIED and the file stays.
//
// Invariant: I-154
func TestSMB_Protected_AccessDenied(t *testing.T) {
	g := startGateway(t)
	must(t, g.sh.Mkdir("p", 0o755))
	g.create("p/f")
	p := g.f.InodeOf(g.f.FS(), "/p")
	g.f.MustApply(meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: p, Mask: meta.AttrFlags, Flags: meta.FlagProtected, Now: 1}})
	if err := g.sh.Remove("p/f"); ntStatus(err) != statusAccessDenied {
		t.Fatalf("delete under protection: %v (status %#x), want STATUS_ACCESS_DENIED", err, ntStatus(err))
	}
	if _, err := g.sh.Stat("p/f"); err != nil {
		t.Fatalf("the protected file is gone: %v", err)
	}
	if err := g.sh.Rename("p/f", "f"); ntStatus(err) != statusAccessDenied {
		t.Fatalf("rename out of protection: %v", err)
	}
	// new content into the folder is fine
	g.create("p/new")
}

// A failed copy into a protected folder cleans up after itself: the adapter lets the
// creating handle's delete through for a file, the file leaves the share for the trash,
// and while a member is too old for the own cleanup the plain refusal is the answer.
// Invariant: I-220
func TestSMB_FailedCopyCleanup_UnderProtected(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	a := smbgw.New(f.FS(), f.FSM)
	must(t, a.MkDir("/p", 0o755))
	must(t, a.WriteFile("/p/copy.bin", nil, 0o644))
	fh, err := a.OpenFile("/p/copy.bin")
	must(t, err)
	w := fh.(interface {
		Truncate(int64) error
		WriteAt([]byte, int64) (int, error)
	})
	must(t, w.Truncate(3*int64(testharness.ChunkSize)))
	_, err = w.WriteAt(testharness.Pattern(int(testharness.ChunkSize), 5), 0)
	must(t, err)
	must(t, fh.Close())
	f.MustApply(meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: f.InodeOf(f.FS(), "/p"), Mask: meta.AttrFlags, Flags: meta.FlagProtected, Now: 1}})
	// an older member: the own cleanup is refused for its codec, the plain delete for protection
	f.SetLeaderCodec(7) // before codec 8 (I-220); not CodecVersion-1, which moves with every bump
	if err := a.DeleteOwnFile("/p/copy.bin"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("own cleanup with an older member: %v, want a permission error", err)
	}
	f.SetLeaderCodec(0)
	must(t, a.CanDeleteOwn("/p/copy.bin"))
	must(t, a.DeleteOwnFile("/p/copy.bin"))
	if _, err := a.Stat("/p/copy.bin"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the failed copy's file is still on the share: %v", err)
	}
	if roots := f.FSM.TrashOf(f.InodeOf(f.FS(), "/p")); len(roots) != 1 {
		t.Fatalf("trash roots under /p: %d, want the cleaned-up copy", len(roots))
	}
}

// Under protection the creating handle's allowance covers files only, and any other delete
// of an existing file or folder is refused as before.
// Invariant: I-220, I-154
func TestSMB_DeleteExistingUnderProtected_StillRefused(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	a := smbgw.New(f.FS(), f.FSM)
	must(t, a.MkDir("/p/sub", 0o755))
	must(t, a.WriteFile("/p/kept.bin", []byte("x"), 0o644))
	f.MustApply(meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: f.InodeOf(f.FS(), "/p"), Mask: meta.AttrFlags, Flags: meta.FlagProtected, Now: 1}})
	if err := a.CanDelete("/p/kept.bin"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("CanDelete of an existing file: %v", err)
	}
	if err := a.DeleteFile("/p/kept.bin"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("DeleteFile of an existing file: %v", err)
	}
	if err := a.CanDeleteOwn("/p/sub"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("CanDeleteOwn of a folder: %v", err)
	}
	if _, err := a.Stat("/p/kept.bin"); err != nil {
		t.Fatalf("the protected file is gone: %v", err)
	}
}
