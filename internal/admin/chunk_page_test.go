package admin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// The web UI pages a file's chunk reports (docs/13 §5): offset/limit over the file's chunk
// list, total = the whole count, limit <= 0 = everything. Boundaries {0, 1, n-1, n, n+1}.
// Invariant: I-57
func TestDescribeChunksPage_Boundaries(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	ctx := context.Background()
	const n = 5
	c.WriteFile(0, "/f", testharness.Pattern(n*int(testharness.ChunkSize), 11))
	c.Settle()
	n0 := c.Nodes[0]
	page := func(offset, limit int) (int, []admin.ChunkReport) {
		t.Helper()
		total, reps, err := admin.DescribeChunksPage(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "/f", offset, limit)
		if err != nil {
			t.Fatalf("page(%d,%d): %v", offset, limit, err)
		}
		if total != n {
			t.Fatalf("page(%d,%d): total %d, want %d", offset, limit, total, n)
		}
		return total, reps
	}
	cases := []struct {
		offset, limit, want int
	}{
		{0, 0, n}, {0, 1, 1}, {0, n - 1, n - 1}, {0, n, n}, {0, n + 1, n},
		{1, 0, n - 1}, {n - 1, 0, 1}, {n, 0, 0}, {n + 1, 0, 0}, {2, 2, 2}, {n - 1, 2, 1},
	}
	for _, tc := range cases {
		_, reps := page(tc.offset, tc.limit)
		if len(reps) != tc.want {
			t.Errorf("page(%d,%d): %d reports, want %d", tc.offset, tc.limit, len(reps), tc.want)
		}
		if len(reps) > 0 && reps[0].Hash != hashHex(t, n0, tc.offset) {
			t.Errorf("page(%d,%d): first chunk is not chunk %d", tc.offset, tc.limit, tc.offset)
		}
	}
	// DescribeChunks is the unpaged form of the same function
	all, err := admin.DescribeChunks(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "/f")
	if err != nil || len(all) != n {
		t.Fatalf("DescribeChunks: %v %d", err, len(all))
	}
	if _, _, err := admin.DescribeChunksPage(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "/nope", 0, 0); !errors.Is(err, meta.ErrNotExist) {
		t.Fatalf("missing path: %v", err)
	}
	// an empty file has zero chunks and an empty first page
	c.WriteFile(0, "/empty", nil)
	c.Settle()
	total, reps, err := admin.DescribeChunksPage(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "/empty", 0, 64)
	if err != nil || total != 0 || len(reps) != 0 {
		t.Fatalf("empty file: %d %d %v", total, len(reps), err)
	}
}

// hashHex is the hex hash of chunk i of /f as the FSM records it.
func hashHex(t *testing.T, n *testharness.ClusterNode, i int) string {
	t.Helper()
	in, err := n.Node.FSM.Resolve(meta.SplitPath("/f"))
	if err != nil || i >= len(in.Chunks) {
		t.Fatalf("resolve /f: %v (%d chunks)", err, len(in.Chunks))
	}
	return admin.DescribeChunk(context.Background(), n.Node.FSM, nil, nil, nil, in.Chunks[i]).Hash
}
