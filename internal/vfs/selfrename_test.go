package vfs_test

import (
	"io"
	"os"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
)

// A rename of a file onto its own name changes nothing (RFC 1813 §3.3.14), so an
// ephemeral file's unflushed bytes stay: the rename had taken the file for the entry it
// replaced and dropped its WAL.
// Invariant: I-238
func TestVFS_RenameOntoItself_KeepsDirtyEphemeral(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	h, err := fs.OpenFile("/._song.mp3", os.O_RDWR|os.O_CREATE, 0o644)
	must(t, err)
	_, err = h.Seek(7, io.SeekStart)
	must(t, err)
	_, err = h.Write([]byte("resource fork"))
	must(t, err)
	must(t, h.Close())
	must(t, fs.Rename("/._song.mp3", "/._song.mp3"))
	h, err = fs.Open("/._song.mp3")
	must(t, err)
	got, err := io.ReadAll(h)
	must(t, err)
	if len(got) != 7+13 || string(got[7:]) != "resource fork" {
		t.Fatalf("after a rename onto itself the file reads %q", got)
	}
}
