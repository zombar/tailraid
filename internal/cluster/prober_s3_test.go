package cluster

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// s3Health answers device probes with a live host and S3 probes from a table.
type s3Health struct {
	down   map[meta.NodeIdx]bool
	probed map[meta.NodeIdx]int
}

func (f *s3Health) Health(context.Context, string) (rpc.Health, error) { return rpc.Health{}, nil }

func (f *s3Health) ProbeS3(_ context.Context, idx meta.NodeIdx) error {
	f.probed[idx]++
	if f.down[idx] {
		return errors.New("bucket down")
	}
	return nil
}

// A bucket is probed every S3ProbeInterval, live within S3LiveThreshold of its last good
// probe, dead to a node that cannot probe it, and ranked after every device (docs/10 §4.3).
// Lifecycle: L-104
func TestProber_S3LivenessAndRank(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	upsert := func(nd meta.Node) meta.NodeIdx {
		t.Helper()
		r, idx, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
		if err != nil || r.Err != nil {
			t.Fatal(err, r.Err)
		}
		if err := n.FSM.WaitApplied(ctx, idx); err != nil {
			t.Fatal(err)
		}
		return meta.NodeIdx(r.Ino)
	}
	self, _ := n.SelfIdx()
	peer := upsert(meta.Node{ID: "peer-b0000000000000", Name: "b", Kind: meta.KindDevice, Addr: "10.0.0.2", RPCPort: 1, RaftPort: 1})
	bucket := upsert(meta.Node{ID: "s3aaaaaaaaaaaaaa", Name: "offsite", Kind: meta.KindS3, S3Endpoint: "https://s3.example.com", S3Bucket: "b", Capacity: 1 << 40, JoinedAt: time.Now().UnixNano()})
	clk := clock.NewFake(time.Now())
	fh := &s3Health{down: map[meta.NodeIdx]bool{}, probed: map[meta.NodeIdx]int{}}
	p := NewProber(n, fh, clk)
	if p.IsLive(bucket) {
		t.Fatal("a bucket is live before its first probe")
	}
	p.Tick(ctx)
	if !p.IsLive(bucket) || fh.probed[bucket] != 1 {
		t.Fatalf("after a good probe: live=%v probes=%d", p.IsLive(bucket), fh.probed[bucket])
	}
	if got := p.Rank([]meta.NodeIdx{bucket, peer, self}); !slices.Equal(got, []meta.NodeIdx{self, peer, bucket}) {
		t.Fatalf("rank %v: the bucket must come last", got)
	}
	// a device tick does not probe the bucket again before S3ProbeInterval
	clk.Advance(ProbeInterval)
	p.Tick(ctx)
	if fh.probed[bucket] != 1 {
		t.Fatalf("bucket probed %d times within %v", fh.probed[bucket], S3ProbeInterval)
	}
	// a failing bucket stays live until S3LiveThreshold has passed since the last good probe
	fh.down[bucket] = true
	for p.IsLive(bucket) {
		clk.Advance(ProbeInterval)
		p.Tick(ctx)
		if d := p.DeadFor(bucket); d > 0 && p.IsLive(bucket) {
			t.Fatal("dead and live at once")
		}
		if fh.probed[bucket] > 10 {
			t.Fatal("bucket never went dead")
		}
	}
	if fh.probed[bucket] < int(S3LiveThreshold/S3ProbeInterval) {
		t.Fatalf("dead after %d probes; a bucket is live for %v", fh.probed[bucket], S3LiveThreshold)
	}
	// and comes back with the next good probe
	fh.down[bucket] = false
	clk.Advance(S3ProbeInterval)
	p.Tick(ctx)
	if !p.IsLive(bucket) {
		t.Fatal("bucket not live after a good probe")
	}
	// a node whose client cannot probe buckets never sees one live
	q := NewProber(n, &diskHealth{}, clk)
	q.Tick(ctx)
	if q.IsLive(bucket) {
		t.Fatal("a bucket is live to a node that cannot probe it")
	}
}
