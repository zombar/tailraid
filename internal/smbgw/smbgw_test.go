package smbgw_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudsoda/go-smb2"
	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/smbgw"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
	"github.com/zombar/tailraid/internal/vfs"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var ctx = context.Background()

// NT statuses the tests pin (docs/15 §3, §4).
const (
	statusAccessDenied   uint32 = 0xC0000022
	statusObjectNotFound uint32 = 0xC0000034
	statusCollision      uint32 = 0xC0000035
	statusLogonFailure   uint32 = 0xC000006D
	statusDiskFull       uint32 = 0xC000007F
	statusIoError        uint32 = 0xC00000E9
)

const password = "correct-horse"

type gateway struct {
	t    *testing.T
	f    *testharness.Fabric
	fs   *vfs.FS
	sh   *smb2.Share
	addr string
}

// startGateway serves a fabric over SMB on a random loopback port and mounts the share
// through the go-smb2 client (the one rclone uses), NTLMv2 with the test password.
func startGateway(t *testing.T) *gateway { return startGatewayWith(t, nil) }

// startGatewayWith runs prep on the fabric (overlay options, faults) before the
// filesystem is built.
func startGatewayWith(t *testing.T, prep func(*testharness.Fabric)) *gateway {
	return startGatewayBound(t, prep, nil)
}

// startGatewayBound is startGatewayWith on a gateway with the device's bindings (nil:
// none, every session is served the node's filesystem); the accounts in the map that
// are not the mount account are added with the same password.
func startGatewayBound(t *testing.T, prep func(*testharness.Fabric), bind map[string]string) *gateway {
	t.Helper()
	f := testharness.NewFabric(t, 3, 2, 1)
	if prep != nil {
		prep(f)
	}
	fs := f.FS()
	var b smbgw.Bindings
	if bind != nil {
		b = bindMap(bind)
	}
	srv, err := smbgw.NewServerWith(fs, f.FSM, password, b)
	must(t, err)
	for cred := range bind {
		if account, ok := strings.CutPrefix(cred, "smb:"); ok && account != smbgw.User {
			srv.AddUser(account, password)
		}
	}
	l, err := smbgw.Listen(t.Context(), "127.0.0.1:0")
	must(t, err)
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = smbgw.Serve(sctx, l, srv); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	g := &gateway{t: t, f: f, fs: fs, addr: l.Addr().String()}
	sess := dial(t, g.addr, smbgw.User, password)
	g.sh, err = sess.Mount(smbgw.ShareName)
	must(t, err)
	t.Cleanup(func() { _ = g.sh.Umount(); _ = sess.Logoff() })
	return g
}

// bindMap is a device's bindings for a test: credential to user name.
type bindMap map[string]string

func (b bindMap) Lookup(cred string) (string, bool) { u, ok := b[cred]; return u, ok }

// mountAs mounts the share as account with the test password.
func (g *gateway) mountAs(account string) *smb2.Share {
	g.t.Helper()
	sess := dial(g.t, g.addr, account, password)
	sh, err := sess.Mount(smbgw.ShareName)
	must(g.t, err)
	g.t.Cleanup(func() { _ = sh.Umount(); _ = sess.Logoff() })
	return sh
}

func dial(t *testing.T, addr, user, pw string) *smb2.Session {
	t.Helper()
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: pw}}
	sess, err := d.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return sess
}

// ntStatus is the NT status an error carries, 0 for nil and 1 for an error without one.
// The client turns the three statuses a CREATE answers most into os errors before they
// reach a caller, so those are read back from the os error.
func ntStatus(err error) uint32 {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, os.ErrNotExist):
		return statusObjectNotFound
	case errors.Is(err, os.ErrExist):
		return statusCollision
	case errors.Is(err, os.ErrPermission):
		return statusAccessDenied
	}
	if re, ok := errors.AsType[*smb2.ResponseError](err); ok {
		return re.Code
	}
	return 1
}

func (g *gateway) create(path string) {
	g.t.Helper()
	fh, err := g.sh.Create(rel(path))
	if err != nil {
		g.t.Fatalf("create %s: %v", path, err)
	}
	must(g.t, fh.Close())
}

// writeAt is one client write at an offset: the client splits it into WRITE requests of
// at most the negotiated size on one open handle.
func (g *gateway) writeAt(path string, off int64, p []byte) error {
	g.t.Helper()
	fh, err := g.sh.OpenFile(rel(path), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	_, err = fh.WriteAt(p, off)
	return err
}

func (g *gateway) readAll(path string, n int) []byte {
	g.t.Helper()
	fh, err := g.sh.Open(rel(path))
	if err != nil {
		g.t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = fh.Close() }()
	out := make([]byte, n)
	got, err := fh.ReadAt(out, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		g.t.Fatalf("read %s: %v", path, err)
	}
	return out[:got]
}

func (g *gateway) fileID(path string) uint64 {
	g.t.Helper()
	fh, err := g.sh.Open(rel(path))
	if err != nil {
		g.t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = fh.Close() }()
	fi, err := fh.Stat()
	must(g.t, err)
	return fi.Sys().(*smb2.FileStat).FileId
}

// rel is the share-relative spelling the client wants for a vfs path.
func rel(path string) string { return strings.TrimPrefix(path, "/") }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Pieces of a file written in a shuffled order, each its own WRITE at its offset, on two
// handles at once, read back whole: dirty and after the flush (docs/15 §3).
// Invariant: I-106, I-7
func TestSMB_WriteSequence_MultiRequest(t *testing.T) {
	g := startGateway(t)
	cs := int64(testharness.ChunkSize)
	payload := testharness.Pattern(int(5*cs+17), 9)
	g.create("/f")
	type piece struct{ off, n int64 }
	var pieces []piece
	for off := int64(0); off < int64(len(payload)); off += cs / 4 {
		pieces = append(pieces, piece{off, min(cs/4, int64(len(payload))-off)})
	}
	seed, rng := testseed.New(t)
	rng.Shuffle(len(pieces), func(i, j int) { pieces[i], pieces[j] = pieces[j], pieces[i] })
	a, err := g.sh.OpenFile("f", os.O_RDWR, 0)
	must(t, err)
	b, err := g.sh.OpenFile("f", os.O_RDWR, 0)
	must(t, err)
	for i, p := range pieces {
		fh := a
		if i%2 == 1 {
			fh = b
		}
		if _, err := fh.WriteAt(payload[p.off:p.off+p.n], p.off); err != nil {
			t.Fatalf("seed %d: write %d@%d: %v", seed, p.n, p.off, err)
		}
	}
	must(t, a.Close())
	must(t, b.Close())
	if got := g.readAll("/f", len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("seed %d: dirty content differs at %d", seed, testharness.FirstDiff(got, payload))
	}
	must(t, g.f.Flusher().FlushAll(ctx))
	if got := g.f.ReadFile(g.f.InodeOf(g.f.FS(), "/f")); !bytes.Equal(got, payload) {
		t.Fatalf("seed %d: flushed content differs at %d", seed, testharness.FirstDiff(got, payload))
	}
}

// Writes of 0, 1, MaxWriteSize−1, MaxWriteSize and MaxWriteSize+1 bytes at a non-zero
// offset (the client sends the last as two requests) read back identically.
// Invariant: I-106
func TestSMB_WriteSize_Boundaries(t *testing.T) {
	g := startGateway(t)
	const maxWrite = 1 << 20
	const off = 4096
	for i, size := range []int{0, 1, maxWrite - 1, maxWrite, maxWrite + 1} {
		name := fmt.Sprintf("/w%d", i)
		g.create(name)
		payload := testharness.Pattern(size, byte(20+i))
		must(t, g.writeAt(name, off, payload))
		got := g.readAll(name, off+size)
		if size == 0 { // nothing written, nothing extended: the file is still empty
			if len(got) != 0 {
				t.Fatalf("size 0: file grew to %d bytes", len(got))
			}
			continue
		}
		if len(got) != off+size || !bytes.Equal(got[off:], payload) || !bytes.Equal(got[:off], make([]byte, off)) {
			t.Fatalf("size %d: read back %d bytes, differs at %d", size, len(got), testharness.FirstDiff(got[off:], payload))
		}
	}
}

// SET_INFO end-of-file shrinks and grows through the vfs and lands in the FSM; past
// MaxFileSize it is STATUS_DISK_FULL and nothing changes.
// Invariant: I-107, I-64
func TestSMB_SetInfo_EndOfFile(t *testing.T) {
	g := startGateway(t)
	cs := int64(testharness.ChunkSize)
	g.create("/f")
	payload := testharness.Pattern(int(3*cs), 4)
	must(t, g.writeAt("/f", 0, payload))
	must(t, g.sh.Truncate("f", cs+5))
	if got := g.readAll("/f", int(4*cs)); !bytes.Equal(got, payload[:cs+5]) {
		t.Fatalf("shrunk: %d bytes, differs at %d", len(got), testharness.FirstDiff(got, payload[:cs+5]))
	}
	must(t, g.sh.Truncate("f", 2*cs))
	want := append(bytes.Clone(payload[:cs+5]), make([]byte, cs-5)...)
	if got := g.readAll("/f", int(4*cs)); !bytes.Equal(got, want) {
		t.Fatalf("grown: %d bytes, differs at %d", len(got), testharness.FirstDiff(got, want))
	}
	must(t, g.f.Flusher().FlushAll(ctx))
	ino := g.f.InodeOf(g.f.FS(), "/f")
	if in := g.f.Inode(ino); in.Size != uint64(2*cs) {
		t.Fatalf("FSM size %d, want %d", in.Size, 2*cs)
	}
	limit := meta.MaxFileSize(testharness.ChunkSize)
	if st := ntStatus(g.sh.Truncate("f", int64(limit)+1)); st != statusDiskFull {
		t.Fatalf("end-of-file past MaxFileSize: status %#x, want STATUS_DISK_FULL", st)
	}
	if in := g.f.Inode(ino); in.Size != uint64(2*cs) {
		t.Fatalf("FSM size changed to %d", in.Size)
	}
}

// The times a client sees are the inode's own (the server copy carries them, docs/15
// §2), and the times a client sets at the end of a copy reach the FSM.
// Invariant: I-107, I-46
func TestSMB_Times_FromFSMAndSetInfo(t *testing.T) {
	g := startGateway(t)
	g.create("/f")
	in := g.f.Inode(g.f.InodeOf(g.f.FS(), "/f"))
	fh, err := g.sh.Open("f")
	must(t, err)
	fi, err := fh.Stat()
	must(t, err)
	must(t, fh.Close())
	st := fi.Sys().(*smb2.FileStat)
	// FILETIME has 100 ns ticks
	if got, want := st.LastWriteTime.UnixNano()/100, in.Mtime/100; got != want {
		t.Fatalf("LastWriteTime %v, inode mtime %v", st.LastWriteTime, time.Unix(0, in.Mtime))
	}
	if got, want := st.LastAccessTime.UnixNano()/100, in.Atime/100; got != want {
		t.Fatalf("LastAccessTime %v, inode atime %v", st.LastAccessTime, time.Unix(0, in.Atime))
	}
	if got, want := st.ChangeTime.UnixNano()/100, in.Ctime/100; got != want {
		t.Fatalf("ChangeTime %v, inode ctime %v", st.ChangeTime, time.Unix(0, in.Ctime))
	}
	atime := time.Date(2020, 2, 3, 4, 5, 6, 700, time.UTC)
	mtime := time.Date(2021, 7, 8, 9, 10, 11, 1200, time.UTC)
	must(t, g.sh.Chtimes("f", atime, mtime))
	in = g.f.Inode(g.f.InodeOf(g.f.FS(), "/f"))
	if in.Mtime/100 != mtime.UnixNano()/100 || in.Atime/100 != atime.UnixNano()/100 {
		t.Fatalf("FSM times %v %v after SET_INFO, want %v %v", time.Unix(0, in.Atime), time.Unix(0, in.Mtime), atime, mtime)
	}
	fi, err = g.sh.Stat("f")
	must(t, err)
	if got := fi.ModTime().UnixNano() / 100; got != mtime.UnixNano()/100 {
		t.Fatalf("ModTime after SET_INFO %v, want %v", fi.ModTime(), mtime)
	}
}

// A rename of the file or of an ancestor keeps the FileId (the inode number) and the
// content, and the FSM holds the new name after commit.
// Invariant: I-107, I-108, I-16
func TestSMB_RenameKeepsFileIdAndContent(t *testing.T) {
	g := startGateway(t)
	must(t, g.sh.Mkdir("d", 0o755))
	g.create("/d/a")
	must(t, g.writeAt("/d/a", 100, []byte("payload")))
	ino := g.f.InodeOf(g.f.FS(), "/d/a")
	if id := g.fileID("/d/a"); id != uint64(ino) {
		t.Fatalf("FileId %d, inode %d", id, ino)
	}
	must(t, g.sh.Rename("d/a", "d/b"))
	must(t, g.sh.Rename("d", "e"))
	if id := g.fileID("/e/b"); id != uint64(ino) {
		t.Fatalf("FileId after renames %d, inode %d", id, ino)
	}
	if got := g.readAll("/e/b", 107); string(got[100:]) != "payload" {
		t.Fatalf("content after renames: %q", got)
	}
	if in := g.f.Inode(ino); in.Name != "b" {
		t.Fatalf("FSM name %q, want b", in.Name)
	}
}

// QUERY_DIRECTORY paging returns every entry once with the inode number as its FileId,
// for a directory that spans many pages.
// Invariant: I-108
func TestSMB_QueryDirectory_LargeDirectory(t *testing.T) {
	g := startGateway(t)
	must(t, g.sh.Mkdir("big", 0o755))
	n := 300
	if !testing.Short() {
		n = 10000
	}
	for i := range n {
		g.create(fmt.Sprintf("/big/f%05d", i))
	}
	ents, err := g.sh.ReadDir("big")
	must(t, err)
	seen := map[string]bool{}
	for _, e := range ents {
		if seen[e.Name()] {
			t.Fatalf("duplicate %s", e.Name())
		}
		seen[e.Name()] = true
		if id := e.Sys().(*smb2.FileStat).FileId; id != uint64(g.f.InodeOf(g.f.FS(), "/big/"+e.Name())) {
			t.Fatalf("%s: FileId %d is not its inode", e.Name(), id)
		}
	}
	if len(seen) != n {
		t.Fatalf("%d entries, want %d", len(seen), n)
	}
}

// While the gateway is stale every metadata mutation fails and every data WRITE still
// lands in the WAL, as does an end-of-file change. The library has no mapping for EROFS,
// so the adapter marks it a permission error and the wire carries STATUS_ACCESS_DENIED
// (docs/15 §4). A delete is a delete-on-close whose failure the server does not report:
// the file stays (§8).
// Invariant: I-109, I-39
// Lifecycle: L-187
func TestSMB_ReadOnly_WireStatuses(t *testing.T) {
	g := startGateway(t)
	g.create("/f")
	must(t, g.sh.Mkdir("d", 0o755))
	if _, err := g.sh.Open("nope"); ntStatus(err) != statusObjectNotFound {
		t.Fatalf("missing path: status %#x", ntStatus(err))
	}
	if _, err := g.sh.OpenFile("f", os.O_CREATE|os.O_EXCL, 0o644); ntStatus(err) != statusCollision {
		t.Fatalf("colliding create: status %#x", ntStatus(err))
	}
	g.f.SetStale(true)
	g.f.ProposeDown(errors.New("no leader"))
	defer g.f.ProposeDown(nil)
	got := map[string]uint32{}
	got["mkdir"] = ntStatus(g.sh.Mkdir("x", 0o755))
	_, err := g.sh.Create("y")
	got["create"] = ntStatus(err)
	got["rename"] = ntStatus(g.sh.Rename("f", "g"))
	for op, st := range got {
		t.Logf("%s: status %#x", op, st)
		if st != statusAccessDenied {
			t.Errorf("%s while stale: status %#x, want STATUS_ACCESS_DENIED", op, st)
		}
	}
	if err := g.sh.Remove("f"); err != nil {
		t.Fatalf("remove while stale reported %v", err)
	}
	if _, err := g.f.FS().Stat("/f"); err != nil {
		t.Fatalf("remove while stale took effect: %v", err)
	}
	must(t, g.writeAt("/f", 4096, []byte("still accepted")))
	if got := g.readAll("/f", 4096+14); string(got[4096:]) != "still accepted" {
		t.Fatalf("write while stale not readable back: %q", got[4096:])
	}
	// so does an end-of-file change: it is a WAL record like a write, not a proposal
	must(t, g.sh.Truncate("f", 10))
	if got := g.readAll("/f", 4096); len(got) != 10 {
		t.Fatalf("truncate while stale: %d bytes, want 10", len(got))
	}
}

// When the cluster has no room for the shards, the WRITE that meets the cap fails with
// STATUS_DISK_FULL, never an I/O error.
// Invariant: I-109, I-79
// Lifecycle: L-189
func TestSMB_WriteWhenFull_IsDiskFull(t *testing.T) {
	g := startGatewayWith(t, func(f *testharness.Fabric) {
		f.RestartOverlay(vfs.WithWALCap(1000), vfs.WithCapWait(0))
		for n := meta.NodeIdx(1); n <= 3; n++ {
			f.SetFull(n, true)
		}
	})
	g.create("/f")
	must(t, g.writeAt("/f", 0, testharness.Pattern(900, 6))) // into the WAL, acknowledged
	if err := g.f.Flusher().FlushAll(ctx); err == nil {
		t.Fatal("flush succeeded against full holders")
	}
	must(t, g.writeAt("/f", 0, testharness.Pattern(900, 6))) // the WAL reaches its cap
	if st := ntStatus(g.writeAt("/f", 0, []byte("x"))); st != statusDiskFull {
		t.Fatalf("WRITE at the cap behind a full cluster: status %#x, want STATUS_DISK_FULL", st)
	}
}

// A client that outruns the flusher is throttled, not failed: the WRITE that meets the
// WAL cap is held until a flush has committed, then answered, and every byte is there.
// Invariant: I-109, I-79
func TestSMB_WriteAtCap_WaitsThenSucceeds(t *testing.T) {
	cs := int64(testharness.ChunkSize)
	g := startGatewayWith(t, func(f *testharness.Fabric) { f.RestartOverlay(vfs.WithWALCap(2 * cs)) })
	g.create("/big")
	payload := testharness.Pattern(int(4*cs), 5)
	done := make(chan error, 1)
	go func() {
		fh, err := g.sh.OpenFile("big", os.O_RDWR, 0)
		if err != nil {
			done <- err
			return
		}
		for off := int64(0); off < int64(len(payload)); off += cs {
			if _, err := fh.WriteAt(payload[off:off+cs], off); err != nil {
				done <- err
				return
			}
		}
		done <- fh.Close()
	}()
	// the third WRITE meets the cap and waits; flushing (as the run loop would at the
	// cap) frees the WAL and lets it through
	testharness.Eventually(t, func() bool {
		must(t, g.f.Flusher().FlushAll(ctx))
		g.f.Clk.Advance(250 * time.Millisecond)
		select {
		case err := <-done:
			must(t, err)
			return true
		default:
			return false
		}
	}, "writes never completed past the WAL cap")
	must(t, g.f.Flusher().FlushAll(ctx))
	if got := g.readAll("/big", len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, payload))
	}
}

// A chunk the reader cannot rebuild (every holder down) answers the READ with
// STATUS_UNEXPECTED_IO_ERROR, which a client surfaces as an error, never
// STATUS_END_OF_FILE, which a client takes for a shorter file and hands the caller a
// short, wrong buffer; the three-VM rounds caught the latter through a Windows mount
// (docs/vms.md).
// Invariant: I-40, I-109
func TestSMB_UnreadableChunk_IsIoError(t *testing.T) {
	g := startGateway(t)
	g.create("/f")
	payload := testharness.Pattern(3000, 7)
	must(t, g.writeAt("/f", 0, payload))
	must(t, g.f.Flusher().FlushAll(ctx)) // committed: the dirty copy is gone, reads go to the shards
	for n := meta.NodeIdx(1); n <= 3; n++ {
		g.f.SetDown(n, true)
	}
	fh, err := g.sh.Open("f")
	must(t, err)
	defer func() { _ = fh.Close() }()
	out := make([]byte, len(payload))
	n, err := fh.ReadAt(out, 0)
	if st := ntStatus(err); st != statusIoError {
		t.Fatalf("read with every holder down: %d bytes, status %#x (%v), want STATUS_UNEXPECTED_IO_ERROR", n, st, err)
	}
}

// The share's size is meta.Usage, as NFS FSSTAT and `status` report it.
// Invariant: I-109
func TestSMB_Statfs_FromUsage(t *testing.T) {
	g := startGateway(t)
	g.create("/f")
	must(t, g.writeAt("/f", 0, testharness.Pattern(int(testharness.ChunkSize)*2, 1)))
	must(t, g.f.Flusher().FlushAll(ctx))
	fs, err := g.sh.Statfs("")
	must(t, err)
	total, _, free := meta.Usage(g.f.FSM.Volume(), g.f.FSM.Nodes(), g.f.FSM.Stats())
	unit := fs.BlockSize() // bytes per allocation unit
	if got := fs.TotalBlockCount() * unit; got != total/unit*unit || total == 0 {
		t.Fatalf("total %d, Usage %d (unit %d)", got, total, unit)
	}
	if got := fs.AvailableBlockCount() * unit; got != free/unit*unit || free >= total {
		t.Fatalf("free %d, Usage %d of %d", got, free, total)
	}
}

// A wrong password or an unknown user is STATUS_LOGON_FAILURE; the server has no guest.
// Invariant: I-110
func TestSMB_Auth_WrongPasswordRefused(t *testing.T) {
	g := startGateway(t)
	for _, c := range []struct{ user, pw string }{{smbgw.User, "wrong"}, {"guest", ""}, {"", ""}} {
		d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: c.user, Password: c.pw}}
		sess, err := d.Dial(ctx, g.addr)
		if err == nil {
			_ = sess.Logoff()
			t.Fatalf("user %q password %q: session established", c.user, c.pw)
		}
		if st := ntStatus(err); st != statusLogonFailure {
			t.Fatalf("user %q password %q: status %#x (%v), want STATUS_LOGON_FAILURE", c.user, c.pw, st, err)
		}
	}
}

// The gateway binds loopback only.
// Invariant: I-110, I-50
func TestSMB_ListenRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "10.0.0.1:0", "[::]:0", "nonsense"} {
		if l, err := smbgw.Listen(t.Context(), addr); err == nil {
			_ = l.Close()
			t.Fatalf("%s: listener accepted", addr)
		}
	}
	if _, err := smbgw.NewServer(nil, nil, ""); err == nil {
		t.Fatal("server without a password accepted")
	}
}

// Dot files are HIDDEN to the client (Samba's "hide dot files"), in a listing and in a
// stat, and still listed and readable; every other name is not. Explorer showed the ._
// files macOS writes beside each file it copies over NFS.
// Invariant: I-141
func TestSMB_DotFilesHidden(t *testing.T) {
	g := startGateway(t)
	must(t, g.sh.Mkdir("d", 0o755))
	for _, name := range []string{"d/photo.jpg", "d/._photo.jpg", "d/.DS_Store"} {
		g.create(name)
	}
	must(t, g.sh.Mkdir("d/.config", 0o755))
	const hidden = 0x2 // FILE_ATTRIBUTE_HIDDEN
	attrs := func(fi os.FileInfo) uint32 { return fi.Sys().(*smb2.FileStat).FileAttributes }
	ents, err := g.sh.ReadDir("d")
	must(t, err)
	got := map[string]bool{}
	for _, e := range ents {
		got[e.Name()] = attrs(e)&hidden != 0
	}
	want := map[string]bool{"photo.jpg": false, "._photo.jpg": true, ".DS_Store": true, ".config": true}
	if len(got) != len(want) {
		t.Fatalf("listing %v, want every entry listed: %v", got, want)
	}
	for name, h := range want {
		if got[name] != h {
			t.Errorf("%s: hidden %v in the listing, want %v", name, got[name], h)
		}
	}
	for name, h := range map[string]bool{"d/._photo.jpg": true, "d/photo.jpg": false, "d": false} {
		fi, err := g.sh.Stat(name)
		must(t, err)
		if (attrs(fi)&hidden != 0) != h {
			t.Errorf("stat %s: attributes %#x, want hidden %v", name, attrs(fi), h)
		}
	}
	if _, err := g.sh.ReadFile("d/._photo.jpg"); err != nil {
		t.Errorf("a hidden file does not read: %v", err)
	}
}
