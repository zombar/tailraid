package vfs_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// A copy that replaces a file is one rewrite: its first flush starts it and the later
// ones carry its id. When another client's rewrite of the file starts after it, the
// copy's next flush is superseded: its dirty data and WAL are dropped and counted, the
// rest of that copy is refused the same way rather than mixed into the other, and once
// the claim has lapsed an in-place edit is a plain write again.
// Invariant: I-223
func TestFlusher_Superseded_DropsDirty(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fl := f.Flusher()
	cs := int(testharness.ChunkSize)
	write := func(h interface {
		io.Seeker
		io.Writer
	}, off int, b []byte,
	) {
		t.Helper()
		if _, err := h.Seek(int64(off), io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	h, err := fs.OpenFile("/f", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	must(t, err)
	write(h, 5, testharness.Pattern(cs, 1)) // a non-zero offset
	must(t, fl.FlushAll(ctx))
	ino := f.InodeOf(fs, "/f")
	mine := f.Inode(ino).RewriteAt
	if mine == 0 {
		t.Fatal("the copy's first flush started no rewrite")
	}

	// another gateway's copy of the file starts its rewrite now: one chunk of its own
	ofs, ofl := f.OtherGateway()
	theirs := testharness.Pattern(cs, 9)
	o, err := ofs.OpenFile("/f", os.O_RDWR|os.O_TRUNC, 0)
	must(t, err)
	if _, err := o.Write(theirs); err != nil {
		t.Fatal(err)
	}
	must(t, o.Close())
	must(t, ofl.FlushAll(ctx))
	if f.Inode(ino).RewriteAt == mine {
		t.Fatal("the other rewrite did not take the file")
	}

	write(h, cs+5, testharness.Pattern(cs, 2)) // the copy goes on
	must(t, fl.FlushAll(ctx))
	if got := fl.RewritesSuperseded(); got != 1 {
		t.Fatalf("superseded %d, want 1", got)
	}
	if d := f.Ov.Dirty(); len(d) != 0 {
		t.Fatalf("the superseded copy is still dirty: %+v", d)
	}
	if wals, _ := filepath.Glob(filepath.Join(f.Staging(), "*.wal")); len(wals) != 0 {
		t.Fatalf("the superseded copy left WALs: %v", wals)
	}
	write(h, 2*cs+5, testharness.Pattern(cs, 3)) // and on: refused the same way
	must(t, h.Close())
	must(t, fl.FlushAll(ctx))
	if got := fl.RewritesSuperseded(); got != 2 {
		t.Fatalf("superseded %d after the copy's last write, want 2", got)
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, theirs) {
		t.Fatalf("the file is not the other copy whole: %d bytes, differs at %d", len(got), testharness.FirstDiff(got, theirs))
	}

	// once the claim lapses, an in-place edit through this gateway lands
	f.Clk.Advance(vfs.RewriteHold + 1)
	e, err := fs.OpenFile("/f", os.O_RDWR, 0)
	must(t, err)
	write(e, 7, []byte("edit"))
	must(t, e.Close())
	must(t, fl.FlushAll(ctx))
	want := append([]byte(nil), theirs...)
	copy(want[7:], "edit")
	if got := f.ReadFile(ino); !bytes.Equal(got, want) {
		t.Fatalf("the in-place edit after the claim lapsed: differs at %d", testharness.FirstDiff(got, want))
	}
	if got := fl.RewritesSuperseded(); got != 2 {
		t.Fatalf("the edit was superseded: %d", got)
	}
}

// Until every live member reads codec 9 the flusher sends no rewrite: a replacing copy
// commits as plain writes, as before (docs/04 §6).
// Invariant: I-223
func TestFlusher_Rewrite_GatedOnMinCodec(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fl := f.Flusher()
	fl.MinCodec = func() uint8 { return meta.CodecRewrite - 1 }
	h, err := fs.OpenFile("/f", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	must(t, err)
	if _, err := h.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	must(t, h.Close())
	must(t, fl.FlushAll(ctx))
	if in := f.Inode(f.InodeOf(fs, "/f")); in.RewriteAt != 0 || in.Size != 5 {
		t.Fatalf("a flush before codec 9 started a rewrite: %+v", in)
	}
}

// A replacing copy whose first flushes committed as plain writes (the gate was closed,
// or the leader refused the rewrite and the same chunks went plain) has no rewrite left
// to start: a later flush, with the gate open, must not truncate what they committed.
// Regression: I-223
func TestFlusher_Rewrite_PlainFlushConsumesClaim(t *testing.T) {
	cs := int(testharness.ChunkSize)
	for _, tc := range []struct {
		name  string
		block func(f *testharness.Fabric, fl *vfs.Flusher, open func())
	}{
		{"gate closed", func(_ *testharness.Fabric, fl *vfs.Flusher, open func()) {
			fl.MinCodec = func() uint8 { return meta.CodecRewrite - 1 }
			open()
		}},
		{"leader refuses", func(f *testharness.Fabric, _ *vfs.Flusher, open func()) {
			f.SetLeaderCodec(meta.CodecRewrite - 1)
			open()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 3, 2, 1)
			fs := f.FS()
			fl := f.Flusher()
			first, second := testharness.Pattern(cs, 1), testharness.Pattern(cs, 2)
			var h billy.File
			tc.block(f, fl, func() {
				o, err := fs.OpenFile("/f", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
				must(t, err)
				h = o
				writeAt(t, h, 5, first) // a non-zero offset
				must(t, fl.FlushAll(ctx))
			})
			// every member reads codec 9 now; the copy goes on
			fl.MinCodec = nil
			f.SetLeaderCodec(0)
			writeAt(t, h, int64(cs+5), second)
			must(t, h.Close())
			must(t, fl.FlushAll(ctx))
			want := make([]byte, 5, 5+2*cs)
			want = append(append(want, first...), second...)
			if got := f.ReadFile(f.InodeOf(fs, "/f")); !bytes.Equal(got, want) {
				t.Fatalf("the copy's first flush was lost: %d bytes, differs at %d", len(got), testharness.FirstDiff(got, want))
			}
		})
	}
}

// A truncate to 0 is a new rewrite even while an older one of this gateway holds the
// inode: a second copy through the same gateway starts its own, so a rewrite another
// gateway started in between does not supersede it (docs/04 §6).
// Regression: I-223
func TestFlusher_Rewrite_TruncateStartsAnother(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	fl := f.Flusher()
	cs := int(testharness.ChunkSize)
	h, err := fs.OpenFile("/f", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	must(t, err)
	writeAt(t, h, 5, testharness.Pattern(cs, 1))
	must(t, h.Close())
	must(t, fl.FlushAll(ctx))

	ofs, ofl := f.OtherGateway() // another copy starts and lands
	o, err := ofs.OpenFile("/f", os.O_RDWR|os.O_TRUNC, 0)
	must(t, err)
	if _, err := o.Write(testharness.Pattern(cs, 9)); err != nil {
		t.Fatal(err)
	}
	must(t, o.Close())
	must(t, ofl.FlushAll(ctx))

	// a third copy through this gateway, truncating by SETATTR as NFS does
	h, err = fs.OpenFile("/f", os.O_RDWR, 0)
	must(t, err)
	must(t, h.Truncate(0))
	mine := testharness.Pattern(cs, 3)
	writeAt(t, h, 0, mine)
	must(t, h.Close())
	must(t, fl.FlushAll(ctx))
	if got := fl.RewritesSuperseded(); got != 0 {
		t.Fatalf("the newest copy was superseded %d times", got)
	}
	if got := f.ReadFile(f.InodeOf(fs, "/f")); !bytes.Equal(got, mine) {
		t.Fatalf("the newest copy did not win: differs at %d", testharness.FirstDiff(got, mine))
	}
}

// A claim whose copy is still dirty does not lapse, however long its flush retries: the
// rest of the copy stays one rewrite (docs/04 §6).
// Regression: I-223
func TestOverlay_PruneRewrites_KeepsDirty(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	h, err := fs.OpenFile("/f", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	must(t, err)
	writeAt(t, h, 5, []byte("dirty"))
	must(t, h.Close())
	ino := f.InodeOf(fs, "/f")
	f.Clk.Advance(vfs.RewriteHold + 1)
	f.Ov.PruneRewrites()
	if start, _ := f.Ov.Rewrite(ino); !start {
		t.Fatal("the claim of a dirty file lapsed")
	}
	must(t, f.Flusher().FlushAll(ctx))
	f.Clk.Advance(vfs.RewriteHold + 1)
	f.Ov.PruneRewrites()
	if start, id := f.Ov.Rewrite(ino); start || id != 0 {
		t.Fatalf("a clean file's lapsed claim stayed: start %v id %d", start, id)
	}
}

func writeAt(t *testing.T, h billy.File, off int64, b []byte) {
	t.Helper()
	if _, err := h.Seek(off, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Write(b); err != nil {
		t.Fatal(err)
	}
}

// The roms library kind is codec 10: while a live member reads an older codec the
// gateway refuses it with ErrCodec before proposing (an older leader would refuse the
// entry as malformed), the other kinds still set, and roms sets once every member reads
// codec 10.
// Invariant: I-277
func TestFS_SetFlags_RomsWaitsForCodec10(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	fs := f.FS()
	must(t, fs.MkdirAll("/roms", 0o755))
	fs.MinCodec = func() uint8 { return meta.CodecRoms - 1 }
	if err := fs.SetFlags("/roms", meta.WithLibrary(0, meta.LibraryRoms)); !errors.Is(err, meta.ErrCodec) {
		t.Fatalf("roms with an old member: %v, want ErrCodec", err)
	}
	must(t, fs.SetFlags("/roms", meta.WithLibrary(0, meta.LibraryMusic)))
	fs.MinCodec = func() uint8 { return meta.CodecRoms }
	must(t, fs.SetFlags("/roms", meta.WithLibrary(0, meta.LibraryRoms)))
	if k := f.Inode(f.InodeOf(fs, "/roms")).Library(); k != meta.LibraryRoms {
		t.Fatalf("kind %s", k)
	}
}
