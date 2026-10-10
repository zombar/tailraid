package cluster_test

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3/s3test"
	"github.com/zombar/tailraid/internal/testharness"
)

// s3Cluster is two devices with a disk each and a bucket under prefix "c" at 2+1: every
// stripe has a slot on the bucket, and voter-first placement gives it the parity.
func s3Cluster(t *testing.T) (*testharness.Cluster, *s3test.Server, meta.NodeIdx) {
	t.Helper()
	c := testharness.NewCluster(t, 2, 2, 1)
	srv := s3test.New()
	t.Cleanup(srv.Close)
	bucket := c.AddS3(srv, "offsite", "c")
	return c, srv, bucket
}

// bucketSlot is the index of the bucket's slot in ch, or -1.
func bucketSlot(ch meta.Chunk, bucket meta.NodeIdx) int { return slices.Index(ch.Placement, bucket) }

// Stripes land on the bucket in the disk layout; reads leave it alone while the devices
// hold k shards, and use it when one is gone (docs/10 §4.3).
// Invariant: I-52
// Lifecycle: L-104
func TestS3Node_PlacementAndReads(t *testing.T) {
	c, srv, bucket := s3Cluster(t)
	defer c.Close()
	data := testharness.Pattern(3*int(testharness.ChunkSize)+100, 4)
	c.WriteFile(0, "/f", data)
	for _, ch := range c.ChunksOf(0, "/f") {
		i := bucketSlot(ch, bucket)
		if i < int(ch.K) {
			t.Fatalf("placement %v: want the bucket in a parity slot", ch.Placement)
		}
		obj, ok := srv.Object("c/" + rpc.ShardObject(chunk.Key(ch.Hash, ch.K, ch.M, i)))
		if !ok {
			t.Fatalf("slot %d of %x not in the bucket: %v", i, ch.Hash[:4], srv.Keys())
		}
		if _, err := chunk.Unframe(obj); err != nil {
			t.Fatalf("object of slot %d: %v", i, err)
		}
	}
	if got, err := c.ReadFileCold(0, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %v", err)
	}
	if n := srv.Count(http.MethodGet); n != 0 {
		t.Fatalf("%d GETs reached the bucket with every device up", n)
	}
	c.FailDisk(1, c.Nodes[1].Target(), true)
	c.Probe()
	if got, err := c.ReadFileCold(0, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read with a disk down: %v", err)
	}
	if srv.Count(http.MethodGet) == 0 {
		t.Fatal("the read with a disk down did not use the bucket")
	}
}

// flushAdvancing flushes node i while moving the fake clock, so the PUT deadline passes.
func flushAdvancing(t *testing.T, c *testharness.Cluster, i int) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Nodes[i].Flush.FlushAll(context.Background()) }()
	var err error
	testharness.Eventually(t, func() bool {
		select {
		case err = <-done:
			return true
		default:
			c.Clk.Advance(500 * time.Millisecond)
			return false
		}
	}, "flush past the PUT deadline")
	must(t, err)
}

// A bucket that holds its PUTs does not hold the flush past the deadline: the chunk
// commits with the bucket's slot missing, and the repairer fills it once the bucket
// answers (docs/10 §4.4).
// Invariant: I-53
// Lifecycle: L-48
func TestS3Node_SlowBucket_DoesNotStallFlush(t *testing.T) {
	c, srv, bucket := s3Cluster(t)
	defer c.Close()
	release := srv.BlockPuts()
	defer release()
	data := testharness.Pattern(int(testharness.ChunkSize), 6)
	f, err := c.Nodes[0].FS.Create("/f")
	must(t, err)
	_, err = f.Write(data)
	must(t, err)
	must(t, f.Close())
	flushAdvancing(t, c, 0)
	ch := c.ChunksOf(0, "/f")[0]
	if bucketSlot(ch, bucket) >= 0 || !slices.Contains(ch.Placement, 0) {
		t.Fatalf("placement %v: want the bucket's slot missing", ch.Placement)
	}
	release()
	c.Clk.Advance(cluster.S3ProbeInterval)
	c.Probe()
	st, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired == 0 {
		t.Fatalf("scan %+v: the missing slot was not filled", st)
	}
	ch = c.ChunksOf(0, "/f")[0]
	if i := bucketSlot(ch, bucket); i < 0 {
		t.Fatalf("placement %v: the bucket was not refilled", ch.Placement)
	} else if _, ok := srv.Object("c/" + rpc.ShardObject(chunk.Key(ch.Hash, ch.K, ch.M, i))); !ok {
		t.Fatal("the refilled slot has no object")
	}
	if got, err := c.ReadFileCold(0, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %v", err)
	}
}

// A bucket that refuses every request degrades the stripe instead of failing the flush.
// Invariant: I-53
func TestS3Node_UnreachableBucket_FlushDegrades(t *testing.T) {
	c, srv, bucket := s3Cluster(t)
	defer c.Close()
	srv.SetDown(true) // still live to the prober until S3LiveThreshold passes
	data := testharness.Pattern(int(testharness.ChunkSize), 8)
	c.WriteFile(0, "/f", data)
	ch := c.ChunksOf(0, "/f")[0]
	if bucketSlot(ch, bucket) >= 0 {
		t.Fatalf("placement %v names a bucket that refused the PUT", ch.Placement)
	}
	c.Settle()
	if got, err := c.ReadFileCold(1, "/f"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %v", err)
	}
}

// An object deleted behind the cluster's back is found by the scrub's HEAD, never a GET,
// and put back by the repairer (docs/06 §5).
// Lifecycle: L-112, L-139
func TestS3Node_ObjectMissing_Repaired(t *testing.T) {
	c, srv, bucket := s3Cluster(t)
	defer c.Close()
	data := testharness.Pattern(int(testharness.ChunkSize), 9)
	c.WriteFile(0, "/f", data)
	ch := c.ChunksOf(0, "/f")[0]
	i := bucketSlot(ch, bucket)
	name := "c/" + rpc.ShardObject(chunk.Key(ch.Hash, ch.K, ch.M, i))
	srv.DeleteObject(name)
	cfg := repairCfg()
	cfg.ScrubShare = 1
	gets := srv.Count(http.MethodGet)
	st, err := newRepairer(c, c.Leader(), cfg).Scrub(ctx)
	must(t, err)
	if st.ScrubBad != 1 || st.Repaired != 1 {
		t.Fatalf("scrub %+v: want the missing object found and repaired", st)
	}
	if n := srv.Count(http.MethodGet) - gets; n != 0 {
		t.Fatalf("the scrub sent %d GETs to the bucket", n)
	}
	if _, ok := srv.Object(name); !ok {
		t.Fatal("the object was not put back")
	}
	if ch2 := c.ChunksOf(0, "/f")[0]; ch2.Placement[i] != bucket {
		t.Fatalf("placement %v", ch2.Placement)
	}
}

// GC deletes a collected chunk's disk shards and never touches the bucket; the leader's
// sweep removes the objects once they are older than grace (docs/06 §3).
// Invariant: I-138
func TestS3Node_GCNeverDeletes(t *testing.T) {
	c, srv, bucket := s3Cluster(t)
	defer c.Close()
	c.WriteFile(0, "/f", testharness.Pattern(int(testharness.ChunkSize), 10))
	c.Settle()
	objects := len(srv.Keys())
	must(t, c.Nodes[0].FS.Remove("/f"))
	c.Settle()
	leader := c.Leader()
	const grace = 10 * time.Minute
	gc := repair.NewGC(leader.Node.FSM, leader.Node, leader.Client, leader.Prober, c.Clk, grace, nil)
	c.Clk.Advance(grace + time.Minute)
	c.Probe()
	res, err := gc.RunOnce(ctx)
	must(t, err)
	if res.Victims != 1 {
		t.Fatalf("gc %+v", res)
	}
	if n := srv.Count(http.MethodDelete); n != 0 || len(srv.Keys()) != objects {
		t.Fatalf("GC sent %d DELETEs; bucket %v", n, srv.Keys())
	}
	n, err := repair.SweepS3(ctx, leader.Client, leader.Node.FSM, bucket, grace, time.Now().Add(grace+time.Second))
	must(t, err)
	if n != objects || len(srv.Keys()) != 0 {
		t.Fatalf("sweep deleted %d of %d objects; left %v", n, objects, srv.Keys())
	}
}

// A device without the bucket's credentials never sees it live and never reaches it:
// its flushes leave the bucket's slot to the repairer (docs/10 §4.1).
// Invariant: I-139
func TestS3Node_DeviceWithoutKeys(t *testing.T) {
	c := testharness.NewCluster(t, 2, 2, 1)
	defer c.Close()
	srv := s3test.New()
	defer srv.Close()
	bucket := c.AddS3(srv, "offsite", "c", 0) // node 1 holds no key
	if !c.Nodes[0].Prober.IsLive(bucket) || c.Nodes[1].Prober.IsLive(bucket) {
		t.Fatalf("live: n0 %v n1 %v", c.Nodes[0].Prober.IsLive(bucket), c.Nodes[1].Prober.IsLive(bucket))
	}
	puts := srv.Count(http.MethodPut)
	c.WriteFile(1, "/f", testharness.Pattern(int(testharness.ChunkSize), 11))
	if srv.Count(http.MethodPut) != puts {
		t.Fatal("a device without the key reached the bucket")
	}
	if ch := c.ChunksOf(1, "/f")[0]; bucketSlot(ch, bucket) >= 0 {
		t.Fatalf("placement %v names the bucket", ch.Placement)
	}
}

// The leader judges a bucket by its own probe (docs/10 §4.3): a diskless voter that leads
// with the key keeps the bucket's slots across fill_after, and one that leads without the
// key re-places them onto devices once fill_after passes, as for a dead disk.
// Invariant: I-148
func TestS3Node_LeaderKeyDecidesFill(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withKeys []int // nodes given the key; the diskless leader is node 0
		kept     bool  // the bucket's slots survive a scan past fill_after
	}{
		{"leader holds the key", nil, true},
		{"leader lacks the key", []int{1, 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testharness.NewCluster(t, 3, 2, 1, testharness.Diskless(0))
			defer c.Close()
			leader := c.Leader()
			if leader != c.Nodes[0] || !c.Voter(0) {
				t.Fatal("test needs the diskless voter n0 leading")
			}
			srv := s3test.New()
			defer srv.Close()
			bucket := c.AddS3(srv, "offsite", "c", tc.withKeys...)
			if live := leader.Prober.IsLive(bucket); live != tc.kept {
				t.Fatalf("bucket live on the leader: %v", live)
			}
			data := testharness.Pattern(2*int(testharness.ChunkSize), 9) // whole chunks: a short tail narrows to 1+1 and may skip the bucket
			c.WriteFile(1, "/f", data)                                   // n1 holds the key in both cases
			before := c.ChunksOf(1, "/f")
			for _, ch := range before {
				if bucketSlot(ch, bucket) < 0 {
					t.Fatalf("placement %v: the bucket got no slot", ch.Placement)
				}
			}
			c.AddNode(false) // a spare disk the leader could re-place the bucket's slots onto
			st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
			must(t, err)
			if st.Repaired != 0 {
				t.Fatalf("re-placed inside fill_after: %+v", st)
			}
			c.Clk.Advance(repairCfg().FillAfter + time.Second)
			c.Probe()
			st, err = newRepairer(c, leader, repairCfg()).Scan(ctx)
			must(t, err)
			c.Settle()
			after := c.ChunksOf(0, "/f")
			for i, ch := range after {
				onBucket := bucketSlot(ch, bucket) >= 0
				switch {
				case tc.kept && !slices.Equal(ch.Placement, before[i].Placement):
					t.Fatalf("placement moved with the key held: %v -> %v (scan %+v)", before[i].Placement, ch.Placement, st)
				case !tc.kept && onBucket:
					t.Fatalf("placement %v still names the bucket after fill_after (scan %+v)", ch.Placement, st)
				}
			}
			if !tc.kept && st.Repaired == 0 {
				t.Fatalf("scan after fill_after moved nothing: %+v", st)
			}
			if got, err := c.ReadFileCold(1, "/f"); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("read: %v", err)
			}
		})
	}
}
