package smbgw_test

import (
	"encoding/binary"
	"os"
	"testing"
)

// Windows' copy engine (Explorer, many files) probes each destination before writing it:
// FILE_CREATE, end-of-file to the source's size, then FSCTL_QUERY_FILE_REGIONS. The
// gateway answers from the vfs file's valid data length: nothing written yet is one
// region of usage 0 over the whole size, as NTFS answers; a write at a non-zero offset
// makes the region before its end valid. A refusal had the engine delete every file it
// created (docs/15 §3).
// Invariant: I-227
func TestSMB_QueryFileRegions_ExplorerProbe(t *testing.T) {
	g := startGateway(t)
	const size = 5<<20 + 7
	fh, err := g.sh.OpenFile("probe.mp3", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	must(t, err)
	defer fh.Close()
	must(t, fh.Truncate(size))

	type region struct {
		off, n int64
		usage  uint32
	}
	regions := func(t *testing.T) []region {
		t.Helper()
		out, err := fh.QueryFileRegions(0, size, 1, 64)
		if err != nil {
			t.Fatalf("FSCTL_QUERY_FILE_REGIONS: %v (status %#x)", err, ntStatus(err))
		}
		if len(out) < 16 {
			t.Fatalf("%d output bytes", len(out))
		}
		n := int(binary.LittleEndian.Uint32(out[8:]))
		if len(out) != 16+24*n {
			t.Fatalf("%d output bytes for %d regions", len(out), n)
		}
		var rs []region
		for i := range n {
			r := out[16+24*i:]
			rs = append(rs, region{int64(binary.LittleEndian.Uint64(r[0:])), int64(binary.LittleEndian.Uint64(r[8:])), binary.LittleEndian.Uint32(r[16:])})
		}
		return rs
	}

	if got := regions(t); len(got) != 1 || got[0] != (region{0, size, 0}) {
		t.Fatalf("created and sized: regions %+v, want one of usage 0 over [0, %d)", got, size)
	}
	const off, n = 1<<20 + 13, 4096
	_, err = fh.WriteAt(make([]byte, n), off)
	must(t, err)
	_, err = fh.WriteAt([]byte("not zeros"), off)
	must(t, err)
	got := regions(t)
	if len(got) != 2 || got[0] != (region{0, off + n, 1}) || got[1] != (region{off + n, size - off - n, 0}) {
		t.Fatalf("after a write at %d: regions %+v", off, got)
	}
	if fi, err := g.sh.Stat("probe.mp3"); err != nil || fi.Size() != size {
		t.Fatalf("the probed file: %v, %v", fi, err)
	}
}
