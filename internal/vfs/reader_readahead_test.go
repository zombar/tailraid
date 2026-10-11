package vfs_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// drained waits for every prefetch of rd to finish before the test ends (goleak, and
// the temp dirs the cache writes into).
func drained(t *testing.T, rd *vfs.Reader) {
	t.Helper()
	t.Cleanup(func() {
		testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches drained")
	})
}

// gets sums the shard GETs every target of f has served.
func gets(f *testharness.Fabric) int {
	n := 0
	for _, tgt := range f.Targets() {
		n += f.Gets(tgt)
	}
	return n
}

// readChunk reads chunk idx of in through rd the way File.ReadAt does: readahead first.
func readChunk(t *testing.T, rd *vfs.Reader, in meta.Inode, idx int) []byte {
	t.Helper()
	cs := int(testharness.ChunkSize)
	buf := make([]byte, cs)
	rd.Prefetch(in, int64(idx*cs))
	if _, err := rd.ReadAt(ctx, in, buf, int64(idx*cs)); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return buf
}

func cached(f *testharness.Fabric, h meta.Hash) bool {
	_, ok := f.Cache.Get(h)
	return ok
}

// A sequential READ prefetches the next ReadAhead chunks into the cache and nothing
// beyond them; the window slides with the reads, each new position costing one more
// chunk, and a READ that finds its chunk prefetched counts as served by readahead.
// Invariant: I-166
func TestReader_Readahead_SequentialFillsCache(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(16*cs, 7)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	rd := f.Reader()
	drained(t, rd)
	k := 2

	got := readChunk(t, rd, in, 0)
	if !bytes.Equal(got, data[:cs]) {
		t.Fatalf("chunk 0 differs at %d", testharness.FirstDiff(got, data[:cs]))
	}
	testharness.Eventually(t, func() bool {
		for i := 1; i <= vfs.ReadAhead; i++ {
			if !cached(f, in.Chunks[i]) {
				return false
			}
		}
		return rd.Prefetching() == 0
	}, "chunks 1..ReadAhead prefetched")
	if cached(f, in.Chunks[vfs.ReadAhead+1]) {
		t.Fatalf("chunk %d was fetched beyond the window", vfs.ReadAhead+1)
	}
	if n := gets(f); n != (vfs.ReadAhead+1)*k {
		t.Fatalf("%d GETs after the first read, want %d", n, (vfs.ReadAhead+1)*k)
	}
	fetched, joined := rd.ReadaheadStats()
	if fetched != uint64(vfs.ReadAhead) || joined != 0 {
		t.Fatalf("stats after the first read: fetched %d joined %d", fetched, joined)
	}

	got = readChunk(t, rd, in, 1) // sequential: served from the cache, the window slides by one
	if !bytes.Equal(got, data[cs:2*cs]) {
		t.Fatalf("chunk 1 differs at %d", testharness.FirstDiff(got, data[cs:2*cs]))
	}
	testharness.Eventually(t, func() bool {
		return cached(f, in.Chunks[vfs.ReadAhead+1]) && rd.Prefetching() == 0
	}, "the window slid by one chunk")
	if n := gets(f); n != (vfs.ReadAhead+2)*k {
		t.Fatalf("%d GETs after the second read, want %d", n, (vfs.ReadAhead+2)*k)
	}
	if fetched, joined = rd.ReadaheadStats(); fetched != uint64(vfs.ReadAhead+1) || joined != 1 {
		t.Fatalf("stats after the second read: fetched %d joined %d", fetched, joined)
	}
}

// A READ that does not follow the inode's previous READ prefetches nothing: random
// access must not fill the cache with chunks nobody asked for.
// Invariant: I-166
func TestReader_Readahead_RandomReadDoesNotPrefetch(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(16*cs, 8)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	rd := f.Reader()
	drained(t, rd)
	for _, idx := range []int{6, 2, 9} {
		got := readChunk(t, rd, in, idx)
		if !bytes.Equal(got, data[idx*cs:(idx+1)*cs]) {
			t.Fatalf("chunk %d differs", idx)
		}
		if rd.Prefetching() != 0 {
			t.Fatalf("a prefetch is in flight after a random read of chunk %d", idx)
		}
	}
	if n := gets(f); n != 3*2 {
		t.Fatalf("%d GETs for three random reads, want %d", n, 3*2)
	}
	if fetched, _ := rd.ReadaheadStats(); fetched != 0 {
		t.Fatalf("%d chunks prefetched by random reads", fetched)
	}
}

// countGate blocks every GET until released and counts the GETs that entered.
type countGate struct {
	chunk.ShardClient
	entered atomic.Int32
	release chan struct{}
}

func (g *countGate) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	g.entered.Add(1)
	<-g.release
	return g.ShardClient.Get(ctx, node, key)
}

// At most ReadAhead prefetches run at once, gateway-wide: a sequential READ that
// finds every slot busy skips its candidates rather than queueing them.
// Invariant: I-166
func TestReader_Readahead_BoundedInFlight(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(16*cs, 9)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	g := &countGate{ShardClient: f, release: make(chan struct{})}
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	drained(t, rd)
	k := 2

	rd.Prefetch(in, 0) // chunk 0 is sequential: chunks 1..ReadAhead start
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) == vfs.ReadAhead*k }, "every slot busy")
	if n := rd.Prefetching(); n != vfs.ReadAhead {
		t.Fatalf("%d prefetches in flight, want %d", n, vfs.ReadAhead)
	}
	rd.Prefetch(in, int64(cs)) // sequential again: chunk ReadAhead+1 wants a slot and finds none
	if n := rd.Prefetching(); n != vfs.ReadAhead {
		t.Fatalf("%d prefetches in flight after a second sequential read, want %d", n, vfs.ReadAhead)
	}
	close(g.release)
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	if n := gets(f); n != vfs.ReadAhead*k {
		t.Fatalf("%d GETs, want %d: the skipped candidate was fetched", n, vfs.ReadAhead*k)
	}
	if cached(f, in.Chunks[vfs.ReadAhead+1]) {
		t.Fatalf("chunk %d was queued behind the busy slots", vfs.ReadAhead+1)
	}
}

// A client READ that reaches a chunk a prefetch is fetching joins that fetch: the
// bytes come back, the chunk costs k GETs in total, and the READ counts as served by
// readahead.
// Invariant: I-166
func TestReader_Readahead_ForegroundJoinsPrefetch(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(16*cs, 10)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	g := &countGate{ShardClient: f, release: make(chan struct{})}
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	drained(t, rd)
	k := 2

	rd.Prefetch(in, 0)
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) == vfs.ReadAhead*k }, "prefetches in flight")
	done := make(chan []byte, 1)
	errs := make(chan error, 1)
	go func() {
		b, err := rd.ReadChunk(ctx, in.Chunks[1])
		done <- b
		errs <- err
	}()
	// the foreground read must be waiting on the prefetch, not fetching: no new GET
	// entered the gate, and releasing it serves both
	if n := g.entered.Load(); int(n) != vfs.ReadAhead*k {
		t.Fatalf("%d GETs entered, want %d", n, vfs.ReadAhead*k)
	}
	close(g.release)
	must(t, <-errs)
	if got := <-done; !bytes.Equal(got, data[cs:2*cs]) {
		t.Fatalf("joined read differs at %d", testharness.FirstDiff(got, data[cs:2*cs]))
	}
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	if n := gets(f); n != vfs.ReadAhead*k {
		t.Fatalf("%d GETs, want %d", n, vfs.ReadAhead*k)
	}
	if _, joined := rd.ReadaheadStats(); joined != 1 {
		t.Fatalf("joined %d, want 1", joined)
	}
}

// A hole (HashZero) and a chunk the cache already holds are never prefetched; the
// window still reaches past them.
// Invariant: I-166
func TestReader_Readahead_SkipsHashZeroAndCached(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(8*cs, 11)
	clear(data[2*cs : 3*cs]) // chunk 2 is a hole
	in := f.Inode(writeAndFlush(t, f, "a", data))
	if in.Chunks[2] != meta.HashZero {
		t.Fatal("chunk 2 should be a hole")
	}
	f.Cache.Put(in.Chunks[3], data[3*cs:4*cs]) // chunk 3 is already cached
	rd := f.Reader()
	drained(t, rd)
	k := 2

	readChunk(t, rd, in, 0) // window: 1 (fetch), 2 (hole), 3 (cached), 4 (fetch)
	testharness.Eventually(t, func() bool {
		return cached(f, in.Chunks[1]) && cached(f, in.Chunks[4]) && rd.Prefetching() == 0
	}, "chunks 1 and 4 prefetched")
	if n := gets(f); n != 3*k {
		t.Fatalf("%d GETs, want %d (chunk 0 and two prefetches)", n, 3*k)
	}
	if fetched, _ := rd.ReadaheadStats(); fetched != 2 {
		t.Fatalf("fetched %d, want 2", fetched)
	}
}

// failFor fails every GET of one chunk's shards while armed.
type failFor struct {
	chunk.ShardClient
	mu    sync.Mutex
	hash  string
	armed bool
}

var errInjected = errors.New("injected")

func (c *failFor) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	c.mu.Lock()
	fail := c.armed && strings.HasPrefix(string(key), c.hash)
	c.mu.Unlock()
	if fail {
		return nil, errInjected
	}
	return c.ShardClient.Get(ctx, node, key)
}

// A prefetch that fails leaves nothing behind: the client's own READ succeeds, and its
// later READ of the chunk that failed fetches it normally (the in-flight entry was
// cleared, not poisoned).
// Invariant: I-166
func TestReader_Readahead_FailureIsInvisible(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(8*cs, 12)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	c := &failFor{ShardClient: f, hash: hex.EncodeToString(in.Chunks[1][:]), armed: true}
	rd := vfs.NewReader(f.FSM, f, c, f.Cache, nil)
	drained(t, rd)

	got := readChunk(t, rd, in, 0)
	if !bytes.Equal(got, data[:cs]) {
		t.Fatalf("chunk 0 differs at %d", testharness.FirstDiff(got, data[:cs]))
	}
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	if cached(f, in.Chunks[1]) {
		t.Fatal("the failed prefetch cached something")
	}
	c.mu.Lock()
	c.armed = false
	c.mu.Unlock()
	got = readChunk(t, rd, in, 1)
	if !bytes.Equal(got, data[cs:2*cs]) {
		t.Fatalf("chunk 1 after the failed prefetch differs at %d", testharness.FirstDiff(got, data[cs:2*cs]))
	}
}

// reversePeers ranks the holders backwards: parity before data, far before near.
type reversePeers struct{ *testharness.Fabric }

func (p reversePeers) Rank(nodes []meta.NodeIdx) []meta.NodeIdx {
	out := make([]meta.NodeIdx, len(nodes))
	for i, n := range nodes {
		out[len(nodes)-1-i] = n
	}
	return out
}

// WantGets is the GET count per holder of a healthy read of c (L-63): this host's own
// shard first whatever its slot, then data slots in order until k shards are in hand.
func WantGets(f *testharness.Fabric, c meta.Chunk) map[meta.NodeIdx]int {
	want := map[meta.NodeIdx]int{}
	own := f.Target(f.Self())
	need := int(c.K)
	for _, n := range c.Placement {
		if n == own {
			want[n] = 1
			need--
		}
	}
	for i := 0; need > 0 && i < int(c.K); i++ {
		if n := c.Placement[i]; n != own {
			want[n] = 1
			need--
		}
	}
	return want
}

// Whatever Rank says, this host's own shard and then the data shards are requested
// first when their holders are live: a healthy cluster never pays a network GET for a
// parity shard because its holder answered /health a millisecond sooner.
// Lifecycle: L-63
func TestReader_FetchFirstK_ParityRanksFirst(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 2)
	data := testharness.Pattern(int(testharness.ChunkSize), 13)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	c, _ := f.FSM.GetChunk(in.Chunks[0])
	rd := vfs.NewReader(f.FSM, reversePeers{f}, f, nil, &ec.Pool{})
	got := make([]byte, len(data))
	if _, err := rd.ReadAt(ctx, in, got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
	want := WantGets(f, c)
	for i, n := range c.Placement {
		if g := f.Gets(n); g != want[n] {
			t.Fatalf("slot %d (node %d): %d GETs, want %d; placement %v", i, n, g, want[n], c.Placement)
		}
	}
}

// The per-stream readahead depth is counted in bytes: ReadAheadBytes ahead of the client,
// never more than ReadAhead chunks and never less than one, so a 4 MiB volume prefetches
// two chunks where a 1 MiB volume prefetched four (docs/04 §5).
// Invariant: I-203
func TestReader_Readahead_DepthByBytes_Table(t *testing.T) {
	for _, tc := range []struct {
		cs   uint32
		want int
	}{
		{64 << 10, vfs.ReadAhead},
		{1 << 20, vfs.ReadAhead},
		{2 << 20, vfs.ReadAhead},
		{4 << 20, 2},
		{8 << 20, 1},
		{16 << 20, 1},
	} {
		if got := vfs.ReadaheadDepth(tc.cs); got != tc.want {
			t.Errorf("depth at %d: %d, want %d", tc.cs, got, tc.want)
		}
	}
}

// priorityLog records, per shard key, whether its GET ran as background work, and the
// order the GETs arrived in.
type priorityLog struct {
	chunk.ShardClient
	mu   sync.Mutex
	bg   map[string]bool
	keys []string
}

func (p *priorityLog) Get(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	p.mu.Lock()
	if p.bg == nil {
		p.bg = map[string]bool{}
	}
	p.bg[string(key)] = chunk.IsBackground(ctx)
	p.keys = append(p.keys, string(key))
	p.mu.Unlock()
	return p.ShardClient.Get(ctx, node, key)
}

// A prefetch's shard GETs carry the background mark, so a holder serves a client's own
// GET before them (I-175); the client's foreground GETs never carry it.
// Invariant: I-203
func TestReader_Readahead_PrefetchIsBackgroundPriority(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(8*cs, 14)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	p := &priorityLog{ShardClient: f}
	rd := vfs.NewReader(f.FSM, f, p, f.Cache, nil)
	drained(t, rd)
	readChunk(t, rd, in, 0)
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, bg := range p.bg {
		fg := strings.HasPrefix(key, hex.EncodeToString(in.Chunks[0][:]))
		if fg == bg {
			t.Fatalf("GET %s background=%v: the client's own GETs are foreground, prefetches background", key[:16], bg)
		}
	}
}

// A READ that leaves the sequence cancels the inode's prefetches no client has joined:
// the slots free at once and nothing the client moved away from is fetched or cached.
// Invariant: I-203
func TestReader_Readahead_RandomReadCancelsUnjoinedPrefetch(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(16*cs, 15)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	g := &countGate{ShardClient: f, release: make(chan struct{})}
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	drained(t, rd)
	k := 2
	rd.Prefetch(in, 0)
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) == vfs.ReadAhead*k }, "prefetches in flight")
	rd.Prefetch(in, int64(9*cs)) // the client seeked: chunks 1..ReadAhead are no longer wanted
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "cancelled prefetches gone while their GETs still hang")
	if n := rd.ReadaheadCancelled(); n != uint64(vfs.ReadAhead) {
		t.Fatalf("cancelled %d, want %d", n, vfs.ReadAhead)
	}
	close(g.release)
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) == vfs.ReadAhead*k }, "no further GET")
	if cached(f, in.Chunks[1]) {
		t.Fatal("a cancelled prefetch cached its chunk")
	}
	if fetched, _ := rd.ReadaheadStats(); fetched != 0 {
		t.Fatalf("fetched %d after cancellation", fetched)
	}
}

// A client reads a chunk in many READs (32 KiB to 1 MiB each, a chunk is 4 MiB): a READ
// in the chunk of the previous one stays in the sequence and cancels nothing, at chunk 0
// and past it. Every READ after the first in a chunk had cancelled the prefetches the
// previous chunk had started, and a sequential read of 64 chunks was served 47 times
// from readahead instead of 59 (the soak's phase 17).
// Regression: I-203
func TestReader_Readahead_ReadInSameChunkKeepsPrefetches(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(16*cs, 18)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	g := &countGate{ShardClient: f, release: make(chan struct{})}
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	drained(t, rd)
	k := 2
	rd.Prefetch(in, 0)
	rd.Prefetch(in, int64(cs/2)) // a second READ in chunk 0
	rd.Prefetch(in, int64(cs))   // chunk 1: next in sequence
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) == vfs.ReadAhead*k }, "prefetches in flight")
	for _, off := range []int64{int64(cs) + 1, int64(cs + cs/2), int64(2*cs - 1)} { // more READs in chunk 1
		rd.Prefetch(in, off)
	}
	if n := rd.ReadaheadCancelled(); n != 0 {
		t.Fatalf("READs inside the chunk of the previous READ cancelled %d prefetches, want none", n)
	}
	close(g.release)
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	if fetched, _ := rd.ReadaheadStats(); fetched == 0 {
		t.Fatal("no prefetch completed")
	}
}

// A READ in the chunk of the previous one retries the candidates its chunk's first READ
// skipped because every slot was busy: an early-served READ returns while its chunk's
// fetch still holds a slot, so the first READ of a chunk often finds them taken, and a
// candidate left until the next chunk's first READ is one the client fetches itself.
// Regression: I-166
func TestReader_Readahead_SameChunkReadRetriesSkippedCandidates(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	busy := f.Inode(writeAndFlush(t, f, "busy", testharness.Pattern(16*cs, 19)))
	in := f.Inode(writeAndFlush(t, f, "a", testharness.Pattern(16*cs, 20)))
	g := &countGate{ShardClient: f, release: make(chan struct{})}
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	drained(t, rd)
	k := 2
	rd.Prefetch(busy, 0) // another stream takes every slot
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) == vfs.ReadAhead*k }, "every slot busy")
	rd.Prefetch(in, 0) // the chunk's first READ: every candidate skipped
	if n := rd.Prefetching(); n != vfs.ReadAhead {
		t.Fatalf("%d prefetches in flight, want only the other stream's %d", n, vfs.ReadAhead)
	}
	if n := rd.ReadaheadSkipped(); n == 0 {
		t.Fatal("a skip with every slot busy was not counted")
	}
	close(g.release)
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "the other stream's prefetches done")
	rd.Prefetch(in, int64(cs/2)+7) // a later READ in chunk 0
	testharness.Eventually(t, func() bool {
		fetched, _ := rd.ReadaheadStats()
		return fetched == uint64(2*vfs.ReadaheadDepth(uint32(cs)))
	},
		"the skipped candidates fetched on the later READ")
}

// A prefetch a client has joined is not cancelled by that client's seek elsewhere: the
// joined read completes from the one fetch, and only the unjoined prefetches are cut.
// Invariant: I-203
func TestReader_Readahead_JoinedPrefetchSurvivesCancel(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(16*cs, 16)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	g := &countGate{ShardClient: f, release: make(chan struct{})}
	rd := vfs.NewReader(f.FSM, f, g, f.Cache, nil)
	drained(t, rd)
	k := 2
	rd.Prefetch(in, 0)
	testharness.Eventually(t, func() bool { return int(g.entered.Load()) == vfs.ReadAhead*k }, "prefetches in flight")
	done := make(chan []byte, 1)
	errs := make(chan error, 1)
	go func() {
		b, err := rd.ReadChunk(ctx, in.Chunks[1])
		done <- b
		errs <- err
	}()
	testharness.Eventually(t, func() bool { _, joined := rd.ReadaheadStats(); return joined == 1 }, "the client joined chunk 1's prefetch")
	rd.Prefetch(in, int64(9*cs))
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 1 }, "only the joined prefetch remains")
	if n := rd.ReadaheadCancelled(); n != uint64(vfs.ReadAhead-1) {
		t.Fatalf("cancelled %d, want %d", n, vfs.ReadAhead-1)
	}
	close(g.release)
	must(t, <-errs)
	if got := <-done; !bytes.Equal(got, data[cs:2*cs]) {
		t.Fatalf("joined read differs at %d", testharness.FirstDiff(got, data[cs:2*cs]))
	}
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
}

// File.ReadAt starts the readahead after its own chunk is in hand, never alongside it:
// every GET of the client's chunk precedes the first GET of a prefetch, so the bytes the
// client waits for are not queued behind the ones it may want later.
// Invariant: I-203
func TestFile_ReadAt_PrefetchStartsAfterTheRead(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(8*cs, 17)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	p := &priorityLog{ShardClient: f}
	rd := vfs.NewReader(f.FSM, f, p, f.Cache, nil)
	drained(t, rd)
	fs := vfs.New(f.FSM, f, f.Ov, rd, f.Clk, 501, 20)
	file, err := fs.Open("/a")
	must(t, err)
	defer file.Close()
	buf := make([]byte, 32<<10)
	if _, err := file.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	testharness.Eventually(t, func() bool { return rd.Prefetching() == 0 }, "prefetches done")
	own := hex.EncodeToString(in.Chunks[0][:])
	p.mu.Lock()
	defer p.mu.Unlock()
	k := 2
	for i, key := range p.keys {
		if strings.HasPrefix(key, own) != (i < k) {
			t.Fatalf("GET order %v: the client's %d GETs must come first", p.keys, k)
		}
	}
}
