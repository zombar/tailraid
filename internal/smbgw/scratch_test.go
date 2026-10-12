package smbgw_test

import (
	"errors"
	"os"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// Windows' copy engine opens a batch with a pass that creates every destination, sizes
// it, marks it delete-on-close and closes it without a write; the copy proper follows in
// a second pass. Such a file never held a byte, so its delete leaves nothing in the
// trash: the name goes and the inode is purged. A file the handle wrote into, at any
// offset, goes to the trash as before (I-220), and so does one under a protected folder.
// Invariant: I-229
func TestSMB_ScratchCreate_NotTrashed(t *testing.T) {
	const size = 5<<20 + 7
	cases := []struct {
		name      string
		write     bool // write one byte at a non-zero offset before the delete
		protected bool // the file is created in a folder with the protected flag
		trashed   int
	}{
		{"created, sized, probed, deleted", false, false, 0},
		{"created, written, deleted", true, false, 1},
		{"created and deleted under a protected folder", false, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := startGateway(t)
			dir := ""
			if tc.protected {
				must(t, g.sh.Mkdir("p", 0o755))
				g.f.MustApply(meta.Command{Op: meta.OpSetAttr, SetAttr: &meta.SetAttr{Ino: g.f.InodeOf(g.fs, "/p"), Mask: meta.AttrFlags, Flags: meta.FlagProtected, Now: 1}})
				dir = "p/"
			}
			before := g.f.FSM.Stats().TrashInodes
			fh, err := g.sh.OpenFile(dir+"scratch.mp3", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
			must(t, err)
			must(t, fh.Truncate(size))
			if _, err := fh.QueryFileRegions(0, size, 1, 64); err != nil {
				t.Fatalf("FSCTL_QUERY_FILE_REGIONS: %v", err)
			}
			if tc.write {
				_, err := fh.WriteAt([]byte{1}, 7)
				must(t, err)
			}
			must(t, fh.SetDeleteOnClose())
			must(t, fh.Close())
			if _, err := g.sh.Stat(dir + "scratch.mp3"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the deleted name: %v, want not-exist", err)
			}
			if got := g.f.FSM.Stats().TrashInodes - before; got != tc.trashed {
				t.Fatalf("trash grew by %d, want %d", got, tc.trashed)
			}
			// the copy proper creates the name again
			fh, err = g.sh.OpenFile(dir+"scratch.mp3", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
			must(t, err)
			must(t, fh.Close())
		})
	}
}
