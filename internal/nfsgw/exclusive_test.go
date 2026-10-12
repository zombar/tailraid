package nfsgw_test

import (
	"io"
	"os"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// createExclusive sends CREATE EXCLUSIVE with verf for name in dir and returns the status.
func (g *gateway) createExclusive(dir, name string, verf [8]byte) uint32 {
	g.t.Helper()
	_, fh, err := g.tgt.Lookup(dir)
	if err != nil {
		g.t.Fatal(err)
	}
	res, err := g.tgt.Call(&struct {
		Header rpc.Header
		Where  nfsc.Diropargs3
		Mode   uint32
		Verf   [8]byte
	}{
		Header: rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: nfsc.NFSProc3Create, Cred: rpc.AuthNull, Verf: rpc.AuthNull},
		Where:  nfsc.Diropargs3{FH: fh, Filename: name},
		Mode:   2, // EXCLUSIVE
		Verf:   verf,
	})
	if err != nil {
		g.t.Fatal(err)
	}
	status, err := xdr.ReadUint32(res)
	if err != nil {
		g.t.Fatal(err)
	}
	return status
}

// CREATE EXCLUSIVE (RFC 1813 §3.3.8), which macOS and Linux send for O_EXCL: a free name
// is created and keeps the verifier, so a retransmission of the same request succeeds; a
// request with another verifier, or for a name that already holds a file, is
// NFS3ERR_EXIST and the file is untouched. Before, the server answered NFS3ERR_NOTSUPP and
// macOS fell back to UNCHECKED, which truncated an existing file.
// Invariant: I-231
func TestNFS_CreateExclusive_Verifier(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	mine := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if st := g.createExclusive("/d", "lock", mine); st != 0 {
		t.Fatalf("EXCLUSIVE create of a free name: status %d", st)
	}
	if st := g.createExclusive("/d", "lock", mine); st != 0 {
		t.Fatalf("a retransmission with the same verifier: status %d, want OK", st)
	}
	if st := g.createExclusive("/d", "lock", [8]byte{9}); st != uint32(nfsc.NFS3ErrExist) {
		t.Fatalf("another client's verifier: status %d, want NFS3ERR_EXIST", st)
	}

	if st := g.createExclusive("/", "d", mine); st != uint32(nfsc.NFS3ErrExist) {
		t.Fatalf("EXCLUSIVE create of a directory's name: status %d, want NFS3ERR_EXIST", st)
	}

	// a file another device wrote: an exclusive create is EXIST and the bytes stay
	h, err := g.fs.OpenFile("/d/theirs", os.O_RDWR|os.O_CREATE, 0o644)
	must(t, err)
	_, err = h.Seek(5, io.SeekStart)
	must(t, err)
	_, err = h.Write([]byte("their data"))
	must(t, err)
	must(t, h.Close())
	if st := g.createExclusive("/d", "theirs", mine); st != uint32(nfsc.NFS3ErrExist) {
		t.Fatalf("EXCLUSIVE create over an existing file: status %d, want NFS3ERR_EXIST", st)
	}
	fi, err := g.fs.Stat("/d/theirs")
	must(t, err)
	if fi.Size() != 15 {
		t.Fatalf("the existing file is %d bytes after an exclusive create, want 15", fi.Size())
	}
}
