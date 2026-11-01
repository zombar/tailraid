package cluster_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// Through the real RPC, a few KiB of a cold chunk read on another node come from a range
// GET of one data shard (the chunk's sums are block roots), exact at the head of the
// file and in its partial last chunk; the chunk's next range reads it whole.
// Invariant: I-424, I-423
func TestCluster_RangeRead_ThroughOtherGateway(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	cs := int(testharness.ChunkSize)
	data := testharness.Pattern(3*cs+777, 44)
	c.WriteFile(0, "/f", data)
	c.Settle()
	for _, ch := range c.ChunksOf(0, "/f") {
		if ch.SumsKind != meta.SumsBlocks || len(ch.Sums) != int(ch.K) {
			t.Fatalf("chunk without block roots: %+v", ch)
		}
	}
	nd := c.Nodes[1]
	in, err := nd.Node.FSM.Resolve([]string{"f"})
	if err != nil {
		t.Fatal(err)
	}
	rd := vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nil, nil)
	for i, off := range []int{0, len(data) - 600} { // the head, and inside the partial last chunk
		buf := make([]byte, 512)
		if _, err := rd.ReadAtEarly(context.Background(), in, buf, int64(off)); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		if !bytes.Equal(buf, data[off:off+512]) {
			t.Fatalf("range at %d differs at %d", off, testharness.FirstDiff(buf, data[off:off+512]))
		}
		if n := rd.ReadsRanged(); n != uint64(i+1) {
			t.Fatalf("reads_ranged %d after read %d, want %d", n, i+1, i+1)
		}
	}
	buf := make([]byte, 4096)
	if _, err := rd.ReadAtEarly(context.Background(), in, buf, 8192); err != nil || !bytes.Equal(buf, data[8192:8192+4096]) {
		t.Fatalf("second touch: %v", err)
	}
	if n := rd.ReadsRanged(); n != 2 {
		t.Fatalf("reads_ranged %d after the second touch, want 2", n)
	}
	testharness.Eventually(t, func() bool { return rd.Fetching() == 0 }, "the chunk fetch completes")
}
