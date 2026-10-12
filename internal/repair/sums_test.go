package repair_test

import (
	"crypto/sha256"
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
					if sha256.Sum256(payload) != c.Sums[j] {
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
