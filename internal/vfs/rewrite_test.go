package vfs_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

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
