package smbgw_test

import (
	"testing"

	"github.com/zombar/tailraid/internal/meta"
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
