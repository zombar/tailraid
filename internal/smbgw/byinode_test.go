package smbgw_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	smb2 "github.com/cloudsoda/go-smb2"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/testharness"
)

// Two clients on one gateway: one holds the file open while the other renames it and
// puts a new, longer file under the old name. The first client's handle answers for its
// own file at the new name (QUERY_INFO size, reads), never for the new one.
// Invariant: I-224, I-265
func TestSMB_TwoSessions_RenameMovesOtherHandle(t *testing.T) {
	g := startGateway(t)
	orig := testharness.Pattern(3000, 4)
	// held sharing delete, as Explorer holds a file: another session may rename it (I-265)
	f1, err := g.sh.OpenFileShare("a.bin", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644, smb2.ShareRead|smb2.ShareWrite|smb2.ShareDelete)
	must(t, err)
	if _, err := f1.WriteAt(orig, 0); err != nil {
		t.Fatal(err)
	}
	sess := dial(t, g.addr, smbgw.User, password)
	sh2, err := sess.Mount(smbgw.ShareName)
	must(t, err)
	t.Cleanup(func() { _ = sh2.Umount(); _ = sess.Logoff() })
	must(t, sh2.Rename("a.bin", "moved.bin"))
	f2, err := sh2.Create("a.bin")
	must(t, err)
	if _, err := f2.WriteAt(testharness.Pattern(9000, 5), 0); err != nil {
		t.Fatal(err)
	}
	must(t, f2.Close())

	st, err := f1.Stat()
	must(t, err)
	if st.Size() != int64(len(orig)) {
		t.Fatalf("the first client's handle reports %d bytes, want its own file's %d (the new file has 9000)", st.Size(), len(orig))
	}
	got := make([]byte, len(orig))
	if _, err := f1.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("the first client's handle reads another file's bytes")
	}
	must(t, f1.Close())
}

// A failed copy's own cleanup under a protected folder removes the file the handle
// created or nothing: renamed away and replaced by another client's file, the new file
// stays and the renamed one goes; with a member before codec 9 the gateway's own lookup
// decides the same.
// Invariant: I-225, I-220
func TestSMB_OwnCleanup_ByInode_UnderProtection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codec uint8
	}{{"codec 9", 0}, {"a member on codec 8", 8}} {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 3, 2, 1)
			fs := f.FS()
			if tc.codec != 0 {
				v := tc.codec
				fs.MinCodec = func() uint8 { return v }
				f.SetLeaderCodec(v)
			}
			a := smbgw.New(fs, f.FSM)
			must(t, a.MkDir("/p", 0o755))
			must(t, a.WriteFile("/p/copy.bin", nil, 0o644)) // this handle's CREATE
			mine := f.InodeOf(fs, "/p/copy.bin")
			f.MustApply(meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: f.InodeOf(fs, "/p"), Mask: meta.AttrFlags, Flags: meta.FlagProtected, Now: 1}})
			// another client renames it within the folder and copies a file in under its name
			must(t, fs.Rename("/p/copy.bin", "/p/kept.bin"))
			must(t, a.WriteFile("/p/copy.bin", []byte("theirs"), 0o644))
			theirs := f.InodeOf(fs, "/p/copy.bin")

			if err := a.DeleteFileIno("/p/copy.bin", uint64(mine), true); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("delete of the old name: %v, want not-exist (another file holds it)", err)
			}
			if f.InodeOf(fs, "/p/copy.bin") != theirs {
				t.Fatal("the other client's file went")
			}
			must(t, a.DeleteFileIno("/p/kept.bin", uint64(mine), true))
			if _, err := a.Stat("/p/kept.bin"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the handle's own file is still there: %v", err)
			}
		})
	}
}
