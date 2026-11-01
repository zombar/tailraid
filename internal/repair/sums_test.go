package repair_test

import (
	"crypto/sha256"
	"slices"
	"strconv"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

// A pass records the data-shard sums of at most SumsBatch chunks that lack them
// ({0, 1, n−1, n, n+1} of n), each after fetching, decoding and verifying the chunk, and
// reports how many still lack them; the recorded sum is the hash of the shard payload the
// holder stores; the next pass takes the rest.
// Invariant: I-201
func TestRepair_BackfillSums_Bounded(t *testing.T) {
	const n = 5
	for _, batch := range []int{0, 1, n - 1, n, n + 1} {
		t.Run(strconv.Itoa(batch), func(t *testing.T) {
			f := testharness.NewFabric(t, 3, 2, 1)
			fl := f.Flusher()
			fl.MinCodec = func() uint8 { return meta.CodecShardSums - 1 } // written before every member read codec 5
			var hashes []meta.Hash
			for i := range n {
				ino := f.Create("f" + strconv.Itoa(i))
				if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), byte(40+i))); err != nil {
					t.Fatal(err)
				}
				if err := fl.FlushInode(ctx, ino); err != nil {
					t.Fatal(err)
				}
				hashes = append(hashes, f.Inode(ino).Chunks[0])
			}
			for _, h := range hashes {
				if c, _ := f.FSM.GetChunk(h); c.Sums != nil {
					t.Fatal("a chunk carries sums before the backfill")
				}
			}
			cfg := repair.DefaultConfig()
			cfg.ScrubPeriod = 0
			cfg.SumsBatch = batch
			r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, cfg, nil)
			st, err := r.Scan(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := min(batch, n)
			if st.SumsRecorded != want || st.SumsPending != n-want {
				t.Fatalf("pass 1: recorded %d pending %d, want %d and %d", st.SumsRecorded, st.SumsPending, want, n-want)
			}
			recorded := 0
			for _, h := range hashes {
				c, _ := f.FSM.GetChunk(h)
				if c.Sums == nil {
					continue
				}
				recorded++
				if len(c.Sums) != int(c.K) {
					t.Fatalf("%d sums for k=%d", len(c.Sums), c.K)
				}
				for j := range int(c.K) {
					payload, err := f.StoreOf(c.Placement[j]).Get(chunk.Key(h, c.K, c.M, j))
					if err != nil {
						t.Fatal(err)
					}
					want := sha256.Sum256(payload)
					if c.SumsKind == meta.SumsBlocks { // every member reads codec 18 here: roots (I-422)
						want = chunk.ShardRoot(payload)
					}
					if c.Sums[j] != want {
						t.Fatalf("recorded sum %d differs from the stored shard's hash", j)
					}
				}
			}
			if recorded != want {
				t.Fatalf("%d chunks carry sums, want %d", recorded, want)
			}
			if batch == 0 {
				return // disabled: nothing more to check
			}
			for st.SumsPending > 0 { // the following passes take the rest
				if st, err = r.Scan(ctx); err != nil {
					t.Fatal(err)
				}
			}
			for _, h := range hashes {
				if c, _ := f.FSM.GetChunk(h); c.Sums == nil {
					t.Fatal("a chunk still lacks sums after the backfill finished")
				}
			}
			if st, err = r.Scan(ctx); err != nil || st.SumsRecorded != 0 || st.SumsPending != 0 {
				t.Fatalf("a pass over recorded chunks: %+v %v", st, err)
			}
		})
	}
}

// While a live member still reads codec 4, a pass backfills no sums: it fetches no chunk
// and proposes nothing the leader would refuse, and reports every chunk still pending.
// Invariant: I-219
func TestRepair_BackfillSums_WaitsForCodec5(t *testing.T) {
	const n = 3
	f := testharness.NewFabric(t, 3, 2, 1)
	fl := f.Flusher()
	fl.MinCodec = func() uint8 { return meta.CodecShardSums - 1 }
	for i := range n {
		ino := f.Create("f" + strconv.Itoa(i))
		if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), byte(60+i))); err != nil {
			t.Fatal(err)
		}
		if err := fl.FlushInode(ctx, ino); err != nil {
			t.Fatal(err)
		}
	}
	cfg := repair.DefaultConfig()
	cfg.ScrubPeriod = 0
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, cfg, nil)
	r.MinCodec = func() uint8 { return meta.CodecShardSums - 1 }
	gets := 0
	for _, idx := range f.Targets() {
		gets += f.Gets(idx)
	}
	st, err := r.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after := 0
	for _, idx := range f.Targets() {
		after += f.Gets(idx)
	}
	if st.SumsRecorded != 0 || st.SumsPending != n || st.Errors != 0 {
		t.Fatalf("recorded %d pending %d errors %d, want 0, %d, 0", st.SumsRecorded, st.SumsPending, st.Errors, n)
	}
	if after != gets {
		t.Fatalf("the pass fetched %d shards for a backfill it could not record", after-gets)
	}
	r.MinCodec = func() uint8 { return meta.CodecShardSums } // the upgrade finished
	if st, err = r.Scan(ctx); err != nil || st.SumsRecorded != n {
		t.Fatalf("after the upgrade: recorded %d, err %v", st.SumsRecorded, err)
	}
}

// The backfill records block roots for a chunk whose sums are missing or whole-shard
// while every member reads codec 18, and whole-shard sums for a chunk without any while
// one reads less, leaving whole-shard sums alone then; a pass after the upgrade records
// nothing and reports nothing pending.
// Invariant: I-422
func TestRepair_BackfillRoots_UpgradesShardSums(t *testing.T) {
	const n = 3
	for _, tc := range []struct {
		name     string
		minCodec uint8
		kind     uint8
		upgrades bool
	}{
		{"every member reads codec 18", meta.CodecBlockRoots, meta.SumsBlocks, true},
		{"a member still reads codec 17", meta.CodecBlockRoots - 1, meta.SumsShard, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 3, 2, 1)
			fl := f.Flusher()
			fl.MinCodec = func() uint8 { return meta.CodecBlockRoots - 1 } // written with whole-shard sums
			var withSums, without []meta.Hash
			for i := range 2 * n {
				if i == n {
					fl.MinCodec = func() uint8 { return meta.CodecShardSums - 1 } // the rest without any
				}
				ino := f.Create("f" + strconv.Itoa(i))
				if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), byte(60+i))); err != nil {
					t.Fatal(err)
				}
				if err := fl.FlushInode(ctx, ino); err != nil {
					t.Fatal(err)
				}
				if i < n {
					withSums = append(withSums, f.Inode(ino).Chunks[0])
				} else {
					without = append(without, f.Inode(ino).Chunks[0])
				}
			}
			for _, h := range withSums {
				if c, _ := f.FSM.GetChunk(h); c.Sums == nil || c.SumsKind != meta.SumsShard {
					t.Fatalf("chunk written at codec 17 carries %v kind %d", c.Sums, c.SumsKind)
				}
			}
			cfg := repair.DefaultConfig()
			cfg.ScrubPeriod = 0
			r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, cfg, nil)
			r.MinCodec = func() uint8 { return tc.minCodec }
			st, err := r.Scan(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := n
			if tc.upgrades {
				want = 2 * n
			}
			if st.SumsRecorded != want || st.SumsPending != 0 {
				t.Fatalf("recorded %d pending %d, want %d and 0", st.SumsRecorded, st.SumsPending, want)
			}
			for _, h := range append(append([]meta.Hash(nil), withSums...), without...) {
				c, _ := f.FSM.GetChunk(h)
				kind := tc.kind
				if !tc.upgrades && slices.Contains(withSums, h) {
					kind = meta.SumsShard
				}
				if len(c.Sums) != int(c.K) || c.SumsKind != kind {
					t.Fatalf("chunk %x: %d sums kind %d, want kind %d", h[:4], len(c.Sums), c.SumsKind, kind)
				}
				for j := range int(c.K) {
					payload, err := f.StoreOf(c.Placement[j]).Get(chunk.Key(h, c.K, c.M, j))
					if err != nil {
						t.Fatal(err)
					}
					want := sha256.Sum256(payload)
					if c.SumsKind == meta.SumsBlocks {
						want = chunk.ShardRoot(payload)
					}
					if c.Sums[j] != want {
						t.Fatalf("chunk %x shard %d: the recorded sum is not the stored payload's", h[:4], j)
					}
				}
			}
			if st, err = r.Scan(ctx); err != nil || st.SumsRecorded != 0 || st.SumsPending != 0 {
				t.Fatalf("second pass: recorded %d pending %d, %v", st.SumsRecorded, st.SumsPending, err)
			}
		})
	}
}
