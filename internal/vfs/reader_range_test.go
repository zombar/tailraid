package vfs_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// oneChunk writes one full chunk with recorded sums and returns the inode and its chunk.
func oneChunk(t *testing.T, f *testharness.Fabric, name string, seed byte) (meta.Inode, meta.Chunk) {
	t.Helper()
	data := testharness.Pattern(int(testharness.ChunkSize), seed)
	in := f.Inode(writeAndFlush(t, f, name, data))
	c, ok := f.FSM.GetChunk(in.Chunks[0])
	if !ok || len(c.Sums) != int(c.K) {
		t.Fatalf("chunk without sums: %+v", c)
	}
	return in, c
}

// readEarly runs ReadAtEarly on its own goroutine and reports when it returned.
func readEarly(rd *vfs.Reader, in meta.Inode, off, n int) (<-chan []byte, <-chan error) {
	out, errs := make(chan []byte, 1), make(chan error, 1)
	go func() {
		buf := make([]byte, n)
		_, err := rd.ReadAtEarly(ctx, in, buf, int64(off))
		out <- buf
		errs <- err
	}()
	return out, errs
}

// A range inside data shard 0 is served once shard 0 arrived and matched its recorded
// sum, while the other shards are still on their way; the chunk enters the cache only
// after it completes and hashes.
// Invariant: I-202
func TestReader_ReadRange_ServedFromDataShardBeforeDecode(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	in, c := oneChunk(t, f, "a", 21)
	data := testharness.Pattern(int(testharness.ChunkSize), 21)
	s := ec.ShardLen(int(testharness.ChunkSize), 2)
	_, rel1 := f.StallGet(c.Placement[1])
	_, rel2 := f.StallGet(c.Placement[2])
	rd := f.Reader()
	out, errs := readEarly(rd, in, s/2, 4096)
	must(t, <-errs) // returned with shards 1 and 2 still stalled
	if got := <-out; !bytes.Equal(got, data[s/2:s/2+4096]) {
		t.Fatalf("early range differs at %d", testharness.FirstDiff(got, data[s/2:s/2+4096]))
	}
	if n := rd.ReadsEarly(); n != 1 {
		t.Fatalf("reads_early %d, want 1", n)
	}
	if cached(f, c.Hash) {
		t.Fatal("the chunk is cached before it completed")
	}
	rel1()
	rel2()
	testharness.Eventually(t, func() bool { return rd.Fetching() == 0 }, "the fetch completes")
	if !cached(f, c.Hash) {
		t.Fatal("the completed chunk is not cached")
	}
	got, err := rd.ReadChunk(ctx, c.Hash)
	must(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("the whole chunk differs")
	}
}

// A range across two data shards waits for both; a range at a shard boundary is exact.
// Invariant: I-202
func TestReader_ReadRange_StraddlesTwoDataShards(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	in, c := oneChunk(t, f, "a", 22)
	data := testharness.Pattern(int(testharness.ChunkSize), 22)
	s := ec.ShardLen(int(testharness.ChunkSize), 2)
	entered, rel := f.StallGet(c.Placement[1])
	_, rel2 := f.StallGet(c.Placement[2])
	rd := f.Reader()
	out, errs := readEarly(rd, in, s-2048, 4096)
	<-entered // the fetch asked for shard 1 and waits for it
	select {
	case <-errs:
		t.Fatal("a range across two shards returned with the second still stalled")
	default:
	}
	rel()
	rel2()
	must(t, <-errs)
	if got := <-out; !bytes.Equal(got, data[s-2048:s+2048]) {
		t.Fatalf("straddling range differs at %d", testharness.FirstDiff(got, data[s-2048:s+2048]))
	}
}

// A data shard that does not match its recorded sum is a bad slot: it is reported, the
// fetch pulls the next slot, and the range is served from the decoded chunk.
// Invariant: I-202
func TestReader_ReadRange_CorruptDataShard_WaitsForDecode(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	in, c := oneChunk(t, f, "a", 23)
	data := testharness.Pattern(int(testharness.ChunkSize), 23)
	s := ec.ShardLen(int(testharness.ChunkSize), 2)
	key0 := chunk.Key(c.Hash, c.K, c.M, 0)
	wrong := testharness.Pattern(s, 99) // frame-valid, wrong bytes
	if _, err := f.StoreOf(c.Placement[0]).Put(key0, wrong); err != nil {
		t.Fatal(err)
	}
	rd := f.Reader()
	var reported []chunk.ShardKey
	rd.BadSlot = func(key chunk.ShardKey, _ meta.NodeIdx) { reported = append(reported, key) }
	buf := make([]byte, 4096)
	if _, err := rd.ReadAtEarly(ctx, in, buf, int64(s/2)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, data[s/2:s/2+4096]) {
		t.Fatal("range served from the corrupt shard")
	}
	testharness.Eventually(t, func() bool { return rd.Fetching() == 0 }, "the fetch completes")
	if len(reported) != 1 || reported[0] != key0 {
		t.Fatalf("bad slot reports %v, want %s", reported, key0)
	}
	if rd.ReadsEarly() != 0 {
		t.Fatal("a range was served early from a shard that failed its sum")
	}
}

// With the holder of data shard 0 dead, a range inside shard 0 is served from the decode
// of the other shards, exactly.
// Invariant: I-202
func TestReader_ReadRange_DeadDataHolder_WaitsForDecode(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	in, c := oneChunk(t, f, "a", 24)
	data := testharness.Pattern(int(testharness.ChunkSize), 24)
	s := ec.ShardLen(int(testharness.ChunkSize), 2)
	f.SetDown(c.Placement[0], true)
	rd := f.Reader()
	buf := make([]byte, 4096)
	if _, err := rd.ReadAtEarly(ctx, in, buf, int64(s/4)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, data[s/4:s/4+4096]) {
		t.Fatal("range differs with the data holder dead")
	}
	if rd.ReadsEarly() != 0 {
		t.Fatal("counted an early read without the shard")
	}
}

// A chunk committed without sums (a mixed cluster, or before the backfill) is served whole:
// a range inside shard 0 waits for shard 1.
// Invariant: I-202
func TestReader_ReadRange_NoSums_WholeChunk(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	data := testharness.Pattern(int(testharness.ChunkSize), 25)
	ino := f.Create("a")
	must(t, f.Ov.Write(ino, 0, 0, data))
	fl := f.Flusher()
	fl.MinCodec = func() uint8 { return meta.CodecShardSums - 1 }
	must(t, fl.FlushInode(ctx, ino))
	in := f.Inode(ino)
	c, _ := f.FSM.GetChunk(in.Chunks[0])
	if c.Sums != nil {
		t.Fatal("sums recorded in a mixed cluster")
	}
	entered, rel := f.StallGet(c.Placement[1])
	_, rel2 := f.StallGet(c.Placement[2])
	rd := f.Reader()
	out, errs := readEarly(rd, in, 100, 4096)
	<-entered
	select {
	case <-errs:
		t.Fatal("a chunk without sums was served from one shard")
	default:
	}
	rel()
	rel2()
	must(t, <-errs)
	if got := <-out; !bytes.Equal(got, data[100:4196]) {
		t.Fatal("range differs")
	}
	if rd.ReadsEarly() != 0 {
		t.Fatal("an early read was counted without sums")
	}
}

// Every offset and length over a file with a hole and a short last chunk reads exactly
// what ReadAt reads: {0, 1, S−1, S, S+1, cs−1} into the first chunk, the hole, and the
// short chunk's end, through the early path.
// Invariant: I-202
func TestReader_ReadRange_HolesAndShortChunk_Table(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	s := ec.ShardLen(cs, 2)
	data := testharness.Pattern(2*cs+1000, 26)
	clear(data[cs : 2*cs]) // chunk 1 is a hole
	in := f.Inode(writeAndFlush(t, f, "a", data))
	if in.Chunks[1] != meta.HashZero {
		t.Fatal("chunk 1 should be a hole")
	}
	rd := f.Reader()
	offs := []int{0, 1, s - 1, s, s + 1, cs - 1, cs, cs + 5, 2 * cs, 2*cs + 999, 2*cs + 500}
	lens := []int{1, 4096, s, cs + 10, 3 * cs}
	for _, off := range offs {
		for _, n := range lens {
			want := make([]byte, n)
			copied := 0
			if off < len(data) {
				copied = copy(want, data[off:])
			}
			got := make([]byte, n)
			rn, err := rd.ReadAtEarly(ctx, in, got, int64(off))
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("off %d len %d: %v", off, n, err)
			}
			if rn != copied {
				t.Fatalf("off %d len %d: read %d bytes, want %d", off, n, rn, copied)
			}
			if !bytes.Equal(got[:rn], want[:copied]) {
				t.Fatalf("off %d len %d: differs at %d", off, n, testharness.FirstDiff(got[:rn], want[:copied]))
			}
		}
	}
}

// A chunk whose k shards each match their recorded sums but decode to the wrong hash: a
// range inside shard 0 is served from that verified shard, a reader of the whole chunk
// (ReadChunk) gets EIO, E-134 is recorded and the chunk never enters the cache.
// Invariant: I-40, I-202
func TestReader_ReadRange_ChunkHashMismatch_LateReadersEIO_EventRecorded(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(cs, 27)
	wrong := sha256.Sum256([]byte("not the data"))
	codec, err := ec.New(2, 1)
	must(t, err)
	shards, err := codec.Encode(data)
	must(t, err)
	sums := []meta.Hash{sha256.Sum256(shards[0]), sha256.Sum256(shards[1])}
	holders := []meta.NodeIdx{f.Target(1), f.Target(2), f.Target(3)}
	for j, n := range holders {
		must(t, f.Put(ctx, n, chunk.Key(wrong, 2, 1, j), shards[j]))
	}
	ino := f.Create("x")
	f.MustApply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{
		Ino: ino, NewSize: uint64(cs), Writes: []meta.ChunkWrite{{Idx: 0, Hash: wrong}},
		NewChunks: []meta.ChunkDesc{{Hash: wrong, Size: uint32(cs), K: 2, M: 1, Placement: holders, Sums: sums}}, Now: 2,
	}})
	in := f.Inode(ino)
	rd := f.Reader()
	rd.Events = meta.NewEventRing(f.Clk, f.SelfIdx)
	buf := make([]byte, 4096)
	if _, err := rd.ReadAtEarly(ctx, in, buf, 100); err != nil {
		t.Fatalf("early range inside a verified shard: %v", err)
	}
	if !bytes.Equal(buf, data[100:4196]) {
		t.Fatal("early range differs from the verified shard")
	}
	testharness.Eventually(t, func() bool { return rd.Fetching() == 0 }, "the fetch completes")
	if _, err := rd.ReadChunk(ctx, wrong); !errors.Is(err, vfs.ErrIO) {
		t.Fatalf("whole-chunk read: %v, want ErrIO", err)
	}
	if !slices.Contains(rd.Events.Kinds(wrong), meta.EvReadBadChunk) {
		t.Fatalf("events %v lack %s", rd.Events.Kinds(wrong), meta.EvReadBadChunk)
	}
	if cached(f, wrong) {
		t.Fatal("a chunk that failed its hash entered the cache")
	}
	// a range covering both data shards is served from the two verified shards as well:
	// they are exactly the bytes a decode would concatenate, each checked against Raft
	whole := make([]byte, cs)
	if _, err := rd.ReadAtEarly(ctx, in, whole, 0); err != nil || !bytes.Equal(whole, data) {
		t.Fatalf("a whole-chunk range through the early path: %v", err)
	}
}

// The flusher's base reads and ReadAt never return early: with shard 1 stalled they wait.
// Invariant: I-202
func TestReader_FlusherBaseRead_NeverEarly(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	in, c := oneChunk(t, f, "a", 28)
	entered, rel := f.StallGet(c.Placement[1])
	_, rel2 := f.StallGet(c.Placement[2])
	rd := f.Reader()
	errs := make(chan error, 2)
	go func() { errs <- rd.BaseReader(ctx, in)(100, make([]byte, 4096)) }()
	go func() { _, err := rd.ReadAt(ctx, in, make([]byte, 4096), 100); errs <- err }()
	<-entered
	select {
	case <-errs:
		t.Fatal("a strict read returned before the chunk completed")
	default:
	}
	rel()
	rel2()
	must(t, <-errs)
	must(t, <-errs)
	if rd.ReadsEarly() != 0 {
		t.Fatal("a strict read was counted as early")
	}
}

// Whatever the offset, length and the dead holder, the early path reads what ReadAt reads.
// Invariant: I-202
func FuzzReadRange(fz *testing.F) {
	fz.Add(uint32(0), uint16(1), uint8(0))
	fz.Add(uint32(32767), uint16(4096), uint8(1))
	fz.Add(uint32(65535), uint16(100), uint8(2))
	fz.Add(uint32(70000), uint16(9000), uint8(3))
	fz.Fuzz(func(t *testing.T, off uint32, n uint16, dead uint8) {
		f := testharness.NewFabric(t, 4, 2, 1)
		cs := int(testharness.ChunkSize)
		data := testharness.Pattern(cs+1000, 29)
		in := f.Inode(writeAndFlush(t, f, "a", data))
		c, _ := f.FSM.GetChunk(in.Chunks[0])
		if dead%4 < 3 {
			f.SetDown(c.Placement[dead%4], true)
		}
		rd := f.Reader()
		got := make([]byte, int(n)+1)
		want := make([]byte, int(n)+1)
		rn, err := rd.ReadAtEarly(ctx, in, got, int64(off))
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		wn, werr := f.ReaderNoCache().ReadAt(ctx, in, want, int64(off))
		if werr != nil && !errors.Is(werr, io.EOF) {
			t.Fatal(werr)
		}
		if rn != wn || !bytes.Equal(got[:rn], want[:wn]) {
			t.Fatalf("off %d len %d: early %d bytes, strict %d; differ at %d", off, n, rn, wn, testharness.FirstDiff(got[:rn], want[:wn]))
		}
	})
}

// A fetch that outlives the client's read (here the early path's, waiting on a stalled
// shard) is cancelled and waited for by Close, so nothing reaches the cache after the
// reader is closed and the node can drain the cache (a late Put raced the harness's
// Drain, macOS make check 2026-10-12); a closed reader starts no prefetch.
// Invariant: I-204, I-202
func TestReader_Close_CancelsDetachedFetches(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	in, c := oneChunk(t, f, "a", 24)
	s := ec.ShardLen(int(testharness.ChunkSize), 2)
	_, rel1 := f.StallGet(c.Placement[1])
	_, rel2 := f.StallGet(c.Placement[2])
	defer rel1()
	defer rel2()
	rd := f.Reader()
	_, errs := readEarly(rd, in, s/2, 4096)
	must(t, <-errs)
	if n := rd.Fetching(); n != 1 {
		t.Fatalf("fetching %d before close, want 1", n)
	}
	rd.Close()
	if n := rd.Fetching(); n != 0 {
		t.Fatalf("fetching %d after close", n)
	}
	if cached(f, c.Hash) {
		t.Fatal("a fetch cut by Close cached its chunk")
	}
	two := f.Inode(writeAndFlush(t, f, "b", testharness.Pattern(2*int(testharness.ChunkSize), 25)))
	rd.Prefetch(two, 0) // a sequential first READ: chunk 1 would be prefetched
	if n := rd.Prefetching(); n != 0 {
		t.Fatalf("a closed reader prefetches %d", n)
	}
	rd.Close() // idempotent
}
