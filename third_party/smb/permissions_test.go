package smb_test

import (
	"errors"
	"os"
	"testing"

	fssmb "github.com/go-filesystems/smb"
)

// Two people on one server, judged by a client this project did not write.
//
// The interesting part is what go-smb2 makes of the refusal: it turns
// STATUS_ACCESS_DENIED into os.ErrPermission, so the assertion is not "the
// server sent the number we chose" but "the client understood it as a
// permission". A share nobody may see is only useful if the client says why.
func TestAShareCanBeLimitedToSomePeople(t *testing.T) {
	addr, srv := serve(t)
	srv.AddUser("bob", "swordfish")
	if err := srv.Share("alices", newMemFS(true), fssmb.AllowUsers("alice")); err != nil {
		t.Fatal(err)
	}
	if err := srv.Share("common", newMemFS(true)); err != nil {
		t.Fatal(err)
	}

	alice, doneA := dial(t, addr, "alice", "hunter2")
	defer doneA()
	for _, name := range []string{"alices", "common"} {
		fs, err := alice.Mount(name)
		if err != nil {
			t.Fatalf("alice mounting %s: %v", name, err)
		}
		fs.Umount()
	}

	bob, doneB := dial(t, addr, "bob", "swordfish")
	defer doneB()
	if fs, err := bob.Mount("common"); err != nil {
		t.Errorf("bob mounting the share he is allowed on: %v", err)
	} else {
		fs.Umount()
	}
	_, err := bob.Mount("alices")
	if err == nil {
		t.Fatal("bob mounted a share he is not allowed on")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("bob was refused with %v, want a permission error", err)
	}
}

// One share, two people, one of them a reader: they hold it at the same time
// and get different answers, which is the whole point of deciding this on the
// connection rather than on the share.
func TestAReaderAndAWriterHoldTheSameShare(t *testing.T) {
	mem := newMemFS(true)
	addr, srv := serve(t)
	srv.AddUser("bob", "swordfish")
	if err := srv.Share("shared", mem, fssmb.WriteUsers("alice")); err != nil {
		t.Fatal(err)
	}

	alice, doneA := dial(t, addr, "alice", "hunter2")
	defer doneA()
	awrite, err := alice.Mount("shared")
	if err != nil {
		t.Fatal(err)
	}
	defer awrite.Umount()

	bob, doneB := dial(t, addr, "bob", "swordfish")
	defer doneB()
	aread, err := bob.Mount("shared")
	if err != nil {
		t.Fatalf("bob mounting a share he may read: %v", err)
	}
	defer aread.Umount()

	if err := awrite.WriteFile("note.txt", []byte("from alice"), 0o644); err != nil {
		t.Fatalf("alice writing to a share she may write: %v", err)
	}
	// Bob reads what Alice wrote, on the mount he already had.
	if got, err := aread.ReadFile("note.txt"); err != nil || string(got) != "from alice" {
		t.Errorf("bob read %q, %v", got, err)
	}
	if err := aread.WriteFile("bob.txt", []byte("nope"), 0o644); err == nil {
		t.Error("a reader wrote to the share")
	}
	if _, err := mem.ReadFile("/bob.txt"); !os.IsNotExist(err) {
		t.Error("the refused write reached the filesystem anyway")
	}
	// And removing is a write too.
	if err := aread.Remove("note.txt"); err == nil {
		t.Error("a reader removed a file")
	}
	if _, err := mem.ReadFile("/note.txt"); err != nil {
		t.Errorf("the file a reader tried to remove is gone: %v", err)
	}
}
