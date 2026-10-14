package smbgw_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/cloudsoda/go-smb2"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/testharness"
)

// usersGateway is a gateway whose device binds the mount account to kim (the admin)
// and the account "lee" to lee, with /w a folder lee holds Write on and /r one they
// hold Read on, each holding a file of 8 KiB; "guest" is an account bound to nobody.
func usersGateway(t *testing.T) (g *gateway, kim, lee meta.UserIdx) {
	t.Helper()
	g = startGatewayBound(t, nil, map[string]string{"smb:" + smbgw.User: "kim", "smb:lee": "lee", "smb:guest": ""})
	kim, lee = g.f.Users()
	for _, d := range []string{"w", "r", "other"} {
		must(t, g.sh.Mkdir(d, 0o755))
		must(t, g.sh.WriteFile(d+"/f", testharness.Pattern(8192, 3), 0o644))
	}
	g.f.Grant(kim, "/w", lee, meta.RoleWrite)
	g.f.Grant(kim, "/r", lee, meta.RoleRead)
	return g, kim, lee
}

// A tree connect is bound to the user its session account maps to: lee's session
// writes under /w at an offset (and the mount account reads the bytes back), is
// STATUS_ACCESS_DENIED under /r, at the root and elsewhere, and a revoked grant refuses
// the next request; the mount account stays the admin's.
// Invariant: I-352
func TestSMB_TreeConnectActor(t *testing.T) {
	g, kim, lee := usersGateway(t)
	ls := g.mountAs("lee")
	must(t, ls.Mkdir("w/sub", 0o755))
	fh, err := ls.OpenFile("w/f", os.O_RDWR, 0)
	must(t, err)
	want := testharness.Pattern(1000, 9)
	if _, err := fh.WriteAt(want, 12000); err != nil {
		t.Fatalf("lee's write under /w: %v", err)
	}
	must(t, fh.Close())
	got, err := g.sh.ReadFile("w/f")
	must(t, err)
	if len(got) != 13000 || !bytes.Equal(got[12000:], want) {
		t.Fatal("the mount account reads other bytes than lee wrote")
	}
	for _, tc := range []struct {
		name string
		got  uint32
	}{
		{"mkdir at the root", ntStatus(ls.Mkdir("top", 0o755))},
		{"create under /r", ntStatus(ls.WriteFile("r/new", []byte("x"), 0o644))},
		{"stat elsewhere", statStatus(ls, "other")},
		{"stat a file elsewhere", statStatus(ls, "other/f")},
		{"remove under /r", ntStatus(ls.Remove("r/f"))},
	} {
		if tc.got != statusAccessDenied {
			t.Errorf("%s: status %#x, want STATUS_ACCESS_DENIED", tc.name, tc.got)
		}
	}
	g.f.Grant(kim, "/w", lee, meta.RoleNone)
	if st := ntStatus(ls.Mkdir("w/after", 0o755)); st != statusAccessDenied {
		t.Fatalf("after the revoke: status %#x, want STATUS_ACCESS_DENIED", st)
	}
	if err := g.sh.Mkdir("admin", 0o755); err != nil {
		t.Fatalf("the mount account after lee's revoke: %v", err)
	}
}

// An account bound to no user is nobody once the cluster has users: its listing of the
// share is empty and every request is STATUS_ACCESS_DENIED; before the first user an
// unbound account is served the node's filesystem.
// Invariant: I-352, I-350
func TestSMB_UnboundDenied(t *testing.T) {
	g := startGatewayBound(t, nil, map[string]string{"smb:" + smbgw.User: "kim", "smb:guest": ""})
	gs := g.mountAs("guest")
	must(t, gs.Mkdir("before", 0o755))
	g.f.Users()
	ents, err := gs.ReadDir(".")
	if err != nil {
		t.Fatalf("nobody's listing of the share: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("nobody's listing shows %d entries", len(ents))
	}
	for _, tc := range []struct {
		name string
		got  uint32
	}{
		{"stat", statStatus(gs, "before")},
		{"mkdir", ntStatus(gs.Mkdir("x", 0o755))},
		{"create", ntStatus(gs.WriteFile("before/f", []byte("x"), 0o644))},
	} {
		if tc.got != statusAccessDenied {
			t.Errorf("%s: status %#x, want STATUS_ACCESS_DENIED", tc.name, tc.got)
		}
	}
	must(t, g.sh.Mkdir("kim", 0o755)) // the bound account works
}

func statStatus(sh *smb2.Share, name string) uint32 {
	_, err := sh.Stat(name)
	return ntStatus(err)
}
