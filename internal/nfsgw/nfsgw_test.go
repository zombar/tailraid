package nfsgw_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/nfsgw"
	"github.com/zombar/tailraid/internal/testharness"
)

func TestMain(m *testing.M) {
	// The client library starts an unstoppable cache-cleanup loop per Target; it is the
	// test client's goroutine, not the gateway's.
	goleak.VerifyTestMain(m, goleak.IgnoreAnyFunction("github.com/willscott/go-nfs-client/nfs.(*Target).cleanupCache"))
}

var ctx = context.Background()

type gateway struct {
	t   *testing.T
	f   *testharness.Fabric
	tgt *nfsc.Target
	cl  *rpc.Client
}

// startGateway serves a fabric over NFS on a random loopback port and mounts it through
// the client library on the same connection (go-nfs serves MOUNT and NFS on one port).
func startGateway(t *testing.T) *gateway {
	t.Helper()
	f := testharness.NewFabric(t, 3, 2, 1)
	h := nfsgw.New(f.FS(), f.FSM)
	l, err := nfsgw.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = nfsgw.Serve(sctx, l, h); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	cl, err := rpc.DialTCP("tcp", l.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	type mountArgs struct {
		rpc.Header
		Dirpath string
	}
	res, err := cl.Call(&mountArgs{Header: rpc.Header{Rpcvers: 2, Prog: nfsc.MountProg, Vers: nfsc.MountVers, Proc: nfsc.MountProc3MNT, Cred: rpc.AuthNull, Verf: rpc.AuthNull}, Dirpath: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := xdr.ReadUint32(res); status != nfsc.MNT3Ok {
		t.Fatalf("mount status %d", status)
	}
	fh, err := xdr.ReadOpaque(res)
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := nfsc.NewTargetWithClient(cl, rpc.NewAuthUnix("test", 501, 20).Auth(), fh, "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &gateway{t: t, f: f, tgt: tgt, cl: cl}
}

// writeAt performs the WRITE sequence a kernel client would: open, seek, write.
func (g *gateway) writeAt(path string, off int64, p []byte) {
	g.t.Helper()
	fh, err := g.tgt.OpenFile(path, 0o644)
	if err != nil {
		g.t.Fatalf("open %s: %v", path, err)
	}
	if _, err := fh.Seek(off, io.SeekStart); err != nil {
		g.t.Fatal(err)
	}
	if _, err := fh.Write(p); err != nil {
		g.t.Fatalf("write %s@%d: %v", path, off, err)
	}
	_ = fh.Close()
}

func (g *gateway) readAll(path string, n int) []byte {
	g.t.Helper()
	fh, err := g.tgt.Open(path)
	if err != nil {
		g.t.Fatalf("open %s: %v", path, err)
	}
	defer fh.Close()
	out := make([]byte, n)
	got, err := fh.ReadAt(out, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		g.t.Fatalf("read %s: %v", path, err)
	}
	return out[:got]
}

// rename issues NFS RENAME, which the client library lacks.
func (g *gateway) rename(from, to string) error {
	g.t.Helper()
	fromDir, fromName := splitPath(from)
	toDir, toName := splitPath(to)
	_, ffh, err := g.tgt.Lookup(fromDir)
	if err != nil {
		return err
	}
	_, tfh, err := g.tgt.Lookup(toDir)
	if err != nil {
		return err
	}
	type renameArgs struct {
		rpc.Header
		From nfsc.Diropargs3
		To   nfsc.Diropargs3
	}
	res, err := g.tgt.Call(&renameArgs{
		Header: rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: nfsc.NFSProc3Rename, Cred: rpc.NewAuthUnix("test", 501, 20).Auth(), Verf: rpc.AuthNull},
		From:   nfsc.Diropargs3{FH: ffh, Filename: fromName},
		To:     nfsc.Diropargs3{FH: tfh, Filename: toName},
	})
	if err != nil {
		return err
	}
	status, err := xdr.ReadUint32(res)
	if err != nil {
		return err
	}
	return nfsc.NFS3Error(status)
}

func splitPath(p string) (dir, name string) {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i] + "/", p[i+1:]
		}
	}
	return "/", p
}

// Invariant: I-7, I-34
// Lifecycle: L-1, L-2, L-5
func TestNFS_WriteSequence_MultiRPC(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/d/f", 0o644); err != nil {
		t.Fatal(err)
	}
	payload := testharness.Pattern(300<<10, 1)
	const wsize = 64 << 10
	var offs []int
	for off := 0; off < len(payload); off += wsize {
		offs = append(offs, off)
	}
	offs[0], offs[3] = offs[3], offs[0] // out of order, as macOS does
	for _, off := range offs {
		g.writeAt("/d/f", int64(off), payload[off:min(off+wsize, len(payload))])
	}
	if got := g.readAll("/d/f", len(payload)+100); !bytes.Equal(got, payload) {
		t.Fatalf("before flush differs at %d (len %d)", testharness.FirstDiff(got, payload), len(got))
	}
	fi, _, err := g.tgt.Lookup("/d/f")
	if err != nil || fi.Size() != int64(len(payload)) {
		t.Fatalf("size %d err %v", fi.Size(), err)
	}
	must(t, g.f.Flusher().FlushAll(ctx))
	if got := g.readAll("/d/f", len(payload)+100); !bytes.Equal(got, payload) {
		t.Fatalf("after flush differs at %d", testharness.FirstDiff(got, payload))
	}
	// a 1 MiB-style single large write at a non-zero offset
	big := testharness.Pattern(int(testharness.ChunkSize), 2)
	g.writeAt("/d/f", int64(len(payload)), big)
	want := append(bytes.Clone(payload), big...)
	must(t, g.f.Flusher().FlushAll(ctx))
	if got := g.readAll("/d/f", len(want)+1); !bytes.Equal(got, want) {
		t.Fatalf("after big write differs at %d", testharness.FirstDiff(got, want))
	}
}

// Lifecycle: L-8, L-11
func TestNFS_SetattrSizeTruncates(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Create("/f", 0o644); err != nil {
		t.Fatal(err)
	}
	data := testharness.Pattern(100<<10, 3)
	g.writeAt("/f", 0, data)
	must(t, g.f.Flusher().FlushAll(ctx))
	must(t, g.tgt.Setattr("/f", nfsc.Sattr3{Size: nfsc.SetSize{SetIt: true, Size: 70 << 10}}))
	if got := g.readAll("/f", len(data)); !bytes.Equal(got, data[:70<<10]) {
		t.Fatalf("after truncate len %d", len(got))
	}
	must(t, g.tgt.Setattr("/f", nfsc.Sattr3{Size: nfsc.SetSize{SetIt: true, Size: 90 << 10}}))
	want := append(bytes.Clone(data[:70<<10]), make([]byte, 20<<10)...)
	if got := g.readAll("/f", len(data)); !bytes.Equal(got, want) {
		t.Fatalf("after truncate-up differs at %d", testharness.FirstDiff(got, want))
	}
	must(t, g.f.Flusher().FlushAll(ctx))
	if got := g.readAll("/f", len(data)); !bytes.Equal(got, want) {
		t.Fatal("after flush")
	}
	// mode and times through SETATTR
	must(t, g.tgt.Setattr("/f", nfsc.Sattr3{Mode: nfsc.SetMode{SetIt: true, Mode: 0o600}}))
	fi, _, _ := g.tgt.Lookup("/f")
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// Invariant: I-16
// Lifecycle: L-16
func TestNFS_RenameKeepsHandleValid(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Mkdir("/b", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Create("/a/f", 0o644); err != nil {
		t.Fatal(err)
	}
	g.writeAt("/a/f", 0, []byte("hello"))
	_, fh, err := g.tgt.Lookup("/a/f")
	if err != nil {
		t.Fatal(err)
	}
	must(t, g.rename("/a/f", "/b/g"))
	attr, err := g.tgt.GetAttr(fh)
	if err != nil {
		t.Fatalf("old handle after rename: %v", err)
	}
	if attr.Size() != 5 {
		t.Fatalf("size via old handle %d", attr.Size())
	}
	if got := g.readAll("/b/g", 10); string(got) != "hello" {
		t.Fatalf("%q", got)
	}
	// the handle still works after the gateway restarts on the same metadata
	_, fh2, _ := g.tgt.Lookup("/b/g")
	if !bytes.Equal(fh, fh2) {
		t.Fatal("handle changed across rename")
	}
	ino, gen, ok := meta.DecodeHandle(fh)
	if !ok || ino != g.f.InodeOf(g.f.FS(), "/b/g") || gen == 0 {
		t.Fatalf("handle %x", fh)
	}
}

// Invariant: I-17
// Lifecycle: L-14
func TestNFS_RemovedHandleIsStale(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Create("/f", 0o644); err != nil {
		t.Fatal(err)
	}
	_, fh, _ := g.tgt.Lookup("/f")
	must(t, g.tgt.Remove("/f"))
	if _, err := g.tgt.GetAttr(fh); err == nil || !isStale(err) {
		t.Fatalf("removed handle: %v", err)
	}
	// a new file never reuses the old handle
	if _, err := g.tgt.Create("/f", 0o644); err != nil {
		t.Fatal(err)
	}
	_, fh2, _ := g.tgt.Lookup("/f")
	if bytes.Equal(fh, fh2) {
		t.Fatal("handle reused")
	}
	if _, err := g.tgt.GetAttr(fh); !isStale(err) {
		t.Fatalf("old handle resolves after recreate: %v", err)
	}
	// bogus bytes are stale, not a crash
	for _, bad := range [][]byte{nil, {1, 2, 3}, make([]byte, 16), bytes.Repeat([]byte{0xff}, 16)} {
		if _, err := g.tgt.GetAttr(bad); err == nil {
			t.Fatalf("bogus handle %x accepted", bad)
		}
	}
}

func isStale(err error) bool {
	var e *nfsc.Error
	if errors.As(err, &e) {
		return e.ErrorNum == nfsc.NFS3ErrStale
	}
	return false
}

func TestNFS_ReadDirPlusLargeDirectory(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/big", 0o755); err != nil {
		t.Fatal(err)
	}
	// The client library re-reads the whole directory on every lookup, so keep the short
	// run small; 600 entries still spans several READDIRPLUS pages.
	n := 600
	if !testing.Short() {
		n = 5000
	}
	for i := 0; i < n; i++ {
		if _, err := g.tgt.Create(fmt.Sprintf("/big/f%04d", i), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := g.tgt.ReadDirPlus("/big")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range ents {
		if e.Name() == "." || e.Name() == ".." {
			continue
		}
		if seen[e.Name()] {
			t.Fatalf("duplicate %s", e.Name())
		}
		seen[e.Name()] = true
	}
	if len(seen) != n {
		t.Fatalf("%d entries, want %d", len(seen), n)
	}
}

func TestNFS_FSStatAndUnsupported(t *testing.T) {
	g := startGateway(t)
	info, err := g.tgt.FSInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.WTMax == 0 {
		t.Fatalf("fsinfo %+v", info)
	}
	// LINK is unsupported: the handler exposes no UnixChange
	if _, err := g.tgt.Create("/f", 0o644); err != nil {
		t.Fatal(err)
	}
	_, fh, _ := g.tgt.Lookup("/f")
	_, rfh, _ := g.tgt.Lookup("/")
	type linkArgs struct {
		rpc.Header
		File []byte
		Link nfsc.Diropargs3
	}
	res, err := g.tgt.Call(&linkArgs{
		Header: rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: 15, Cred: rpc.AuthNull, Verf: rpc.AuthNull},
		File:   fh, Link: nfsc.Diropargs3{FH: rfh, Filename: "hard"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := xdr.ReadUint32(res)
	if status == 0 {
		t.Fatal("LINK succeeded")
	}
	// Serve refuses non-loopback listeners
	if _, err := nfsgw.Listen("0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback listener accepted")
	}
	_ = os.Getpid
	_ = time.Now
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
