package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// gatedSource is a mirror source that counts the object reads in flight, keeps the most
// seen at once, holds the first `meet` reads until that many are in flight together (or a
// second has passed, so a restore one chunk at a time fails the test instead of hanging),
// and blocks every read of block until release closes.
type gatedSource struct {
	*testharness.Mirrors
	mu      sync.Mutex
	in, max int
	meet    int
	met     chan struct{}
	block   meta.Hash
	release chan struct{}
	blocked chan struct{} // closed once a read of block is waiting
}

func gateSource(src *testharness.Mirrors, meet int) *gatedSource {
	return &gatedSource{Mirrors: src, meet: meet, met: make(chan struct{}), release: make(chan struct{}), blocked: make(chan struct{})}
}

func (g *gatedSource) Object(ctx context.Context, m meta.NodeIdx, h meta.Hash) ([]byte, error) {
	g.mu.Lock()
	g.in++
	g.max = max(g.max, g.in)
	if g.in == g.meet {
		close(g.met)
	}
	block := h == g.block && g.block != meta.HashZero
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.in--; g.mu.Unlock() }()
	if block {
		close(g.blocked)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case <-g.met:
	case <-time.After(time.Second): // a restore one chunk at a time never meets: it goes on, and the test sees max 1
	}
	return g.Mirrors.Object(ctx, m, h)
}

// treeRig is a snapshot of a folder with large and small files, a hole, a link and a
// subfolder, whose chunks are then collected from the pool, so a restore reads every one
// from the mirror.
type treeRig struct {
	f    *testharness.Fabric
	fs   *vfs.FS
	src  *testharness.Mirrors
	want map[string][]byte
	big  meta.Hash // the first chunk of /t/a.bin
}

func newTreeRig(t *testing.T) *treeRig {
	t.Helper()
	f := testharness.NewFabric(t, 3, 2, 1)
	r := &treeRig{f: f, fs: f.FS(), src: testharness.NewMirrors(), want: map[string][]byte{}}
	r.fs.SetFlusher(f.Flusher())
	cs := int(f.FSM.Volume().ChunkSize)
	put := func(name string, data []byte, off int) { // written at a non-zero offset after the head
		t.Helper()
		must(t, r.fs.MkdirAll(path.Dir(name), 0o755))
		fh, err := r.fs.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
		must(t, err)
		if off > 0 {
			_, err = fh.Write(data[:off])
			must(t, err)
		}
		_, err = fh.(*vfs.File).WriteAt(data[off:], int64(off))
		must(t, err)
		must(t, fh.Close())
		r.want[name] = data
	}
	put("/t/a.bin", testharness.Pattern(3*cs+cs/2, 1), cs+5)
	put("/t/b/c.bin", testharness.Pattern(cs+100, 2), 77)
	put("/t/b/d.txt", []byte("small"), 2)
	holey := append(make([]byte, 2*cs), testharness.Pattern(cs/2, 3)...)
	put("/t/b/e/f.bin", holey, 2*cs)
	for i := range 12 {
		put(fmt.Sprintf("/t/s/%02d.dat", i), testharness.Pattern(1000+i*997, byte(10+i)), 1)
	}
	must(t, r.fs.Symlink("a.bin", "/t/link"))
	must(t, f.Flusher().FlushAll(ctx))
	in, err := f.FSM.Resolve([]string{"t", "a.bin"})
	must(t, err)
	r.big = in.Chunks[0]
	m := f.AddMirror(r.src, "host/m1")
	f.TakeSnapshot(r.src, m, "s1")
	r.fs.SetMirrors(r.src)
	// the live tree goes, and its chunks with it: every restored chunk comes from the mirror
	tin, err := f.FSM.Resolve([]string{"t"})
	must(t, err)
	f.MustApply(meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: meta.RootIno, Name: "t", Recursive: true, Now: f.Clk.Now().UnixNano()}})
	f.MustApply(meta.Command{Op: meta.OpPurge, Purge: &meta.Purge{Ino: tin.Ino, Now: f.Clk.Now().UnixNano()}})
	f.MustApply(meta.Command{Op: meta.OpGCDelete, GCDelete: &meta.GCDelete{Before: 1 << 62}})
	if _, ok := f.FSM.GetChunk(r.big); ok {
		t.Fatal("the snapshot's chunk is still in the pool")
	}
	return r
}

// tree lists every entry under dir with its kind and bytes (a link's target).
func (r *treeRig) tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(p string)
	walk = func(p string) {
		fis, err := r.fs.ReadDir(p)
		must(t, err)
		for _, fi := range fis {
			q := path.Join(p, fi.Name())
			rel := strings.TrimPrefix(q, dir)
			switch {
			case fi.IsDir():
				out[rel] = "dir"
				walk(q)
			case fi.Mode()&os.ModeSymlink != 0:
				target, err := r.fs.Readlink(q)
				must(t, err)
				out[rel] = "link " + target
			default:
				b, err := readAt(t, r.fs, q, int(fi.Size())+1, 0)
				must(t, err)
				out[rel] = fmt.Sprintf("file %x", b)
			}
		}
	}
	walk(dir)
	return out
}

// A restore of four files and eight chunks at once writes the tree a restore one chunk at
// a time writes, every file's bytes the snapshot's, and has more than one and never more
// than eight chunks in flight.
//
// Invariant: I-366
func TestRestoreJob_ParallelEqualsSerial(t *testing.T) {
	r := newTreeRig(t)
	g := gateSource(r.src, 4)
	r.fs.SetMirrors(g)
	if _, err := r.fs.RestoreSnapshotWith(ctx, "/.snapshots/host-m1/s1/t", "/par", vfs.RestoreOptions{Files: vfs.RestoreFiles, InFlight: vfs.RestoreInFlight}); err != nil {
		t.Fatal(err)
	}
	if g.max < 2 || g.max > vfs.RestoreInFlight {
		t.Fatalf("%d chunk reads in flight at most, want 2..%d", g.max, vfs.RestoreInFlight)
	}
	r.fs.SetMirrors(r.src)
	if _, err := r.fs.RestoreSnapshot(ctx, "/.snapshots/host-m1/s1/t", "/ser"); err != nil {
		t.Fatal(err)
	}
	par, ser := r.tree(t, "/par"), r.tree(t, "/ser")
	if len(par) != len(ser) || len(par) != 20 {
		t.Fatalf("%d entries in parallel, %d serial, want 20", len(par), len(ser))
	}
	for k, v := range ser {
		if par[k] != v {
			t.Fatalf("%s differs between the parallel and the serial restore", k)
		}
	}
	for name, data := range r.want {
		got, err := readAt(t, r.fs, "/par"+strings.TrimPrefix(name, "/t"), len(data)+1, 0)
		must(t, err)
		if !bytes.Equal(got, data) {
			t.Fatalf("%s differs at %d", name, testharness.FirstDiff(got, data))
		}
	}
}

// The counted totals are what the restore writes, and the progress it reports adds up to
// them: chunk by chunk and file by file into an empty folder, and as files already there
// when run again over the result (I-365).
//
// Invariant: I-365
func TestRestoreJob_ProgressAddsUpToTotals(t *testing.T) {
	r := newTreeRig(t)
	files, total, err := r.fs.RestoreTotals(ctx, "/.snapshots/host-m1/s1/t", "/out")
	must(t, err)
	var wantBytes int64
	for _, d := range r.want {
		wantBytes += int64(len(d))
	}
	if files != int64(len(r.want)) || total != wantBytes {
		t.Fatalf("totals %d files %d bytes, want %d and %d", files, total, len(r.want), wantBytes)
	}
	for _, rerun := range []bool{false, true} {
		var mu sync.Mutex
		var gotFiles, gotBytes, existing int64
		_, err := r.fs.RestoreSnapshotWith(ctx, "/.snapshots/host-m1/s1/t", "/out", vfs.RestoreOptions{
			Files: vfs.RestoreFiles, InFlight: vfs.RestoreInFlight,
			Progress: func(p vfs.RestoreProgress) {
				mu.Lock()
				defer mu.Unlock()
				if p.Files < 0 || p.Bytes < 0 {
					t.Errorf("a step that falls: %+v", p)
				}
				gotFiles += p.Files
				gotBytes += p.Bytes
				if p.Existing {
					existing += p.Files
				}
			},
		})
		must(t, err)
		if gotFiles != files || gotBytes != total {
			t.Fatalf("rerun %v: progress %d files %d bytes, totals %d and %d", rerun, gotFiles, gotBytes, files, total)
		}
		if want := map[bool]int64{false: 0, true: files}[rerun]; existing != want {
			t.Fatalf("rerun %v: %d files already there, want %d", rerun, existing, want)
		}
	}
}

// A cancelled restore stops before its next chunk: the file it was writing leaves no
// temporary file and no name, and the files it had finished stay whole (I-368).
//
// Invariant: I-368
func TestRestoreJob_Cancel_NoTempLeft(t *testing.T) {
	r := newTreeRig(t)
	g := gateSource(r.src, 1)
	g.block = r.big
	r.fs.SetMirrors(g)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := r.fs.RestoreSnapshotWith(cctx, "/.snapshots/host-m1/s1/t", "/out", vfs.RestoreOptions{Files: vfs.RestoreFiles, InFlight: vfs.RestoreInFlight})
		done <- err
	}()
	<-g.blocked
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled restore answered %v", err)
	}
	got := r.tree(t, "/out")
	for name, kind := range got {
		if strings.Contains(name, ".tailraid-restore-") {
			t.Fatalf("a temporary file is left: %s", name)
		}
		if strings.HasPrefix(kind, "file ") {
			if want, ok := r.want["/t"+name]; !ok || kind != fmt.Sprintf("file %x", want) {
				t.Fatalf("%s is not the snapshot's whole file", name)
			}
		}
	}
	if _, ok := got["/a.bin"]; ok {
		t.Fatal("the file being written when the restore was cancelled has its name")
	}
}
