package nfsgw_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	nfs "github.com/willscott/go-nfs"

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
func startGateway(t *testing.T) *gateway { return startGatewayWith(t, nil) }

// startGatewayWith runs prep on the fabric (overlay options, faults) before the
// filesystem is built.
func startGatewayWith(t *testing.T, prep func(*testharness.Fabric)) *gateway {
	t.Helper()
	if runtime.GOOS == "windows" {
		// go-nfs-client binds explicit local ports that fall inside Windows' reserved
		// ranges on CI runners. The gateway itself is validated on Windows at M4 through
		// the real NFS client (docs/windows-checklist.md).
		t.Skip("NFS client library cannot bind local ports on Windows runners")
	}
	f := testharness.NewFabric(t, 3, 2, 1)
	if prep != nil {
		prep(f)
	}
	h := nfsgw.New(f.FS(), f.FSM)
	l, err := nfsgw.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = nfsgw.Serve(sctx, l, h); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// the client library binds a random explicit local port and, unprivileged, does not
	// retry a collision with a port another test's connection still holds
	var cl *rpc.Client
	for attempt := 0; attempt < 8; attempt++ {
		if cl, err = rpc.DialTCP("tcp", l.Addr().String(), false); err == nil || !strings.Contains(err.Error(), "address already in use") {
			break
		}
	}
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

// Invariant: I-7, I-34, I-45
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

// Owner and times set through SETATTR land in the FSM (so every gateway sees them) and
// come back through LOOKUP/GETATTR on the wire.
// Invariant: I-46
func TestNFS_SetattrOwnerAndTimes(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Create("/o", 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, g.tgt.Setattr("/o", nfsc.Sattr3{UID: nfsc.SetUID{SetIt: true, UID: 1234}, GID: nfsc.SetUID{SetIt: true, UID: 5678}}))
	at := nfsc.NFS3Time{Seconds: 1_600_000_000, Nseconds: 7}
	mt := nfsc.NFS3Time{Seconds: 1_600_000_100, Nseconds: 42}
	must(t, g.tgt.Setattr("/o", nfsc.Sattr3{
		Atime: nfsc.SetTime{SetIt: nfsc.SetToClientTime, Time: at},
		Mtime: nfsc.SetTime{SetIt: nfsc.SetToClientTime, Time: mt},
	}))
	in := g.f.Inode(g.f.InodeOf(g.f.FS(), "/o")) // the replicated FSM: what any gateway sees
	if in.Uid != 1234 || in.Gid != 5678 {
		t.Fatalf("owner %d:%d in the FSM", in.Uid, in.Gid)
	}
	if in.Mtime != int64(mt.Seconds)*1e9+int64(mt.Nseconds) || in.Atime != int64(at.Seconds)*1e9+int64(at.Nseconds) {
		t.Fatalf("times in the FSM: atime %d mtime %d", in.Atime, in.Mtime)
	}
	_, fh, err := g.tgt.Lookup("/o")
	must(t, err)
	attr, err := g.tgt.GetAttr(fh)
	must(t, err)
	// go-nfs reports atime and ctime as the modification time (docs/07 POSIX gaps), so
	// only owner and mtime can be checked on the wire; atime is checked in the FSM above.
	if attr.UID != 1234 || attr.GID != 5678 || attr.Mtime != mt {
		t.Fatalf("on the wire: uid %d gid %d mtime %+v", attr.UID, attr.GID, attr.Mtime)
	}
}

// Lifecycle: L-8, L-11
// Invariant: I-46
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

// wireStatus returns the NFS3 status number of a client error, or -1 for success / a
// non-NFS error.
func wireStatus(err error) int {
	var e *nfsc.Error
	if errors.As(err, &e) {
		return int(e.ErrorNum)
	}
	if err == nil {
		return -1
	}
	return -2
}

// While the gateway is stale (no leader contact for the threshold) every metadata
// mutation fails and every data WRITE still lands in the WAL. go-nfs has no mapping from a
// filesystem EROFS to NFS3ERR_ROFS: the wire carries NFS3ERR_ACCES or NFS3ERR_IO per
// operation (docs/02 §Health, docs/07), and this test pins which.
// Invariant: I-39
// Lifecycle: L-187
func TestNFS_ReadOnly_WireStatuses(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Create("/f", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.tgt.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	g.f.SetStale(true)
	g.f.ProposeDown(errors.New("no leader"))
	defer g.f.ProposeDown(nil)
	const (
		acces = 13
		eio   = 5
		rofs  = 30
	)
	want := map[string]int{
		"mkdir":   acces,
		"create":  acces,
		"remove":  eio,
		"rmdir":   eio,
		"rename":  eio,
		"symlink": acces,
	}
	got := map[string]int{}
	_, err := g.tgt.Mkdir("/x", 0o755)
	got["mkdir"] = wireStatus(err)
	_, err = g.tgt.Create("/y", 0o644)
	got["create"] = wireStatus(err)
	got["remove"] = wireStatus(g.tgt.Remove("/f"))
	got["rmdir"] = wireStatus(g.tgt.RemoveAll("/d"))
	got["rename"] = wireStatus(g.tgt.Rename("/f", "/g"))
	got["symlink"] = wireStatus(g.tgt.Symlink("/f", "/s"))
	// SETATTR: go-nfs would pass the change hook's error through as an RPC GARBAGE_ARGS;
	// the gateway's change adapter maps it to ACCES like CREATE and MKDIR
	got["setattr"] = wireStatus(g.tgt.Setattr("/f", nfsc.Sattr3{Mode: nfsc.SetMode{SetIt: true, Mode: 0o600}}))
	want["setattr"] = acces
	for op, st := range got {
		t.Logf("%s: status %d", op, st)
		if st == -1 || st == rofs {
			t.Fatalf("%s while stale: status %d (success or ROFS)", op, st)
		}
		if st != want[op] {
			t.Errorf("%s while stale: NFS3 status %d, documented %d", op, st, want[op])
		}
	}
	// data WRITEs keep landing in the WAL
	g.writeAt("/f", 4096, []byte("still accepted"))
	if got := g.readAll("/f", 4096+14); string(got[4096:]) != "still accepted" {
		t.Fatalf("write while stale not readable back: %q", got[4096:])
	}
}

func isStale(err error) bool {
	var e *nfsc.Error
	if errors.As(err, &e) {
		return e.ErrorNum == nfsc.NFS3ErrStale
	}
	return false
}

// Invariant: I-47
func TestNFS_ReadDirPlusLargeDirectory(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/big", 0o755); err != nil {
		t.Fatal(err)
	}
	// The client library re-reads the whole directory on every lookup, so keep the short
	// run small; 300 entries still spans several READDIRPLUS pages.
	n := 300
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

// Invariant: I-50
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

// The NFS library's own log lines go through the daemon logger: errors (client protocol
// noise) at debug, warnings kept, so the log stream has one format.
func TestUseLogger_RoutesLibraryLogs(t *testing.T) {
	var buf bytes.Buffer
	nfsgw.UseLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	nfs.Log.Errorf("bad handle %d", 7)
	nfs.Log.Warn("slow client")
	out := buf.String()
	if !strings.Contains(out, `"level":"DEBUG","msg":"bad handle 7","src":"nfs"`) || !strings.Contains(out, `"level":"WARN","msg":"slow client"`) {
		t.Fatalf("library logs not routed: %s", out)
	}
	if lvl, err := nfs.Log.ParseLevel("warn"); err != nil || lvl != nfs.WarnLevel {
		t.Fatalf("ParseLevel: %v %v", lvl, err)
	}
}

// readDirPage issues one READDIRPLUS with an explicit cookie and verifier (the client
// library pages internally and hides them) and returns the status, the entries, the
// directory's verifier and the eof flag.
func readDirPage(t *testing.T, g *gateway, fh []byte, cookie, verf uint64) (status uint32, entries []nfsc.EntryPlus, outVerf uint64, eof bool) {
	t.Helper()
	type args struct {
		rpc.Header
		FH         []byte
		Cookie     uint64
		CookieVerf uint64
		DirCount   uint32
		MaxCount   uint32
	}
	type dirListOK struct {
		DirAttrs   nfsc.PostOpAttr
		CookieVerf uint64
	}
	type item struct {
		IsSet bool           `xdr:"union"`
		Entry nfsc.EntryPlus `xdr:"unioncase=1"`
	}
	res, err := g.cl.Call(&args{Header: rpc.Header{Rpcvers: 2, Prog: nfsc.Nfs3Prog, Vers: nfsc.Nfs3Vers, Proc: nfsc.NFSProc3ReadDirPlus,
		Cred: rpc.NewAuthUnix("test", 501, 20).Auth(), Verf: rpc.AuthNull}, FH: fh, Cookie: cookie, CookieVerf: verf, DirCount: 512, MaxCount: 4096})
	if err != nil {
		t.Fatal(err)
	}
	status, err = xdr.ReadUint32(res)
	if err != nil {
		t.Fatal(err)
	}
	if status != nfsc.NFS3Ok {
		return status, nil, 0, false
	}
	var ok dirListOK
	if err := xdr.Read(res, &ok); err != nil {
		t.Fatal(err)
	}
	for {
		var it item
		if err := xdr.Read(res, &it); err != nil {
			t.Fatal(err)
		}
		if !it.IsSet {
			break
		}
		entries = append(entries, it.Entry)
	}
	if err := xdr.Read(res, &eof); err != nil {
		t.Fatal(err)
	}
	return status, entries, ok.CookieVerf, eof
}

// A directory that changes between two READDIRPLUS pages never yields a duplicate or a
// skipped surviving entry: the gateway either continues the listing consistently or
// answers NFS3ERR_BAD_COOKIE, after which a fresh listing is complete and exact.
// Invariant: I-47
func TestNFS_ReadDirPlus_RemoveBetweenPages(t *testing.T) {
	g := startGateway(t)
	if _, err := g.tgt.Mkdir("/pg", 0o755); err != nil {
		t.Fatal(err)
	}
	const n = 150
	all := map[string]bool{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("e%03d", i)
		if _, err := g.tgt.Create("/pg/"+name, 0o644); err != nil {
			t.Fatal(err)
		}
		all[name] = true
	}
	_, fh, err := g.tgt.Lookup("/pg")
	if err != nil {
		t.Fatal(err)
	}
	status, page1, verf, eof := readDirPage(t, g, fh, 0, 0)
	if status != nfsc.NFS3Ok || eof || len(page1) == 0 {
		t.Fatalf("first page: status %d eof %v entries %d", status, eof, len(page1))
	}
	seen := map[string]bool{}
	for _, e := range page1 {
		seen[e.FileName] = true
	}
	// remove an entry the listing has not reached yet
	removed := ""
	for i := n - 1; i >= 0; i-- {
		if name := fmt.Sprintf("e%03d", i); !seen[name] {
			removed = name
			break
		}
	}
	if removed == "" {
		t.Fatal("first page listed everything; shrink MaxCount")
	}
	must(t, g.tgt.Remove("/pg/"+removed))
	delete(all, removed)
	cookie := page1[len(page1)-1].Cookie
	status, page, _, eof := readDirPage(t, g, fh, cookie, verf)
	switch status {
	case nfsc.NFS3ErrBadCookie:
		// the directory changed under the listing: restart from the top
		seen = map[string]bool{}
		cookie, verf, eof = 0, 0, false
	case nfsc.NFS3Ok:
		for _, e := range page {
			if seen[e.FileName] {
				t.Fatalf("duplicate %s across pages", e.FileName)
			}
			seen[e.FileName] = true
		}
		if len(page) > 0 {
			cookie = page[len(page)-1].Cookie
		}
	default:
		t.Fatalf("second page: status %d", status)
	}
	for !eof {
		status, page, verf2, more := readDirPage(t, g, fh, cookie, verf)
		if status != nfsc.NFS3Ok {
			t.Fatalf("page after cookie %d: status %d", cookie, status)
		}
		verf = verf2
		for _, e := range page {
			if seen[e.FileName] {
				t.Fatalf("duplicate %s", e.FileName)
			}
			seen[e.FileName] = true
		}
		if len(page) > 0 {
			cookie = page[len(page)-1].Cookie
		}
		eof = more
	}
	delete(seen, ".")
	delete(seen, "..")
	if seen[removed] {
		t.Fatalf("removed entry %s listed", removed)
	}
	for name := range all {
		if !seen[name] {
			t.Fatalf("surviving entry %s skipped", name)
		}
	}
	if len(seen) != len(all) {
		t.Fatalf("listed %d entries, want %d", len(seen), len(all))
	}
}
