package cluster_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// While a member still reads codec 4 the flusher commits without shard sums and nothing
// fails; once every member reads codec 5 the next commit carries them; a file committed
// without sums reads exactly on every node (served whole).
// Invariant: I-201
func TestCluster_V5CommitWithoutSumsWhileMemberOld(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	old := func() uint8 { return meta.CodecShardSums - 1 }
	for _, nd := range c.Nodes {
		nd.Node.MinCodec = old
	}
	defer func() {
		for _, nd := range c.Nodes {
			nd.Node.MinCodec = nil
		}
	}()
	data := testharness.Pattern(2*int(testharness.ChunkSize), 30)
	c.WriteFile(0, "/old", data)
	c.Settle()
	for _, ch := range c.ChunksOf(0, "/old") {
		if ch.Sums != nil {
			t.Fatalf("a commit while a member reads codec 4 carried sums: %v", ch.Sums)
		}
	}
	current := func() uint8 { return meta.CodecVersion }
	for _, nd := range c.Nodes {
		nd.Node.MinCodec = current
	}
	c.WriteFile(0, "/new", testharness.Pattern(int(testharness.ChunkSize), 33))
	c.Settle()
	for _, ch := range c.ChunksOf(0, "/new") {
		if len(ch.Sums) != int(ch.K) {
			t.Fatalf("after the update the commit carried %d sums, want %d", len(ch.Sums), ch.K)
		}
	}
	for i := range c.Nodes {
		got, err := c.ReadFile(i, "/old")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n%d read of the file without sums: %v", i, err)
		}
	}
}

// Every gateway returns the same bytes for every range, straddling shards, chunk
// boundaries, the hole-free tail and the file's end, through the early path.
// Invariant: I-202
func TestCluster_EarlyRange_SameBytesEveryGateway(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	cs := int(testharness.ChunkSize)
	s := ec.ShardLen(cs, 2)
	data := testharness.Pattern(3*cs+1000, 31)
	c.WriteFile(0, "/f", data)
	c.Settle()
	offs := []int{0, s / 2, s - 1, s, s + 1, cs - 1, cs, cs + s/2, 2*cs + s - 10, 3 * cs, 3*cs + 900}
	for i := range c.Nodes {
		f, err := c.Nodes[i].FS.Open("/f")
		if err != nil {
			t.Fatal(err)
		}
		for _, off := range offs {
			buf := make([]byte, 4096)
			n, err := f.ReadAt(buf, int64(off))
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("n%d off %d: %v", i, off, err)
			}
			want := data[off:min(off+4096, len(data))]
			if n != len(want) || !bytes.Equal(buf[:n], want) {
				t.Fatalf("n%d off %d: %d bytes, differ at %d", i, off, n, testharness.FirstDiff(buf[:n], want))
			}
		}
		_ = f.Close()
	}
}

// A range inside data shard 0 reads exactly before and after shard 0's holder is killed:
// early from the shard while it is there, from the decode of the others once it is gone.
// Invariant: I-202
func TestCluster_EarlyServe_HolderKilledMidRead_StillExact(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	cs := int(testharness.ChunkSize)
	s := ec.ShardLen(cs, 2)
	data := testharness.Pattern(cs, 32)
	c.WriteFile(0, "/f", data)
	c.Settle()
	ch, _ := shardHolders(c, "/f", 0)
	if len(ch.Sums) != int(ch.K) {
		t.Fatalf("chunk without sums: %+v", ch)
	}
	victim, gateway := -1, -1
	for i, nd := range c.Nodes {
		if nd.Target() == ch.Placement[0] {
			victim = i
		} else if gateway < 0 {
			gateway = i
		}
	}
	if victim < 0 || gateway < 0 {
		t.Fatalf("placement %v across the nodes", ch.Placement)
	}
	nd := c.Nodes[gateway]
	in, err := nd.Node.FSM.Resolve(meta.SplitPath("/f"))
	if err != nil {
		t.Fatal(err)
	}
	rd := vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nil, nil) // cold: every read fetches
	read := func(off int) []byte {
		t.Helper()
		buf := make([]byte, 4096)
		if _, err := rd.ReadAtEarly(ctx, in, buf, int64(off)); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		return buf
	}
	if got := read(s / 2); !bytes.Equal(got, data[s/2:s/2+4096]) {
		t.Fatal("range inside shard 0 differs with every holder alive")
	}
	c.Kill(victim)
	c.Probe()
	if got := read(s / 2); !bytes.Equal(got, data[s/2:s/2+4096]) {
		t.Fatal("range inside shard 0 differs with its holder dead")
	}
	if got := read(s + s/2); !bytes.Equal(got, data[s+s/2:s+s/2+4096]) {
		t.Fatal("range inside shard 1 differs with shard 0's holder dead")
	}
}
