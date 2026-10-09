package smb_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"testing"

	fssmb "github.com/go-filesystems/smb"
)

// The mount, done by a client this project did not write: make a file, write
// it, read it back, list the directory, stat it, and remove it.
//
// It runs twice over two filesystems that differ in ONE capability -- one
// answers OpenFile and one does not -- because the fallback path is a
// different set of statements and a mount that works over one must work over
// the other.
func TestAFileGoesThereAndComesBack(t *testing.T) {
	for _, positional := range []bool{true, false} {
		name := "with positional access"
		if !positional {
			name = "with only whole-file access"
		}
		t.Run(name, func(t *testing.T) {
			mem := newMemFS(positional)
			addr, srv := serve(t)
			if err := srv.Share("disk", mem); err != nil {
				t.Fatal(err)
			}
			s, done := dial(t, addr, "alice", "hunter2")
			defer done()
			fs, err := s.Mount("disk")
			if err != nil {
				t.Fatal(err)
			}
			defer fs.Umount()

			want := bytes.Repeat([]byte("the quick brown fox "), 500) // 10 kB, several blocks
			if err := fs.WriteFile("greeting.txt", want, 0o644); err != nil {
				t.Fatalf("writing: %v", err)
			}
			got, err := fs.ReadFile("greeting.txt")
			if err != nil {
				t.Fatalf("reading: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("read back %d bytes of %d, and they differ", len(got), len(want))
			}
			// …and the driver underneath holds the same bytes, which is what
			// says the server wrote through rather than remembered.
			if inDriver, err := mem.ReadFile("/greeting.txt"); err != nil || !bytes.Equal(inDriver, want) {
				t.Errorf("the filesystem holds %d bytes (%v), not what was written", len(inDriver), err)
			}

			st, err := fs.Stat("greeting.txt")
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if st.Size() != int64(len(want)) {
				t.Errorf("stat says %d bytes, want %d", st.Size(), len(want))
			}
			if st.IsDir() {
				t.Error("stat says a file is a directory")
			}

			if err := fs.Mkdir("sub", 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := fs.WriteFile("sub/inner.txt", []byte("inside"), 0o644); err != nil {
				t.Fatalf("writing inside a directory: %v", err)
			}

			names := readdir(t, fs, ".")
			if want := []string{"greeting.txt", "sub"}; !equal(names, want) {
				t.Errorf("the root lists %v, want %v", names, want)
			}
			if names := readdir(t, fs, "sub"); !equal(names, []string{"inner.txt"}) {
				t.Errorf("sub lists %v", names)
			}

			if err := fs.Remove("greeting.txt"); err != nil {
				t.Errorf("removing: %v", err)
			}
			if _, err := fs.Stat("greeting.txt"); err == nil {
				t.Error("the file is still there after being removed")
			}
		})
	}
}

// A read at an offset is a read at that offset, and one past the end says so
// rather than returning zeros.
func TestReadingAtAnOffset(t *testing.T) {
	mem := newMemFS(true)
	addr, srv := serve(t)
	if err := srv.Share("disk", mem); err != nil {
		t.Fatal(err)
	}
	body := []byte("0123456789abcdefghij")
	if err := mem.WriteFile("/f.txt", body, 0o644); err != nil {
		t.Fatal(err)
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()
	fs, err := s.Mount("disk")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Umount()

	f, err := fs.Open("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 10); err != nil || n != 5 || string(buf) != "abcde" {
		t.Errorf("ReadAt(10) = %q, %d, %v", buf[:n], n, err)
	}
	if _, err := f.ReadAt(buf, int64(len(body))); err != io.EOF {
		t.Errorf("reading past the end returned %v, want io.EOF", err)
	}
}

// A share exported read-only refuses the write, and says which kind of refusal
// it is: the medium, not the permissions.
func TestAReadOnlyShareRefusesWrites(t *testing.T) {
	mem := newMemFS(true)
	if err := mem.WriteFile("/there.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	addr, srv := serve(t)
	if err := srv.Share("ro", mem, fssmb.ReadOnly()); err != nil {
		t.Fatal(err)
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()
	fs, err := s.Mount("ro")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Umount()

	if got, err := fs.ReadFile("there.txt"); err != nil || string(got) != "hello" {
		t.Errorf("a read-only share did not serve a read: %q, %v", got, err)
	}
	if err := fs.WriteFile("new.txt", []byte("nope"), 0o644); err == nil {
		t.Error("a read-only share accepted a write")
	}
	if _, err := mem.ReadFile("/new.txt"); !os.IsNotExist(err) {
		t.Error("the refused write reached the filesystem anyway")
	}
}

// A path that climbs out of the share does not: the share is the root, and
// "\..\..\etc\passwd" resolves inside it or not at all.
func TestAPathCannotLeaveTheShare(t *testing.T) {
	mem := newMemFS(true)
	addr, srv := serve(t)
	if err := srv.Share("disk", mem); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/inside.txt", []byte("in"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()
	fs, err := s.Mount("disk")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Umount()

	// Climbing out lands back at the root, where the file IS -- so the check
	// is that it resolves inside rather than that it fails, which is what a
	// filesystem does with "/../x".
	if got, err := fs.ReadFile(`..\..\..\inside.txt`); err != nil || string(got) != "in" {
		t.Errorf("a climbing path read %q, %v", got, err)
	}
	// A stream name is not a path, and this server has no streams.
	if _, err := fs.ReadFile("inside.txt:hidden:$DATA"); err == nil {
		t.Error("a stream name was accepted as a file")
	}
}

func readdir(t *testing.T, fs interface {
	ReadDir(string) ([]os.FileInfo, error)
}, dir string) []string {
	t.Helper()
	entries, err := fs.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Several clients on one share, which is not an exotic case: macOS opens TWO
// connections for a single mount.
//
// The driver underneath has no lock of its own -- see memfs_test.go -- so this
// is a test of the SERVER's serialisation and nothing else. Under -race it
// finds any command that forgot to take the share's lock, which is how the
// missing one was found in the first place.
func TestSeveralClientsOneShare(t *testing.T) {
	mem := newMemFS(true)
	addr, srv := serve(t)
	if err := srv.Share("disk", mem); err != nil {
		t.Fatal(err)
	}

	const clients, each = 4, 15
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			s, done := dial(t, addr, "alice", "hunter2")
			defer done()
			fs, err := s.Mount("disk")
			if err != nil {
				t.Errorf("client %d mounting: %v", c, err)
				return
			}
			defer fs.Umount()
			for i := 0; i < each; i++ {
				name := fmt.Sprintf("c%d-%02d.txt", c, i)
				if err := fs.WriteFile(name, []byte(name), 0o644); err != nil {
					t.Errorf("client %d writing %s: %v", c, name, err)
					return
				}
				got, err := fs.ReadFile(name)
				if err != nil || string(got) != name {
					t.Errorf("client %d read back %q, %v", c, got, err)
					return
				}
				if _, err := fs.ReadDir("."); err != nil {
					t.Errorf("client %d listing: %v", c, err)
					return
				}
			}
		}(c)
	}
	wg.Wait()

	// Everything every client wrote is there: a race that lost a map write
	// would show up as a missing file even when the detector is off.
	entries, err := mem.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != clients*each {
		t.Errorf("the share holds %d files, want %d", len(entries), clients*each)
	}
}

// SMB is caseless and this server says so. The driver underneath may not be --
// memFS is not, and neither is ext4 -- so the promise has to be kept here.
func TestTheShareIsCaselessEvenWhenTheDriverIsNot(t *testing.T) {
	mem := newMemFS(true)
	addr, srv := serve(t)
	if err := srv.Share("disk", mem); err != nil {
		t.Fatal(err)
	}
	s, done := dial(t, addr, "alice", "hunter2")
	defer done()
	fs, err := s.Mount("disk")
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Umount()

	if err := fs.WriteFile("Hello.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir("Sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("Sub/Inner.TXT", []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"hello.txt", "HELLO.TXT", "HeLLo.TxT"} {
		got, err := fs.ReadFile(name)
		if err != nil || string(got) != "hi" {
			t.Errorf("reading %q gave %q, %v", name, got, err)
		}
	}
	// A parent whose case is wrong has to be found before its child can be.
	if got, err := fs.ReadFile("sub/inner.txt"); err != nil || string(got) != "inside" {
		t.Errorf("reading through a folded directory gave %q, %v", got, err)
	}

	// The name is PRESERVED, not folded: a listing shows what was written.
	entries, err := fs.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if want := []string{"Hello.txt", "Sub"}; !equal(names, want) {
		t.Errorf("the listing shows %v, want %v", names, want)
	}
	// …and only the driver's own name is there: folding must not have created
	// a second file.
	if _, err := mem.Stat("/hello.txt"); err == nil {
		t.Error("a second file appeared under the folded name")
	}

	// A name that differs by more than case is still missing.
	if _, err := fs.ReadFile("hello.text"); err == nil {
		t.Error("a name that is not there was found")
	}
}
