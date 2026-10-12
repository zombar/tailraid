package nfsgw_test

import (
	"bytes"
	"slices"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// snapGateway serves a volume holding /a and /d over NFS with snapshot s1 of it on mirror
// host/m1, and returns the bytes /a had then (it has changed since).
func snapGateway(t *testing.T) (*gateway, *testharness.Mirrors, meta.NodeIdx, meta.Snapshot, []byte) {
	t.Helper()
	g := startGateway(t)
	old := testharness.Pattern(150<<10, 3)
	g.writeAt("/a", 0, old)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := g.f.Flusher().FlushAll(ctx); err != nil {
		t.Fatal(err)
	}
	src := testharness.NewMirrors()
	m := g.f.AddMirror(src, "host/m1")
	sn := g.f.TakeSnapshot(src, m, "s1")
	g.fs.SetMirrors(src)
	g.writeAt("/a", 0, []byte("changed since"))
	return g, src, m, sn, old
}

// Over the wire: /.snapshots is not in the root's READDIRPLUS, LOOKUP finds it and hands
// out 24-byte handles, a READ returns the snapshot's bytes, every mutation under it is
// NFS3ERR_ROFS and a RENAME across it NFS3ERR_NOTSUPP; offline reads are NFS3ERR_IO and a
// gone row's handle NFS3ERR_STALE (docs/07, I-185).
//
// Invariant: I-185
func TestNFS_Snapshots_OverTheWire(t *testing.T) {
	g, src, m, sn, old := snapGateway(t)
	const file = "/.snapshots/host-m1/s1/a"

	root, err := g.tgt.ReadDirPlus("/")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range root {
		if e.FileName == ".snapshots" {
			t.Fatal("/.snapshots is listed at the root")
		}
	}
	ents, err := g.tgt.ReadDirPlus("/.snapshots/host-m1/s1")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, e := range ents {
		if e.FileName != "." && e.FileName != ".." {
			listed = append(listed, e.FileName)
		}
	}
	slices.Sort(listed)
	if !slices.Equal(listed, []string{"a", "d"}) {
		t.Fatalf("s1 lists %v", listed)
	}
	_, fh, err := g.tgt.Lookup(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(fh) != meta.SnapHandleLen {
		t.Fatalf("snapshot handle of %d bytes", len(fh))
	}
	if got := g.readAll(file, len(old)+10); !bytes.Equal(got, old) {
		t.Fatalf("read differs at %d", testharness.FirstDiff(got, old))
	}

	const (
		eio     = 5
		rofs    = 30
		stale   = 70
		notsupp = 10004
	)
	got := map[string]int{}
	_, err = g.tgt.Mkdir("/.snapshots/host-m1/s1/x", 0o755)
	got["mkdir"] = wireStatus(err)
	_, err = g.tgt.Create("/.snapshots/host-m1/s1/y", 0o644)
	got["create"] = wireStatus(err)
	got["remove"] = wireStatus(g.tgt.Remove(file))
	got["rmdir"] = wireStatus(g.tgt.RemoveAll("/.snapshots/host-m1/s1/d"))
	got["rename within"] = wireStatus(g.rename(file, "/.snapshots/host-m1/s1/b"))
	got["rename in"] = wireStatus(g.rename("/a", "/.snapshots/host-m1/s1/b"))
	got["symlink"] = wireStatus(g.tgt.Symlink("/a", "/.snapshots/host-m1/s1/s"))
	got["setattr"] = wireStatus(g.tgt.Setattr(file, nfsc.Sattr3{Mode: nfsc.SetMode{SetIt: true, Mode: 0o600}}))
	if w, err := g.tgt.OpenFile(file, 0o644); err == nil {
		_, err = w.Write([]byte("x"))
		got["write"] = wireStatus(err)
		_ = w.Close()
	} else {
		got["write"] = wireStatus(err)
	}
	want := map[string]int{"mkdir": rofs, "create": rofs, "remove": rofs, "rmdir": rofs, "rename within": rofs, "rename in": notsupp, "symlink": rofs, "setattr": rofs, "write": rofs}
	for op, st := range want {
		if got[op] != st {
			t.Errorf("%s under /.snapshots: NFS3 status %d, want %d", op, got[op], st)
		}
	}
	if got := g.readAll(file, 4); !bytes.Equal(got, old[:4]) {
		t.Fatal("the snapshot changed")
	}

	src.SetOnline(m, false)
	if _, err := g.tgt.ReadDirPlus("/.snapshots/host-m1"); err != nil {
		t.Fatalf("an offline mirror's names: %v", err)
	}
	if fh, err := g.tgt.Open("/.snapshots/host-m1/s1/d"); err == nil {
		_ = fh.Close()
		t.Fatal("a lookup inside an offline mirror's snapshot succeeded")
	} else if st := wireStatus(err); st != eio && st != stale {
		t.Fatalf("offline: status %d", st)
	}
	src.SetOnline(m, true)
	g.f.MustApply(meta.Command{Op: meta.OpSnapshotDelete, SnapshotDelete: &meta.SnapshotDelete{ID: sn.ID, Now: 9}})
	if _, _, err := g.tgt.Lookup(file); err == nil {
		t.Fatal("a deleting snapshot still resolves")
	}
}
