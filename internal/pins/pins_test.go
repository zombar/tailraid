package pins_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/pins"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var ctx = context.Background()

// rig is a fabric with its own small cache and a reader over it, as one gateway sees the
// cluster: the fabric's own cache stays cold so every chunk read either comes from this
// cache or crosses the (fake) network.
type rig struct {
	t     *testing.T
	f     *testharness.Fabric
	dir   string
	cache *chunk.Cache
	rd    *vfs.Reader
	p     *pins.Pins
}

func newRig(t *testing.T, cacheMax int64) *rig {
	t.Helper()
	f := testharness.NewFabric(t, 3, 2, 1)
	dir := t.TempDir()
	c, err := chunk.OpenCache(sysfs.OS{}, filepath.Join(dir, "cache"), cacheMax, f.Clk)
	if err != nil {
		t.Fatal(err)
	}
	rd := vfs.NewReader(f.FSM, f, f, c, &ec.Pool{})
	p, err := pins.Open(dir, f.FSM, rd, c, f.Clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, f: f, dir: dir, cache: c, rd: rd, p: p}
}

// mkdir makes a directory under parent.
func (r *rig) mkdir(parent meta.Ino, name string) meta.Ino {
	r.t.Helper()
	return r.f.MustApply(meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: parent, Name: name, Mode: 0o755, Now: 1}}).Ino
}

// file writes data under parent/name through the overlay and flushes it, so its chunks
// are committed and placed on the fabric's stores (and in the fabric's cache, not ours).
func (r *rig) file(parent meta.Ino, name string, data []byte) meta.Ino {
	r.t.Helper()
	ino := r.f.MustApply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: parent, Name: name, Type: meta.TFile, Mode: 0o644, Now: 1}}).Ino
	if err := r.f.Ov.Write(ino, 0, 0, data); err != nil {
		r.t.Fatal(err)
	}
	if err := r.f.Flusher().FlushInode(ctx, ino); err != nil {
		r.t.Fatal(err)
	}
	return ino
}

// chunks lists the chunk hashes of a file.
func (r *rig) chunks(ino meta.Ino) []meta.Hash {
	r.t.Helper()
	return r.f.Inode(ino).Chunks
}

// read reads a whole file through the rig's reader.
func (r *rig) read(ino meta.Ino) ([]byte, error) {
	in := r.f.Inode(ino)
	buf := make([]byte, in.Size)
	n, err := r.rd.ReadAt(ctx, in, buf, 0)
	return buf[:n], err
}

func (r *rig) allCached(ino meta.Ino) bool {
	for _, h := range r.chunks(ino) {
		if !r.cache.Has(h) {
			return false
		}
	}
	return true
}

// A walk fetches every chunk under the pin into the cache and pins it; with every other
// host dead the folder still reads from this device, byte for byte, while a cold reader
// cannot.
//
// Invariant: I-162
func TestPins_PrefetchFillsThenReadsOffline(t *testing.T) {
	r := newRig(t, 1<<30)
	music := r.mkdir(meta.RootIno, "music")
	album := r.mkdir(music, "album")
	var files []meta.Ino
	var datas [][]byte
	for i := range 3 {
		data := testharness.Pattern(2*int(testharness.ChunkSize)+17*i, byte(i+1))
		files = append(files, r.file(album, "t"+string(rune('a'+i)), data))
		datas = append(datas, data)
	}
	other := r.file(meta.RootIno, "other", testharness.Pattern(int(testharness.ChunkSize), 9))
	e, err := r.p.Pin("/music")
	if err != nil {
		t.Fatal(err)
	}
	var want uint64
	for _, ino := range files {
		want += r.f.Inode(ino).Size
	}
	if e.Path != "/music" || e.Bytes != want || e.CachedBytes != 0 {
		t.Fatalf("pin entry %+v, want %d bytes, none cached yet", e, want)
	}
	if err := r.p.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ino := range files {
		if !r.allCached(ino) {
			t.Fatalf("file %d not fully cached after the walk", ino)
		}
	}
	if r.allCached(other) {
		t.Fatal("a file outside the pin was fetched")
	}
	rep := r.p.List()
	if len(rep.Pins) != 1 || rep.Pins[0].CachedBytes != want || rep.PinnedBytes != int64(want) || rep.Size != 1<<30 {
		t.Fatalf("report %+v", rep)
	}
	// every other host dies: the pinned folder still reads from the cache
	for _, n := range []meta.NodeIdx{1, 2, 3} {
		r.f.SetDown(n, true)
	}
	for i, ino := range files {
		got, err := r.read(ino)
		if err != nil || !bytes.Equal(got, datas[i]) {
			t.Fatalf("offline read of file %d: %v, differs at %d", i, err, testharness.FirstDiff(got, datas[i]))
		}
	}
	if _, err := r.read(other); err == nil {
		t.Fatal("an unpinned file read with every host dead")
	}
	cold := vfs.NewReader(r.f.FSM, r.f, r.f, nil, &ec.Pool{})
	in := r.f.Inode(files[0])
	if _, err := cold.ReadAt(ctx, in, make([]byte, in.Size), 0); err == nil {
		t.Fatal("a cold reader read the pinned file with every host dead")
	}
}

// A rename keeps the pin: it is stored by inode, and the listing shows the new path.
//
// Invariant: I-162
func TestPins_RenameKeepsPin(t *testing.T) {
	r := newRig(t, 1<<30)
	music := r.mkdir(meta.RootIno, "music")
	f := r.file(music, "a", testharness.Pattern(100, 1))
	if _, err := r.p.Pin("/music"); err != nil {
		t.Fatal(err)
	}
	if err := r.p.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	r.f.MustApply(meta.Command{Op: meta.OpRename, Rename: &meta.Rename{OldParent: meta.RootIno, OldName: "music", NewParent: meta.RootIno, NewName: "tunes", Now: 2}})
	rep := r.p.List()
	if len(rep.Pins) != 1 || rep.Pins[0].Path != "/tunes" {
		t.Fatalf("after rename: %+v", rep.Pins)
	}
	g := r.file(music, "b", testharness.Pattern(100, 2))
	if err := r.p.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.allCached(f) || !r.allCached(g) {
		t.Fatal("the renamed folder's chunks are not held")
	}
	if err := r.p.Unpin("/tunes"); err != nil {
		t.Fatalf("unpin by the new name: %v", err)
	}
	if len(r.p.List().Pins) != 0 {
		t.Fatal("pin survived the unpin")
	}
}

// An unpin returns the folder's chunks to the LRU: the pinned bytes drop to zero at once
// and the next traffic evicts them down to the budget. A folder that is deleted lapses
// on the next walk the same way. Chunks shared with another pin stay pinned.
//
// Invariant: I-162
func TestPins_UnpinReleases(t *testing.T) {
	cs := int(testharness.ChunkSize)
	r := newRig(t, int64(2*cs)) // two chunks of budget
	a := r.mkdir(meta.RootIno, "a")
	b := r.mkdir(meta.RootIno, "b")
	shared := testharness.Pattern(cs, 5)
	fa := r.file(a, "f", append(testharness.Pattern(3*cs, 1), shared...)) // four chunks, one shared with b
	fb := r.file(b, "f", shared)
	for _, p := range []string{"/a", "/b"} {
		if _, err := r.p.Pin(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.p.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.allCached(fa) || !r.allCached(fb) || r.cache.Pinned() != int64(4*cs) {
		t.Fatalf("after the walk: pinned %d, a cached %v, b cached %v", r.cache.Pinned(), r.allCached(fa), r.allCached(fb))
	}
	if err := r.p.Unpin("/a"); err != nil {
		t.Fatal(err)
	}
	if r.cache.Pinned() != int64(cs) || !r.allCached(fb) {
		t.Fatalf("after unpin of a: pinned %d (want the shared chunk only), b cached %v", r.cache.Pinned(), r.allCached(fb))
	}
	if r.cache.Used()-r.cache.Pinned() > int64(2*cs) {
		t.Fatalf("unpinned bytes %d over the budget", r.cache.Used()-r.cache.Pinned())
	}
	if err := r.p.Unpin("/a"); !errors.Is(err, meta.ErrNotExist) {
		t.Fatalf("second unpin: %v", err)
	}
	// b's folder is trashed: the pin lapses on the next walk and nothing stays pinned
	r.f.MustApply(meta.Command{Op: meta.OpTrash, Trash: &meta.Trash{Parent: meta.RootIno, Name: "b", Recursive: true, Now: 3}})
	if err := r.p.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	if rep := r.p.List(); len(rep.Pins) != 0 || rep.PinnedBytes != 0 {
		t.Fatalf("after the folder's delete: %+v", rep)
	}
	b2, err := os.ReadFile(filepath.Join(r.dir, pins.File))
	if err != nil || !bytes.Equal(bytes.TrimSpace(b2), []byte("[]")) {
		t.Fatalf("pins file after the lapse: %q %v", b2, err)
	}
}

// The pin list survives a restart: a fresh Pins over the same data directory lists the
// pin and its walk refills a fresh cache.
//
// Invariant: I-162
func TestPins_SurviveRestart(t *testing.T) {
	r := newRig(t, 1<<30)
	music := r.mkdir(meta.RootIno, "music")
	f := r.file(music, "a", testharness.Pattern(2*int(testharness.ChunkSize), 1))
	if _, err := r.p.Pin("/music"); err != nil {
		t.Fatal(err)
	}
	c2, err := chunk.OpenCache(sysfs.OS{}, filepath.Join(t.TempDir(), "cache"), 1<<30, r.f.Clk)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := pins.Open(r.dir, r.f.FSM, vfs.NewReader(r.f.FSM, r.f, r.f, c2, &ec.Pool{}), c2, r.f.Clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep := p2.List(); len(rep.Pins) != 1 || rep.Pins[0].Path != "/music" || rep.Pins[0].CachedBytes != 0 {
		t.Fatalf("reopened: %+v", rep)
	}
	if err := p2.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	for _, h := range r.chunks(f) {
		if !c2.Has(h) {
			t.Fatal("the reopened pin did not refill the cache")
		}
	}
	// a corrupt file refuses to open rather than silently forgetting the pins
	if err := os.WriteFile(filepath.Join(r.dir, pins.File), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pins.Open(r.dir, r.f.FSM, r.rd, r.cache, r.f.Clk, nil); err == nil {
		t.Fatal("a corrupt pins file opened")
	}
}

// The worker fetches a file written into a pinned folder after the pin, on its next
// check once the applied index has moved; the root and a file refuse to be pinned.
//
// Invariant: I-162
func TestPins_NewFileInPinnedDirIsFetched(t *testing.T) {
	r := newRig(t, 1<<30)
	music := r.mkdir(meta.RootIno, "music")
	first := r.file(music, "a", testharness.Pattern(100, 1))
	if _, err := r.p.Pin("/"); !errors.Is(err, pins.ErrRoot) {
		t.Fatalf("pin of the root: %v", err)
	}
	if _, err := r.p.Pin("/music/a"); !errors.Is(err, meta.ErrNotDir) {
		t.Fatalf("pin of a file: %v", err)
	}
	if _, err := r.p.Pin("/nope"); !errors.Is(err, meta.ErrNotExist) {
		t.Fatalf("pin of nothing: %v", err)
	}
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.p.Run(wctx); close(done) }()
	if _, err := r.p.Pin("/music"); err != nil {
		t.Fatal(err)
	}
	testharness.Eventually(t, func() bool { return r.allCached(first) }, "the pin's first walk")
	later := r.file(music, "b", testharness.Pattern(3*int(testharness.ChunkSize), 2))
	testharness.Eventually(t, func() bool {
		r.f.Clk.Advance(pins.Interval)
		return r.allCached(later)
	}, "the later file fetched on the worker's check")
	cancel()
	<-done
}

// Every shard request of a walk carries the background mark, so a prefetch of a large
// folder queues behind client reads for a node's slots (docs/06 §4).
//
// Invariant: I-175
func TestPins_WalkFetchesBackground(t *testing.T) {
	r := newRig(t, 1<<30)
	music := r.mkdir(meta.RootIno, "music")
	r.file(music, "a", testharness.Pattern(2*int(testharness.ChunkSize), 1))
	seen := &markingReader{rd: r.rd}
	p, err := pins.Open(t.TempDir(), r.f.FSM, seen, r.cache, r.f.Clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Pin("/music"); err != nil {
		t.Fatal(err)
	}
	if err := p.Walk(ctx); err != nil {
		t.Fatal(err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.calls != 2 || seen.foreground != 0 {
		t.Fatalf("%d chunk reads, %d of them without the background mark; want 2 and 0", seen.calls, seen.foreground)
	}
}

// markingReader counts the reads that reach the reader and those missing the mark.
type markingReader struct {
	rd                pins.Reader
	mu                sync.Mutex
	calls, foreground int
}

func (m *markingReader) ReadChunk(ctx context.Context, h meta.Hash) ([]byte, error) {
	m.mu.Lock()
	m.calls++
	if !chunk.IsBackground(ctx) {
		m.foreground++
	}
	m.mu.Unlock()
	return m.rd.ReadChunk(ctx, h)
}
