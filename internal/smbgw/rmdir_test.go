package smbgw_test

import (
	"errors"
	"os"
	"testing"
)

// Deleting a folder that still holds entries is STATUS_DIRECTORY_NOT_EMPTY when the
// client marks it (MS-FSA FileDispositionInformation), and the folder stays; rd, .NET's
// Directory.Delete and Python's os.rmdir had been told it went. Emptied, it goes.
// Invariant: I-236
func TestSMB_DeleteNonEmptyFolder_NotEmpty(t *testing.T) {
	const statusDirectoryNotEmpty = 0xC0000101
	g := startGateway(t)
	must(t, g.sh.Mkdir("d", 0o755))
	g.create("/d/f")
	err := g.sh.Remove("d")
	if err == nil || ntStatus(err) != statusDirectoryNotEmpty {
		t.Fatalf("delete of a folder with a file: %v (status %#x), want STATUS_DIRECTORY_NOT_EMPTY", err, ntStatus(err))
	}
	if _, err := g.fs.Stat("/d/f"); err != nil {
		t.Fatalf("the file went: %v", err)
	}
	must(t, g.sh.Remove(`d\f`))
	must(t, g.sh.Remove("d"))
	if _, err := g.fs.Stat("/d"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the emptied folder is still there: %v", err)
	}
}
