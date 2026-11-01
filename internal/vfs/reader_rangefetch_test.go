package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// rangeCS is the chunk size of the range-fetch tests: with k = 2 a data shard is two
// 64 KiB blocks, so a range fetch of a few KiB moves one block, not the shard.
const rangeCS = 4 * chunk.BlockSize

// rangeFabric is a 4-node 2+1 fabric at rangeCS with one chunk of pattern seed written,
// its sums recorded as block roots.
func rangeFabric(t *testing.T, seed byte) (*testharness.Fabric, meta.Inode, meta.Chunk, []byte) {
	t.Helper()
	f := testharness.NewFabricChunk(t, 4, 2, 1, rangeCS)
	f.Ranges = true
	data := testharness.Pattern(rangeCS, seed)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	c, ok := f.FSM.GetChunk(in.Chunks[0])
	if !ok || len(c.Sums) != int(c.K) || c.SumsKind != meta.SumsBlocks {
		t.Fatalf("chunk without roots: %+v", c)
	}
	return f, in, c, data
}

// allGets counts the whole-shard GETs issued to every node of f.
func allGets(f *testharness.Fabric) int {
	n := 0
	for _, t := range f.Targets() {
		n += f.Gets(t)
	}
	return n
}

// The first READ of a few KiB in a cold chunk fetches one block of one data shard from
// its holder and nothing else: no whole shard, no decode, no cache fill, the bytes exact.
// Invariant: I-424
func TestReader_RangeFetch_FirstTouchBlocksOnly(t *testing.T) {
	f, in, c, data := rangeFabric(t, 31)
	rd := f.Reader()
	buf := make([]byte, 4096)
	off := chunk.BlockSize + 100 // the second block of data shard 0
	_, err := rd.ReadAtEarly(ctx, in, buf, int64(off))
	must(t, err)
	if !bytes.Equal(buf, data[off:off+4096]) {
		t.Fatalf("range differs at %d", testharness.FirstDiff(buf, data[off:off+4096]))
	}
	if got := f.RangeGets(c.Placement[0]); got != 1 {
		t.Fatalf("%d range GETs to the holder of shard 0, want 1", got)
	}
	if got := f.RangeBytes(); got != chunk.BlockSize {
		t.Fatalf("%d bytes moved by range GETs, want one block of %d", got, chunk.BlockSize)
	}
	if got := allGets(f); got != 0 {
		t.Fatalf("%d whole-shard GETs, want none", got)
	}
	if n := rd.ReadsRanged(); n != 1 {
		t.Fatalf("reads_ranged %d, want 1", n)
	}
	if rd.Fetching() != 0 || cached(f, c.Hash) {
		t.Fatal("a range fetch started a chunk fetch or filled the cache")
	}
}

// The chunk's next READ reads it whole: shards are fetched, the chunk is cached, and a
// third READ moves nothing; afterwards a range of it is a range fetch again only once
// the cache no longer holds it.
// Invariant: I-424
func TestReader_RangeFetch_SecondTouchReadsChunk(t *testing.T) {
	f, in, c, data := rangeFabric(t, 32)
	rd := f.Reader()
	buf := make([]byte, 4096)
	_, err := rd.ReadAtEarly(ctx, in, buf, 0)
	must(t, err)
	_, err = rd.ReadAtEarly(ctx, in, buf, 8192) // the second touch
	must(t, err)
	if !bytes.Equal(buf, data[8192:8192+4096]) {
		t.Fatal("the second range differs")
	}
	if got := f.RangeGets(c.Placement[0]); got != 1 {
		t.Fatalf("%d range GETs, want 1: the second touch reads the chunk", got)
	}
	testharness.Eventually(t, func() bool { return rd.Fetching() == 0 }, "the chunk fetch completes")
	if !cached(f, c.Hash) {
		t.Fatal("the chunk fetch did not cache the chunk")
	}
	if got := allGets(f); got < int(c.K) {
		t.Fatalf("%d whole-shard GETs, want at least k=%d", got, c.K)
	}
	gets := allGets(f)
	_, err = rd.ReadAtEarly(ctx, in, buf, 16384)
	must(t, err)
	if allGets(f) != gets || f.RangeGets(c.Placement[0]) != 1 {
		t.Fatal("a cached chunk's READ moved shards")
	}
	if n := rd.ReadsRanged(); n != 1 {
		t.Fatalf("reads_ranged %d, want 1", n)
	}
}

// A range across two data shards fetches a slice from each holder, the bytes exact at
// the boundary.
// Invariant: I-424
func TestReader_RangeFetch_CrossesShards(t *testing.T) {
	f, in, c, data := rangeFabric(t, 33)
	s := ec.ShardLen(rangeCS, int(c.K))
	rd := f.Reader()
	buf := make([]byte, 200)
	_, err := rd.ReadAtEarly(ctx, in, buf, int64(s-100))
	must(t, err)
	if !bytes.Equal(buf, data[s-100:s+100]) {
		t.Fatalf("range differs at %d", testharness.FirstDiff(buf, data[s-100:s+100]))
	}
	if a, b := f.RangeGets(c.Placement[0]), f.RangeGets(c.Placement[1]); a != 1 || b != 1 {
		t.Fatalf("range GETs %d and %d, want one to each data holder", a, b)
	}
	if got := f.RangeBytes(); got != 2*chunk.BlockSize {
		t.Fatalf("%d bytes moved, want the last block of shard 0 and the first of shard 1", got)
	}
	if allGets(f) != 0 {
		t.Fatal("a whole shard was fetched")
	}
}

// wrongSlice is a holder that answers a range with one byte changed: the slice fails its
// table, the slot is reported bad and the range reads the chunk.
type wrongSlice struct {
	chunk.ShardClient
	node meta.NodeIdx
}

func (w *wrongSlice) GetRange(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, off, n int64) (chunk.Slice, error) {
	s, err := w.ShardClient.GetRange(ctx, node, key, off, n)
	if err == nil && node == w.node && len(s.Data) > 0 {
		s.Data = append([]byte(nil), s.Data...)
		s.Data[0] ^= 1
	}
	return s, err
}

// A slice that fails its table and a corrupt shard each report the slot bad and fall
// back to the chunk, which decodes from the other holders; the bytes are exact.
// Invariant: I-424
func TestReader_RangeFetch_BadBlockFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *testharness.Fabric, c meta.Chunk) chunk.ShardClient
	}{
		{"a slice that fails its table", func(f *testharness.Fabric, c meta.Chunk) chunk.ShardClient {
			return &wrongSlice{ShardClient: f, node: c.Placement[0]}
		}},
		{"a corrupt shard", func(f *testharness.Fabric, c meta.Chunk) chunk.ShardClient {
			f.CorruptShard(c.Placement[0], chunk.Key(c.Hash, c.K, c.M, 0))
			return f
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, in, c, data := rangeFabric(t, 34)
			rd := vfs.NewReader(f.FSM, f, tc.setup(f, c), f.Cache, nil)
			var bad []meta.NodeIdx
			rd.BadSlot = func(_ chunk.ShardKey, n meta.NodeIdx) { bad = append(bad, n) }
			buf := make([]byte, 4096)
			_, err := rd.ReadAtEarly(ctx, in, buf, 0)
			must(t, err)
			if !bytes.Equal(buf, data[:4096]) {
				t.Fatal("the range differs")
			}
			if len(bad) == 0 || bad[0] != c.Placement[0] {
				t.Fatalf("bad slots %v, want the holder of shard 0", bad)
			}
			if allGets(f) == 0 {
				t.Fatal("the range did not fall back to the chunk")
			}
			testharness.Eventually(t, func() bool { return rd.Fetching() == 0 }, "the chunk fetch completes")
			if n := rd.ReadsRanged(); n != 0 {
				t.Fatalf("reads_ranged %d, want 0", n)
			}
		})
	}
}

// failRange is a holder whose range GETs fail: no slot is bad, the range reads the chunk.
type failRange struct{ chunk.ShardClient }

func (failRange) GetRange(context.Context, meta.NodeIdx, chunk.ShardKey, int64, int64) (chunk.Slice, error) {
	return chunk.Slice{}, errors.New("range refused")
}

// A chunk with whole-shard sums, a dead data holder and a failed range GET each read the
// chunk as before, no slot reported bad and the bytes exact.
// Invariant: I-424
func TestReader_RangeFetch_FallsBackTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *testharness.Fabric, c meta.Chunk) chunk.ShardClient
		write func(f *testharness.Fabric, data []byte) meta.Ino
	}{
		{"whole-shard sums", nil, func(f *testharness.Fabric, data []byte) meta.Ino {
			ino := f.Create("b")
			must(t, f.Ov.Write(ino, 0, 0, data))
			fl := f.Flusher()
			fl.MinCodec = func() uint8 { return meta.CodecBlockRoots - 1 } // a member still reads 17
			must(t, fl.FlushInode(context.Background(), ino))
			return ino
		}},
		{"a dead data holder", func(f *testharness.Fabric, c meta.Chunk) chunk.ShardClient {
			f.SetDown(c.Placement[0], true)
			return f
		}, nil},
		{"a failed range GET", func(f *testharness.Fabric, c meta.Chunk) chunk.ShardClient { return failRange{f} }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, in, c, data := rangeFabric(t, 35)
			if tc.write != nil {
				in = f.Inode(tc.write(f, testharness.Pattern(rangeCS, 36)))
				data = testharness.Pattern(rangeCS, 36)
				c, _ = f.FSM.GetChunk(in.Chunks[0])
				if c.SumsKind != meta.SumsShard || len(c.Sums) != int(c.K) {
					t.Fatalf("the chunk carries kind %d with %d sums", c.SumsKind, len(c.Sums))
				}
			}
			var client chunk.ShardClient = f
			if tc.setup != nil {
				client = tc.setup(f, c)
			}
			rd := vfs.NewReader(f.FSM, f, client, f.Cache, nil)
			var bad []meta.NodeIdx
			rd.BadSlot = func(_ chunk.ShardKey, n meta.NodeIdx) { bad = append(bad, n) }
			buf := make([]byte, 4096)
			_, err := rd.ReadAtEarly(ctx, in, buf, 0)
			must(t, err)
			if !bytes.Equal(buf, data[:4096]) {
				t.Fatal("the range differs")
			}
			if len(bad) != 0 {
				t.Fatalf("slots reported bad: %v", bad)
			}
			if n := rd.ReadsRanged(); n != 0 {
				t.Fatalf("reads_ranged %d, want 0", n)
			}
			testharness.Eventually(t, func() bool { return rd.Fetching() == 0 }, "the chunk fetch completes")
		})
	}
}

// A range of a chunk whose fetch is in flight joins it and is served from the verified
// shard as before: no range GET.
// Invariant: I-424, I-202
func TestReader_RangeFetch_JoinsFetchInFlight(t *testing.T) {
	f, in, c, data := rangeFabric(t, 37)
	entered, release := f.StallGet(c.Placement[1])
	rd := f.Reader()
	whole, werrs := make(chan []byte, 1), make(chan error, 1)
	go func() {
		b, err := rd.ReadChunk(ctx, c.Hash)
		whole <- b
		werrs <- err
	}()
	<-entered // the chunk fetch holds shard 1
	buf := make([]byte, 4096)
	_, err := rd.ReadAtEarly(ctx, in, buf, 0) // inside shard 0, which arrived
	must(t, err)
	if !bytes.Equal(buf, data[:4096]) {
		t.Fatal("the range differs")
	}
	if f.RangeGets(c.Placement[0]) != 0 || rd.ReadsRanged() != 0 || rd.ReadsEarly() != 1 {
		t.Fatalf("range GETs %d, reads_ranged %d, reads_early %d: the READ did not join the fetch", f.RangeGets(c.Placement[0]), rd.ReadsRanged(), rd.ReadsEarly())
	}
	release()
	must(t, <-werrs)
	if !bytes.Equal(<-whole, data) {
		t.Fatal("the whole chunk differs")
	}
}

// A client READ served by a range fetch starts no readahead; the chunk's next READ
// reads it whole and readahead follows from there.
// Invariant: I-424, I-203
func TestFile_ReadAt_RangeFetchStartsNoReadahead(t *testing.T) {
	f := testharness.NewFabricChunk(t, 4, 2, 1, rangeCS)
	f.Ranges = true
	data := testharness.Pattern(8*rangeCS, 38)
	in := f.Inode(writeAndFlush(t, f, "a", data))
	rd := f.Reader()
	fs := vfs.New(f.FSM, f, f.Ov, rd, f.Clk, 501, 20)
	file, err := fs.Open("/a")
	must(t, err)
	defer file.Close()
	buf := make([]byte, 4096)
	_, err = file.ReadAt(buf, 0)
	must(t, err)
	if !bytes.Equal(buf, data[:4096]) {
		t.Fatal("the head differs")
	}
	if rd.Prefetching() != 0 || allGets(f) != 0 {
		t.Fatalf("a range-served READ started readahead: %d prefetching, %d GETs", rd.Prefetching(), allGets(f))
	}
	if fetched, _ := rd.ReadaheadStats(); fetched != 0 {
		t.Fatalf("readahead fetched %d chunks after a range-served READ", fetched)
	}
	_, err = file.ReadAt(buf, 4096) // the second touch: the chunk, then readahead
	must(t, err)
	depth := uint64(vfs.ReadaheadDepth(rangeCS))
	testharness.Eventually(t, func() bool {
		fetched, _ := rd.ReadaheadStats()
		return fetched == depth && rd.Prefetching() == 0
	}, "readahead fetched the next chunks")
	for i := uint64(1); i <= depth; i++ {
		if !cached(f, in.Chunks[i]) {
			t.Fatalf("chunk %d not prefetched", i)
		}
	}
}
