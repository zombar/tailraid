package smbgw_test

import (
	"errors"
	"os"
	"testing"
)

// SMB is caseless and the volume is not: a path a client spells in another case than the
// folders it names reaches those folders, a create or a rename never makes a second folder
// or file that differs from an existing one only by case, a parent that does not exist is
// STATUS_OBJECT_PATH_NOT_FOUND (MS-FSA 2.1.5.1) rather than made on the way, and a rename
// that only changes a file's case renames it.
// Invariant: I-233
func TestSMB_CaseFold_NoTwins(t *testing.T) {
	g := startGateway(t)
	must(t, g.sh.Mkdir("Photos", 0o755))

	// a create through a wrong-case parent lands in the existing folder
	fh, err := g.sh.OpenFile(`photos\new.jpg`, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	must(t, err)
	_, err = fh.WriteAt([]byte("jpeg"), 3)
	must(t, err)
	must(t, fh.Close())
	if _, err := g.fs.Stat("/Photos/new.jpg"); err != nil {
		t.Fatalf("the file is not in /Photos: %v", err)
	}
	if _, err := g.fs.Stat("/photos"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a second folder /photos: %v", err)
	}

	// a parent that does not exist is not made on the way
	if _, err := g.sh.OpenFile(`PHOTOS\2024\x.jpg`, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
		t.Fatal("a create under a missing folder succeeded")
	}
	if _, err := g.fs.Stat("/Photos/2024"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the missing folder was made: %v", err)
	}

	// a rename into a wrong-case folder lands in the existing one
	must(t, g.sh.Rename(`Photos\new.jpg`, `PHOTOS\moved.jpg`))
	if _, err := g.fs.Stat("/Photos/moved.jpg"); err != nil {
		t.Fatalf("the renamed file is not in /Photos: %v", err)
	}
	if _, err := g.fs.Stat("/PHOTOS"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a second folder /PHOTOS: %v", err)
	}

	// a rename that changes only the case renames the file
	must(t, g.sh.Rename(`Photos\moved.jpg`, `Photos\Moved.jpg`))
	if _, err := g.fs.Stat("/Photos/Moved.jpg"); err != nil {
		t.Fatalf("the case-only rename: %v", err)
	}
	if _, err := g.fs.Stat("/Photos/moved.jpg"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old spelling is still there: %v", err)
	}

	// a rename onto another file's name in another case: a collision without replace, and
	// with it the other file is replaced, never doubled
	g.create("/Photos/b.txt")
	fh, err = g.sh.OpenFile(`Photos\Moved.jpg`, os.O_RDWR, 0)
	must(t, err)
	if err := fh.RenameTo(`Photos\B.txt`, false); err == nil {
		t.Fatal("a rename without replace onto b.txt as B.txt succeeded")
	}
	must(t, fh.Close())
	must(t, g.sh.Rename(`Photos\Moved.jpg`, `Photos\B.txt`))
	ents, err := g.fs.ReadDir("/Photos")
	must(t, err)
	if len(ents) != 1 {
		names := []string{}
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("/Photos holds %v after a replacing rename, want one file", names)
	}
}
