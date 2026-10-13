package nfsgw_test

import (
	"encoding/binary"
	"io"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// A refused RMDIR, FSSTAT, FSINFO or PATHCONF carries the body RFC 1813 gives its failure:
// RMDIR3resfail the directory's wcc_data (its attributes after the call), the others an
// obj_attributes post_op_attr. Before, they carried the status alone, and a client that
// decodes the body (Linux decodes a failed RMDIR's wcc_data) took the short reply for an
// I/O error: rmdir of a non-empty folder said "Input/output error", not "Directory not
// empty".
// Invariant: I-266
func TestNFS_FailureBodies(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/d/f", 0o644); err != nil {
		t.Fatal(err)
	}
	_, root, err := g.tgt.Lookup("/")
	must(t, err)
	call := func(proc uint32, args any) []byte {
		t.Helper()
		res, err := g.tgt.Call(&struct {
			rpc.Header
			Args any
		}{rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: proc, Cred: rpc.AuthNull, Verf: rpc.AuthNull}, args})
		must(t, err)
		b, err := io.ReadAll(res)
		must(t, err)
		return b
	}
	u32 := func(b []byte, off int) uint32 {
		if len(b) < off+4 {
			t.Fatalf("reply of %d bytes ends before offset %d", len(b), off)
		}
		return binary.BigEndian.Uint32(b[off:])
	}

	// RMDIR of a folder that is not empty: NOTEMPTY, then wcc_data with the folder's
	// attributes after the call
	b := call(nfsc.NFSProc3RmDir, nfsc.Diropargs3{FH: root, Filename: "d"})
	if st := u32(b, 0); st != nfsc.NFS3ErrNotEmpty {
		t.Fatalf("RMDIR of a non-empty folder: status %d, want NFS3ERR_NOTEMPTY", st)
	}
	off := 4
	if u32(b, off) == 1 { // pre_op_attr
		off += 4 + 24
	} else {
		off += 4
	}
	if u32(b, off) != 1 {
		t.Fatal("the refused RMDIR carries no attributes of the folder after the call")
	}

	// FSSTAT, FSINFO, PATHCONF of a handle that resolves to nothing: STALE, then a
	// post_op_attr
	stale := make([]byte, len(root))
	for i := range stale {
		stale[i] = 0xEE
	}
	for _, proc := range []uint32{18, nfsc.NFSProc3FSInfo, 20} { // FSSTAT, FSINFO, PATHCONF
		b := call(proc, stale)
		if st := u32(b, 0); st == 0 {
			t.Fatalf("proc %d of a stale handle succeeded", proc)
		}
		if len(b) < 8 {
			t.Fatalf("proc %d: a refusal of %d bytes carries no post_op_attr", proc, len(b))
		}
	}
}
