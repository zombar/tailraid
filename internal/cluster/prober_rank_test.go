package cluster

import (
	"flag"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

var rankSeed = flag.Int64("seed", 0, "seed of the shuffled rank input (0: the clock)")

// Rank orders this host and its own disks first, then holders by the class of the path to
// their host and the RTT within it, S3 nodes last, the same on every node for the same
// path table; a disk takes its host's class; without a mesh every path is a LAN and the
// RTT alone orders; the snapshot carries the class.
// Invariant: I-327
func TestProber_RankByClass_Determinism(t *testing.T) {
	seed := *rankSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	t.Logf("seed %d", seed)
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
	dev := func(name string) meta.NodeIdx {
		return upsert(meta.Node{ID: name + "-000000000000000", Name: name, Kind: meta.KindDevice, Addr: "10.0.0." + name, RPCPort: 1, RaftPort: 1})
	}
	b, c, d, e := dev("2"), dev("3"), dev("4"), dev("5")
	ownDisk := upsert(meta.Node{ID: "disk-self-0000000", Name: "a/1", Kind: meta.KindDisk, Host: self, Capacity: 1})
	diskOfC := upsert(meta.Node{ID: "disk-c-0000000000", Name: "3/1", Kind: meta.KindDisk, Host: c, Capacity: 1})
	s3 := upsert(meta.Node{ID: "s3aaaaaaaaaaaaaa", Name: "offsite", Kind: meta.KindS3, S3Endpoint: "https://s3.example.com", S3Bucket: "b", Capacity: 1 << 40})
	classes := map[meta.NodeIdx]meta.PathClass{b: meta.PathLAN, c: meta.PathWAN, d: meta.PathRelay, e: meta.PathWAN}
	rtts := map[meta.NodeIdx]time.Duration{b: 30 * time.Millisecond, c: 50 * time.Millisecond, d: 5 * time.Millisecond, e: 5 * time.Millisecond}
	now := time.Now()
	mk := func(withMesh bool) *Prober {
		p := NewProber(n, nil, clock.NewFake(now))
		if withMesh {
			p.PathOf = func(idx meta.NodeIdx) meta.PathClass { return classes[idx] }
		}
		for idx, rtt := range rtts {
			p.record(idx, now, rtt, true, rpc.Health{}, nil)
		}
		return p
	}
	all := []meta.NodeIdx{self, ownDisk, b, c, d, e, diskOfC, s3}
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // a test shuffle
	in := slices.Clone(all)
	rng.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })

	// by class then RTT: own first, b (lan), e (wan 5 ms), c (wan 50 ms), c's disk (wan,
	// unprobed), d (relay 5 ms), the bucket
	var first []meta.NodeIdx
	for i := range 3 {
		got := mk(true).Rank(in)
		if i == 0 {
			first = got
		} else if !slices.Equal(got, first) {
			t.Fatalf("node %d ranks %v, node 0 ranked %v (seed %d)", i, got, first, seed)
		}
	}
	own := []meta.NodeIdx{first[0], first[1]}
	slices.Sort(own)
	if want := []meta.NodeIdx{self, ownDisk}; !slices.Equal(own, want) {
		t.Fatalf("own first: %v, want %v (rank %v, seed %d)", own, want, first, seed)
	}
	if rest, want := first[2:], []meta.NodeIdx{b, e, c, diskOfC, d, s3}; !slices.Equal(rest, want) {
		t.Fatalf("rank %v, want own then %v (seed %d)", first, want, seed)
	}

	// without a mesh: own first, then RTT alone (d and e tie at 5 ms in input order), S3 last
	p := mk(false)
	got := p.Rank(in)
	rest := got[2:]
	tie := []meta.NodeIdx{rest[0], rest[1]}
	slices.Sort(tie)
	if !slices.Equal(tie, []meta.NodeIdx{d, e}) || !slices.Equal(rest[2:], []meta.NodeIdx{b, c, diskOfC, s3}) {
		t.Fatalf("no mesh: rank %v (seed %d)", got, seed)
	}
	for _, idx := range []meta.NodeIdx{b, c, d, e, diskOfC} {
		if cl := p.Class(idx); cl != meta.PathLAN {
			t.Fatalf("no mesh: class of %d is %s, want lan", idx, cl)
		}
	}

	// the class table and the snapshot
	p = mk(true)
	for _, tc := range []struct {
		idx  meta.NodeIdx
		want meta.PathClass
	}{{self, meta.PathLAN}, {ownDisk, meta.PathLAN}, {b, meta.PathLAN}, {c, meta.PathWAN}, {diskOfC, meta.PathWAN}, {d, meta.PathRelay}, {s3, meta.PathWAN}} {
		if got := p.Class(tc.idx); got != tc.want {
			t.Errorf("class of %d: %s, want %s", tc.idx, got, tc.want)
		}
	}
	if snap := p.Snapshot(); snap[d].Class != meta.PathRelay || snap[b].Class != meta.PathLAN {
		t.Fatalf("snapshot classes: d %s, b %s", snap[d].Class, snap[b].Class)
	}
}
