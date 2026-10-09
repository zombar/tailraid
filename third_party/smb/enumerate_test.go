package smb_test

import (
	"strings"
	"testing"

	fssmb "github.com/go-filesystems/smb"
)

// What shares are there? Asked by a client this project did not write, which
// is the whole point: the answer is three formats deep -- IOCTL, DCE/RPC, NDR
// -- and a server that gets any layer wrong hands back something that decodes
// into nonsense rather than nothing.
//
// go-smb2 does it the way macOS and Windows do: TREE_CONNECT to IPC$, open
// \srvsvc, bind, then NetrShareEnum at level 1.
func TestAClientAsksWhatSharesThereAre(t *testing.T) {
	addr, srv := serve(t)
	for _, name := range []string{"photos", "attic", "disk"} {
		if err := srv.Share(name, newMemFS(false)); err != nil {
			t.Fatal(err)
		}
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()

	names, err := s.ListSharenames()
	if err != nil {
		t.Fatalf("listing the shares: %v", err)
	}
	// Sorted, and IPC$ last: the order is deliberate, because a list a person
	// reads must not reshuffle itself between two runs.
	if got := strings.Join(names, ","); got != "attic,disk,photos,IPC$" {
		t.Errorf("the shares came back as %q", got)
	}
}

// A share a person may not connect to is not in their list. A name is not a
// secret, but offering one and then refusing it is worse than not offering it.
func TestTheListIsWhatThisUserMayUse(t *testing.T) {
	addr, srv := serve(t)
	srv.AddUser("bob", "swordfish")
	if err := srv.Share("common", newMemFS(false)); err != nil {
		t.Fatal(err)
	}
	if err := srv.Share("alices", newMemFS(false), fssmb.AllowUsers("alice")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		user, password, want string
	}{
		{"alice", "hunter2", "alices,common,IPC$"},
		{"bob", "swordfish", "common,IPC$"},
	} {
		s, done := dial(t, addr, tc.user, tc.password)
		names, err := s.ListSharenames()
		done()
		if err != nil {
			t.Fatalf("%s listing: %v", tc.user, err)
		}
		if got := strings.Join(names, ","); got != tc.want {
			t.Errorf("%s was shown %q, want %q", tc.user, got, tc.want)
		}
	}
}

// The reply that does not fit. A client offers a buffer; the answer is as much
// as fits, a warning, and the tail waiting on the pipe for the READ that
// follows. Sixty shares with long names is far past the 1 KiB go-smb2 offers.
func TestAListTooBigForTheClientsBuffer(t *testing.T) {
	addr, srv := serve(t)
	var want []string
	for i := range 60 {
		name := "a-share-with-a-name-long-enough-to-fill-a-buffer-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		if err := srv.Share(name, newMemFS(false)); err != nil {
			t.Fatal(err)
		}
		want = append(want, name)
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()

	names, err := s.ListSharenames()
	if err != nil {
		t.Fatalf("listing 60 shares: %v", err)
	}
	if len(names) != len(want)+1 {
		t.Fatalf("%d names came back, want %d", len(names), len(want)+1)
	}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("name %d is %q, want %q", i, names[i], w)
		}
	}
}
