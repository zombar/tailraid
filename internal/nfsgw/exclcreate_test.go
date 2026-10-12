package nfsgw_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"

	"github.com/zombar/tailraid/internal/meta"
)

// createHow sends CREATE with the given mode (0 UNCHECKED, 1 GUARDED) for name in the
// directory dir and returns the NFS status: the client library only sends UNCHECKED.
func (g *gateway) createHow(dir, name string, how uint32) uint32 {
	g.t.Helper()
	_, fh, err := g.tgt.Lookup(dir)
	if err != nil {
		g.t.Fatal(err)
	}
	type howArg struct {
		Mode uint32
		Attr nfsc.Sattr3
	}
	res, err := g.tgt.Call(&struct {
		Header rpc.Header
		Where  nfsc.Diropargs3
		HW     howArg
	}{
		Header: rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: nfsc.NFSProc3Create, Cred: rpc.AuthNull, Verf: rpc.AuthNull},
		Where:  nfsc.Diropargs3{FH: fh, Filename: name},
		HW:     howArg{Mode: how, Attr: nfsc.Sattr3{Mode: nfsc.SetMode{SetIt: true, Mode: 0o644}}},
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

// CREATE GUARDED is exclusive however far this gateway's FSM lags: another gateway's
// create of the name, landing between the gateway's lookup and its own create, is
// NFS3ERR_EXIST and the winner's bytes stay; a free name is created.
// Invariant: I-222
func TestNFS_CreateGuarded_RacingBackend_Exist(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	dir := g.f.InodeOf(g.f.FS(), "/d")
	winner := []byte("the other gateway's copy")
	g.f.ForestallNext(meta.OpCreate, func() {
		g.f.MustApply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{
			Parent: dir, Name: "x", Type: meta.TFile, Mode: 0o644, Now: g.f.Clk.Now().UnixNano(),
		}})
		h, err := g.fs.OpenFile("/d/x", os.O_RDWR, 0)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := h.Seek(3, io.SeekStart); err != nil { // a non-zero offset
			t.Error(err)
		}
		if _, err := h.Write(winner); err != nil {
			t.Error(err)
		}
		_ = h.Close()
	})
	if st := g.createHow("/d", "x", 1); st != uint32(nfsc.NFS3ErrExist) {
		t.Fatalf("GUARDED create of a name taken meanwhile: status %d, want NFS3ERR_EXIST", st)
	}
	h, err := g.fs.Open("/d/x")
	must(t, err)
	got, err := io.ReadAll(h)
	must(t, err)
	_ = h.Close()
	if want := append(make([]byte, 3), winner...); !bytes.Equal(got, want) {
		t.Fatalf("the winner's file is %q, want %q", got, want)
	}
	if st := g.createHow("/d", "free", 1); st != 0 {
		t.Fatalf("GUARDED create of a free name: status %d", st)
	}
}
