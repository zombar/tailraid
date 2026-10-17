package vfs_test

import (
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// copyRig is a filesystem and a flusher over one fabric whose proposals are counted.
func copyRig(t *testing.T) (*testharness.Fabric, *vfs.FS, *vfs.Flusher, *vfs.Proposals) {
	t.Helper()
	f := testharness.NewFabric(t, 3, 2, 1)
	p := vfs.CountProposals(f)
	fs := vfs.New(f.FSM, p, f.Ov, f.Reader(), f.Clk, 501, 20)
	fl := vfs.NewFlusher(f.Ov, f.FSM, p, f, f, f.Reader(), f.Clk, vfs.DefaultConfig(), nil)
	return f, fs, fl, p
}

// cpp is what `cp -p` does to one file: create it, write it (at a non-zero offset), set
// its mode, owner and the source's times.
func cpp(t *testing.T, fs *vfs.FS, name string, mtime time.Time) {
	t.Helper()
	fh, err := fs.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	must(t, err)
	_, err = fh.(interface {
		WriteAt([]byte, int64) (int, error)
	}).WriteAt(testharness.Pattern(3000, 4), 5)
	must(t, err)
	must(t, fh.Close())
	must(t, fs.Chmod(name, 0o640))
	must(t, fs.Lchown(name, 777, 778))
	must(t, fs.Chtimes(name, mtime, mtime))
}

// A copy of a file lands whole: its create is the one proposal before the flush, the
// gateway shows the mode, owner and times at once, and the flush's one Commit lands them
// with the data; while a member reads codec 15 a SetAttr follows the Commit instead.
// Invariant: I-401, I-403
func TestVFS_CopyLandsWhole(t *testing.T) {
	src := time.Date(2019, 3, 14, 15, 9, 26, 0, time.UTC)
	for _, old := range []bool{false, true} {
		name := map[bool]string{false: "codec 16", true: "a member reads codec 15"}[old]
		t.Run(name, func(t *testing.T) {
			f, fs, fl, p := copyRig(t)
			if old {
				fl.MinCodec = func() uint8 { return meta.CodecHeal }
			}
			cpp(t, fs, "/f", src)
			if got := p.Counts(); got["create"] != 1 || got["setattr"] != 0 || len(got) != 1 {
				t.Fatalf("before the flush: %v, want the create alone", got)
			}
			if fs.AttrsHeld() != 3 {
				t.Fatalf("held %d SETATTRs, want 3", fs.AttrsHeld())
			}
			fi, err := fs.Stat("/f")
			must(t, err)
			if fi.Mode().Perm() != 0o640 || !fi.ModTime().Equal(src) {
				t.Fatalf("this gateway shows mode %v mtime %v at once, want 0640 and %v", fi.Mode(), fi.ModTime(), src)
			}
			must(t, fl.FlushAll(ctx))
			got := p.Counts()
			wantSet := map[bool]uint64{false: 0, true: 1}[old]
			if got["commit"] != 1 || got["setattr"] != wantSet {
				t.Fatalf("after the flush: %v, want one commit and %d setattr", got, wantSet)
			}
			in, err := f.FSM.Resolve([]string{"f"})
			must(t, err)
			if in.Mode&0o777 != 0o640 || in.Uid != 777 || in.Gid != 778 || in.Mtime != src.UnixNano() || in.Size != 3005 {
				t.Fatalf("committed inode %+v", in)
			}
		})
	}
}

// A SETATTR that changes nothing on a clean file proposes nothing; one that changes
// something is proposed, as is any SETATTR of a directory.
// Invariant: I-402
func TestVFS_SetAttr_NoChangeNoProposal(t *testing.T) {
	_, fs, fl, p := copyRig(t)
	cpp(t, fs, "/f", time.Unix(1_600_000_000, 0))
	must(t, fl.FlushAll(ctx))
	before := p.Counts()["setattr"]
	must(t, fs.Chmod("/f", 0o640))                                                      // already
	must(t, fs.Lchown("/f", 777, 778))                                                  // already
	must(t, fs.Chtimes("/f", time.Unix(1_600_000_000, 0), time.Unix(1_600_000_000, 0))) // already
	if n := p.Counts()["setattr"]; n != before {
		t.Fatalf("no-op SETATTRs proposed %d", n-before)
	}
	must(t, fs.Chmod("/f", 0o600))
	if n := p.Counts()["setattr"]; n != before+1 {
		t.Fatalf("a real change on a clean file: %d proposals, want 1", n-before)
	}
	must(t, fs.MkdirAll("/d", 0o755))
	must(t, fs.Chmod("/d", 0o700))
	if n := p.Counts()["setattr"]; n != before+2 {
		t.Fatalf("a directory's SETATTR: %d proposals, want 2", n-before)
	}
}
