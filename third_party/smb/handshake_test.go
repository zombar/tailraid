package smb_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cloudsoda/go-smb2"

	fssmb "github.com/go-filesystems/smb"
)

// The judge here is a client this project did not write: go-smb2, which speaks
// the protocol the way the specification describes rather than the way the
// server happens to. A server tested only against its own client agrees with
// itself.
//
// The macOS and Linux kernel clients are the other judges, in CI and by hand;
// this one is the fast one, and it runs everywhere.
func serve(t *testing.T) (addr string, srv *fssmb.Server) {
	t.Helper()
	srv = fssmb.New()
	srv.AddUser("alice", "hunter2")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), srv
}

func dial(t *testing.T, addr, user, password string) (*smb2.Session, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{User: user, Password: password},
	}
	s, err := d.Dial(ctx, addr)
	if err != nil {
		cancel()
		t.Fatalf("dialing: %v", err)
	}
	return s, func() { s.Logoff(); cancel() }
}

// The whole handshake, end to end: negotiate a dialect, authenticate with
// NTLMv2, and connect to a share.
func TestAClientAuthenticatesAndConnects(t *testing.T) {
	addr, srv := serve(t)
	if err := srv.Share("disk", stubFS{}); err != nil {
		t.Fatal(err)
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()
	fs, err := s.Mount("disk")
	if err != nil {
		t.Fatalf("connecting to the share: %v", err)
	}
	defer fs.Umount()
}

// A wrong password is refused, and the refusal says nothing about whether the
// user exists.
func TestTheWrongPasswordIsRefused(t *testing.T) {
	addr, srv := serve(t)
	if err := srv.Share("disk", stubFS{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, user, password string }{
		{"a wrong password", "alice", "not it"},
		{"a user who does not exist", "mallory", "hunter2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: tc.user, Password: tc.password}}
			if s, err := d.Dial(ctx, addr); err == nil {
				s.Logoff()
				t.Error("the server let it in")
			}
		})
	}
}

// A share nobody exported is refused by name, not by silence.
func TestAShareThatIsNotThere(t *testing.T) {
	addr, srv := serve(t)
	if err := srv.Share("disk", stubFS{}); err != nil {
		t.Fatal(err)
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()
	if fs, err := s.Mount("nope"); err == nil {
		fs.Umount()
		t.Error("the server connected to a share it does not have")
	}
}
