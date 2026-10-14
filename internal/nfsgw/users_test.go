package nfsgw_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// bindMap is a device's bindings for a test: credential to user name.
type bindMap map[string]string

func (b bindMap) Lookup(cred string) (string, bool) { u, ok := b[cred]; return u, ok }

// usersGateway is a gateway whose device binds uid 501 to kim (the admin) and 502 to
// lee, with /w a folder lee holds Write on and /r one they hold Read on, each with a
// file of 8 KiB.
func usersGateway(t *testing.T) (g *gateway, kim, lee meta.UserIdx) {
	t.Helper()
	g = startGatewayBound(t, nil, bindMap{"uid:501": "kim", "uid:502": "lee"})
	kim, lee = g.f.Users()
	for _, d := range []string{"/w", "/r", "/other"} {
		if _, err := g.tgt.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := g.tgt.Create(d+"/f", 0o644); err != nil {
			t.Fatal(err)
		}
		g.writeAt(d+"/f", 4096, testharness.Pattern(4096, 3))
	}
	g.f.Grant(kim, "/w", lee, meta.RoleWrite)
	g.f.Grant(kim, "/r", lee, meta.RoleRead)
	return g, kim, lee
}

// The uid a call carries names its actor: lee's calls under /w land (and kim reads the
// bytes back), under /r and at the root they are NFS3ERR_ACCES, and a revoked grant
// refuses the next call. Two targets on one connection differ only by their credential.
// Invariant: I-351, I-350
func TestNFS_AuthSysActor(t *testing.T) {
	g, kim, lee := usersGateway(t)
	lt := g.targetAs(502)
	if _, err := lt.Mkdir("/w/sub", 0o755); err != nil {
		t.Fatalf("lee's mkdir under /w: %v", err)
	}
	fh, err := lt.OpenFile("/w/f", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	want := testharness.Pattern(1000, 9)
	if _, err := fh.Seek(12000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(want); err != nil {
		t.Fatalf("lee's write under /w: %v", err)
	}
	_ = fh.Close()
	if got := g.readAll("/w/f", 13000); !bytes.Equal(got[12000:], want) {
		t.Fatal("kim reads other bytes than lee wrote")
	}
	for _, tc := range []struct {
		name string
		got  int
	}{
		{"mkdir at the root", mkdirStatus(lt, "/top")},
		{"create under /r", createStatus(lt, "/r/new")},
		{"lookup elsewhere", lookupStatus(lt, "/other")},
		{"lookup a file elsewhere", lookupStatus(lt, "/other/f")},
		{"remove under /r", wireStatus(lt.Remove("/r/f"))},
	} {
		if tc.got != nfsc.NFS3ErrAcces {
			t.Errorf("%s: status %d, want NFS3ERR_ACCES", tc.name, tc.got)
		}
	}
	g.f.Grant(kim, "/w", lee, meta.RoleNone)
	if st := mkdirStatus(lt, "/w/after"); st != nfsc.NFS3ErrAcces {
		t.Fatalf("after the revoke: status %d, want NFS3ERR_ACCES", st)
	}
	_ = lee
}

// An unbound uid is nobody once the cluster has users: the root lists empty, and every
// look-up and mutation is NFS3ERR_ACCES. Before the first user, every uid is served the
// node's filesystem, so an upgraded volume keeps working until its first user exists.
// Invariant: I-350, I-349
func TestNFS_UnboundIsNobody(t *testing.T) {
	g := startGatewayBound(t, nil, bindMap{"uid:501": "kim"})
	ut := g.targetAs(503)
	if _, err := ut.Mkdir("/before", 0o755); err != nil {
		t.Fatalf("an unbound uid before the first user: %v", err)
	}
	g.f.Users()
	ents, err := ut.ReadDirPlus("/")
	if err != nil {
		t.Fatalf("nobody's root listing: %v", err)
	}
	for _, e := range ents {
		if e.FileName != "." && e.FileName != ".." {
			t.Fatalf("nobody's root lists %q", e.FileName)
		}
	}
	for _, tc := range []struct {
		name string
		got  int
	}{
		{"lookup", lookupStatus(ut, "/before")},
		{"mkdir", mkdirStatus(ut, "/x")},
		{"create", createStatus(ut, "/before/f")},
	} {
		if tc.got != nfsc.NFS3ErrAcces {
			t.Errorf("%s: status %d, want NFS3ERR_ACCES", tc.name, tc.got)
		}
	}
	if _, err := g.tgt.Mkdir("/kim", 0o755); err != nil { // the bound uid works
		t.Fatal(err)
	}
}

// The mode bits a user sees follow their role: the stored mode masked to 0500 under
// Read and 0700 under Write or above; a SETATTR of mode or owner is stored and never
// consulted, so a file set 0444 by lee is still written by lee.
// Invariant: I-351
func TestNFS_ModeFromRole(t *testing.T) {
	g, _, _ := usersGateway(t)
	lt := g.targetAs(502)
	for path, want := range map[string]os.FileMode{"/r": 0o500, "/r/f": 0o400, "/w": 0o700, "/w/f": 0o600} {
		fi, _, err := lt.Lookup(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("lee's %s: mode %04o, want %04o", path, got, want)
		}
	}
	if fi, _, _ := g.tgt.Lookup("/r"); fi.Mode().Perm() != 0o700 {
		t.Errorf("the admin's /r: mode %04o, want 0700", fi.Mode().Perm())
	}
	if err := lt.Setattr("/w/f", nfsc.Sattr3{Mode: nfsc.SetMode{SetIt: true, Mode: 0o444}, UID: nfsc.SetUID{SetIt: true, UID: 999}}); err != nil {
		t.Fatalf("setattr by lee: %v", err)
	}
	fi, _, err := lt.Lookup("/w/f")
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o400 {
		t.Errorf("after chmod 444 under Write: mode %04o, want 0400 (stored 0444 masked)", got)
	}
	fh, err := lt.OpenFile("/w/f", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("still writable")); err != nil {
		t.Fatalf("write after chmod 444: %v (the mode is never consulted)", err)
	}
	_ = fh.Close()
}

func mkdirStatus(tgt *nfsc.Target, path string) int {
	_, err := tgt.Mkdir(path, 0o755)
	return wireStatus(err)
}

func createStatus(tgt *nfsc.Target, path string) int {
	_, err := tgt.Create(path, 0o644)
	return wireStatus(err)
}

func lookupStatus(tgt *nfsc.Target, path string) int {
	_, _, err := tgt.Lookup(path)
	return wireStatus(err)
}
