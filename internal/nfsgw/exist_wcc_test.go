package nfsgw_test

import (
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// A MKDIR or CREATE refused with NFS3ERR_EXIST carries the parent directory's attributes
// after the call (dir_wcc, RFC 1813 MKDIR3resfail and CREATE3resfail), so a client whose
// cache still says the name is free (another gateway made it a moment ago) sees the
// folder changed and looks again: mkdir -p, cp -R and the soak's copies then find the
// folder instead of failing on "File exists" with "No such file" right after.
// Invariant: I-242
func TestNFS_ExistReply_CarriesParentAttributes(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Mkdir("/d/x", 0o755); err != nil {
		t.Fatal(err)
	}
	_, fh, err := g.tgt.Lookup("/d")
	must(t, err)
	for _, tc := range []struct {
		name string
		proc uint32
	}{{"MKDIR", nfsc.NFSProc3Mkdir}, {"CREATE GUARDED", nfsc.NFSProc3Create}} {
		res, err := g.tgt.Call(&struct {
			Header rpc.Header
			Where  nfsc.Diropargs3
			How    uint32 // CREATE's createhow3 discriminant; MKDIR reads it as the sattr3's mode flag
			Attr   nfsc.Sattr3
		}{
			Header: rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: tc.proc, Cred: rpc.AuthNull, Verf: rpc.AuthNull},
			Where:  nfsc.Diropargs3{FH: fh, Filename: "x"},
			How:    1, // GUARDED; for MKDIR: mode follows
			Attr:   nfsc.Sattr3{},
		})
		must(t, err)
		status, err := xdr.ReadUint32(res)
		must(t, err)
		if status != nfsc.NFS3ErrExist {
			t.Fatalf("%s of an existing name: status %d, want NFS3ERR_EXIST", tc.name, status)
		}
		pre, err := xdr.ReadUint32(res) // pre_op_attr: attributes_follow
		must(t, err)
		if pre == 1 {
			var skip [24]byte
			must(t, xdr.Read(res, &skip))
		}
		post, err := xdr.ReadUint32(res) // post_op_attr: attributes_follow
		must(t, err)
		if post != 1 {
			t.Fatalf("%s refused with no post-op attributes of the parent: the client cannot see it changed", tc.name)
		}
	}
}
