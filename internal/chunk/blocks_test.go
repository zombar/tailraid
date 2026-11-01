package chunk

import (
	"crypto/sha256"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// A shard's block table has one SHA-256 per BlockSize block, the last short; its root is
// the SHA-256 of the table; one byte changed in a block changes that entry and the root;
// BlockRange widens to whole blocks and clamps to the payload.
// Invariant: I-422
func TestBlockSums_Table(t *testing.T) {
	for _, n := range []int{0, 1, BlockSize - 1, BlockSize, BlockSize + 1, 3*BlockSize + 7} {
		p := payload(n, int64(n))
		table := BlockSums(p)
		if want := (n + BlockSize - 1) / BlockSize; len(table) != want {
			t.Fatalf("n=%d: %d entries, want %d", n, len(table), want)
		}
		for i := range table {
			if got := sha256.Sum256(p[i*BlockSize : min((i+1)*BlockSize, n)]); got != table[i] {
				t.Fatalf("n=%d block %d: wrong hash", n, i)
			}
		}
		if ShardRoot(p) != BlockRoot(table) {
			t.Fatalf("n=%d: ShardRoot differs from BlockRoot(BlockSums)", n)
		}
		if n == 0 {
			continue
		}
		q := append([]byte(nil), p...)
		q[n-1] ^= 1
		other := BlockSums(q)
		last := len(table) - 1
		for i := range table {
			if (other[i] != table[i]) != (i == last) {
				t.Fatalf("n=%d: a change in the last block changed entry %d", n, i)
			}
		}
		if BlockRoot(other) == BlockRoot(table) {
			t.Fatalf("n=%d: the root did not change", n)
		}
		if !VerifySlice(Slice{Table: table, Off: 0, Data: p}, BlockRoot(table)) {
			t.Fatalf("n=%d: the whole payload does not verify", n)
		}
		if VerifySlice(Slice{Table: table, Off: 0, Data: q}, BlockRoot(table)) {
			t.Fatalf("n=%d: a changed payload verifies", n)
		}
	}
	for _, tc := range []struct{ lo, hi, n, a, b int64 }{
		{0, 1, 10, 0, 10},
		{0, 1, 3 * BlockSize, 0, BlockSize},
		{BlockSize - 1, BlockSize, 3 * BlockSize, 0, BlockSize},
		{BlockSize - 1, BlockSize + 1, 3 * BlockSize, 0, 2 * BlockSize},
		{BlockSize, 2 * BlockSize, 3 * BlockSize, BlockSize, 2 * BlockSize},
		{2*BlockSize + 5, 4 * BlockSize, 3*BlockSize + 7, 2 * BlockSize, 3*BlockSize + 7},
		{5, 5, 100, 0, 0},
		{6, 5, 100, 0, 0},
		{100, 101, 100, 0, 0},
		{-1, 1, 100, 0, 0},
	} {
		a, b := BlockRange(tc.lo, tc.hi, tc.n)
		if a != tc.a || b != tc.b {
			t.Fatalf("BlockRange(%d, %d, %d) = (%d, %d), want (%d, %d)", tc.lo, tc.hi, tc.n, a, b, tc.a, tc.b)
		}
	}
	// a slice of whole blocks from the middle verifies; an unaligned one, a short block
	// before the end, a block beyond the table and a wrong root do not
	p := payload(3*BlockSize+7, 9)
	table, root := BlockSums(p), ShardRoot(p)
	if !VerifySlice(Slice{Table: table, Off: BlockSize, Data: p[BlockSize : 3*BlockSize]}, root) {
		t.Fatal("two whole middle blocks do not verify")
	}
	if !VerifySlice(Slice{Table: table, Off: 2 * BlockSize, Data: p[2*BlockSize:]}, root) {
		t.Fatal("the tail does not verify")
	}
	if VerifySlice(Slice{Table: table, Off: 1, Data: p[1 : BlockSize+1]}, root) {
		t.Fatal("an unaligned slice verifies")
	}
	if VerifySlice(Slice{Table: table, Off: 0, Data: p[:BlockSize-1]}, root) {
		t.Fatal("a short block before the end verifies")
	}
	if VerifySlice(Slice{Table: table, Off: 4 * BlockSize, Data: p[:BlockSize]}, root) {
		t.Fatal("a block beyond the table verifies")
	}
	if VerifySlice(Slice{Table: table, Off: 0, Data: p[:BlockSize]}, meta.Hash{1}) {
		t.Fatal("a wrong root verifies")
	}
}
