package nfsgw_test

import (
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// listReply walks a READDIR or READDIRPLUS reply: its entry count, whether it says eof,
// and its size in bytes after the status (the READDIR3resok/READDIRPLUS3resok the
// client's count bounds).
func listReply(t *testing.T, res io.Reader, plus bool) (entries int, eof bool, size int) {
	t.Helper()
	all, err := io.ReadAll(res)
	must(t, err)
	b := all
	u32 := func() uint32 {
		if len(b) < 4 {
			t.Fatalf("reply cut short")
		}
		v := binary.BigEndian.Uint32(b)
		b = b[4:]
		return v
	}
	skip := func(n int) { b = b[(n+3)&^3:] }
	if st := u32(); st != 0 {
		t.Fatalf("status %d", st)
	}
	if u32() == 1 { // the directory's post_op_attr
		skip(84)
	}
	skip(8) // cookieverf
	for u32() == 1 {
		entries++
		skip(8)          // fileid
		skip(int(u32())) // name
		skip(8)          // cookie
		if plus {
			if u32() == 1 {
				skip(84)
			}
			if u32() == 1 {
				skip(int(u32()))
			}
		}
	}
	eof = u32() == 1
	return entries, eof, len(all) - 4
}

// A listing reply is filled by the entries' real XDR sizes: as many entries as the
// client's count (READDIR) or dircount and maxcount (READDIRPLUS) admit, never a byte
// over, and FSINFO prefers 1 MiB listings. Before, every entry was charged 512 bytes
// whatever its size, so a READDIR of short names came back with about a sixteenth of the
// entries its count had room for and a READDIRPLUS with about a third, and FSINFO
// preferred 8 KiB: a large folder took many times the round trips.
// Invariant: I-248
func TestNFS_ListingFillsTheClientsCount(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/big", 0o755); err != nil {
		t.Fatal(err)
	}
	const files = 300
	for i := range files {
		if _, err := g.tgt.Create(fmt.Sprintf("/big/f%04d", i), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, fh, err := g.tgt.Lookup("/big")
	must(t, err)
	hdr := func(proc uint32) rpc.Header {
		return rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: proc, Cred: rpc.AuthNull, Verf: rpc.AuthNull}
	}

	// READDIR, count 8 KiB: an entry of a 5-byte name is 32 bytes on the wire
	const count = 8192
	res, err := g.tgt.Call(&struct {
		rpc.Header
		FH     []byte
		Cookie uint64
		Verf   uint64
		Count  uint32
	}{hdr(16), fh, 0, 0, count})
	must(t, err)
	n, eof, size := listReply(t, res, false)
	if size > count {
		t.Fatalf("READDIR reply of %d bytes against a count of %d", size, count)
	}
	if eof || n < 200 {
		t.Fatalf("READDIR of %d files with count %d: %d entries (eof %v), want at least 200", files, count, n, eof)
	}

	// READDIRPLUS, maxcount 32 KiB: an entry with attributes and a handle is about 150
	const maxcount = 32768
	res, err = g.tgt.Call(&struct {
		rpc.Header
		FH       []byte
		Cookie   uint64
		Verf     uint64
		DirCount uint32
		MaxCount uint32
	}{hdr(nfsc.NFSProc3ReadDirPlus), fh, 0, 0, maxcount, maxcount})
	must(t, err)
	n, eof, size = listReply(t, res, true)
	if size > maxcount {
		t.Fatalf("READDIRPLUS reply of %d bytes against a maxcount of %d", size, maxcount)
	}
	if eof || n < 150 {
		t.Fatalf("READDIRPLUS of %d files with maxcount %d: %d entries (eof %v), want at least 150", files, maxcount, n, eof)
	}

	info, err := g.tgt.FSInfo()
	must(t, err)
	if info.DTPref != 1<<20 {
		t.Errorf("FSINFO dtpref %d, want 1 MiB", info.DTPref)
	}
}
