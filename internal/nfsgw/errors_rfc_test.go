package nfsgw_test

import (
	"errors"
	"os"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// The namespace procedures answer the status RFC 1813 names for what went wrong, so a
// client shows "Directory not empty", "File exists", "Invalid argument" or "File too
// large" rather than "Input/output error": RMDIR of a folder with entries
// NFS3ERR_NOTEMPTY, a rename onto one NFS3ERR_EXIST (§3.3.14), a folder renamed under
// itself NFS3ERR_INVAL, MKDIR of a name that holds a file NFS3ERR_EXIST, a SETATTR size past the largest file NFS3ERR_FBIG
// (an NFS status, not an RPC failure).
// Invariant: I-238
func TestNFS_NamespaceErrors_RFCStatuses(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/d/f", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Mkdir("/e", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/e/g", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/file", 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := g.tgt.Mkdir("/file", 0o755)
	big := meta.MaxFileSize(testharness.ChunkSize) + 1
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"rmdir of a folder with a file", wireStatus(g.tgt.RmDir("/d")), nfsc.NFS3ErrNotEmpty},
		{"rename onto a folder with a file", existStatus(g.tgt.Rename("/d", "/e")), nfsc.NFS3ErrExist},
		{"a folder renamed under itself", wireStatus(g.tgt.Rename("/d", "/d/sub")), nfsc.NFS3ErrInval},
		{"mkdir of a file's name", existStatus(err), nfsc.NFS3ErrExist},
		{"setattr size past the largest file", wireStatus(g.tgt.Setattr("/file", nfsc.Sattr3{Size: nfsc.SetSize{SetIt: true, Size: big}})), nfsc.NFS3ErrFBig},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// MKDIR is exclusive across gateways, as CREATE GUARDED is (I-222): another gateway's
// folder made between this gateway's lookup and its own mkdir is NFS3ERR_EXIST, never a
// success that lets two devices both think they made it (the mkdir-as-lock idiom).
// Invariant: I-238
func TestNFS_Mkdir_RacingBackend_Exist(t *testing.T) {
	g := startGateway(t)
	g.f.ForestallNext(meta.OpMkdir, func() {
		g.f.MustApply(meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: "lock", Mode: 0o755, Now: 1}})
	})
	if _, err := g.tgt.Mkdir("/lock", 0o755); existStatus(err) != nfsc.NFS3ErrExist {
		t.Fatalf("mkdir of a folder another gateway made meanwhile: %v, want NFS3ERR_EXIST", err)
	}
}

// existStatus is wireStatus that reads NFS3ERR_EXIST through the client, which turns it
// into os.ErrExist.
func existStatus(err error) int {
	if errors.Is(err, os.ErrExist) {
		return nfsc.NFS3ErrExist
	}
	return wireStatus(err)
}
