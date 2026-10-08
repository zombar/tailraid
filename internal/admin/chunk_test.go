package admin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

// `chunk describe` derives the state from the FSM copy and liveness, lists the files that
// reference the chunk, checks each live slot, and prints the node's event ring, which
// shows the path a chunk took (upload then commit; a dedup hit on the second file).
// Invariant: I-57
// Lifecycle: L-33, L-140, L-109
func TestChunkDescribe_StateFilesAndEvents(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	ctx := context.Background()
	data := testharness.Pattern(int(testharness.ChunkSize), 9)
	c.WriteFile(0, "/a", data)
	c.WriteFile(0, "/d-b", data) // same bytes: dedup on the second flush
	c.Settle()
	n0 := c.Nodes[0]
	reps, err := admin.DescribeChunks(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "/a")
	if err != nil || len(reps) != 1 {
		t.Fatalf("describe /a: %v %d", err, len(reps))
	}
	r := reps[0]
	if !r.Found || r.State != "committed" || r.Refs != 2 || r.K != 2 || r.M != 1 || r.LiveSlots != 3 || len(r.Slots) != 3 {
		t.Fatalf("report %+v", r)
	}
	if len(r.Files) != 2 || r.Files[0] != "/a" || r.Files[1] != "/d-b" {
		t.Fatalf("files %v", r.Files)
	}
	for _, s := range r.Slots {
		if s.Node == 0 || !s.Live || s.Present == nil || !*s.Present || s.Name == "" {
			t.Fatalf("slot %+v", s)
		}
	}
	kinds := n0.Events.Kinds(hashOf(t, r.Hash))
	if !hasSeq(kinds, meta.EvUploaded, meta.EvCommitted, meta.EvDedupHit, meta.EvCommitted) {
		t.Fatalf("event path %v", kinds)
	}
	// a hash prefix selects the same chunk; an ambiguous or malformed one is refused
	byPrefix, err := admin.DescribeChunks(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, r.Hash[:12])
	if err != nil || len(byPrefix) != 1 || byPrefix[0].Hash != r.Hash {
		t.Fatalf("by prefix: %v %+v", err, byPrefix)
	}
	if _, err := admin.DescribeChunks(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "abc"); err == nil {
		t.Fatal("short selector accepted")
	}
	if _, err := admin.DescribeChunks(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "/nope"); !errors.Is(err, meta.ErrNotExist) {
		t.Fatalf("missing path: %v", err)
	}
	// a scrub pass records scrub_ok on the scanning node
	cfg := repair.DefaultConfig()
	cfg.ScrubShare = 1
	rp := repair.New(n0.Node.FSM, n0.Node, n0.Prober, n0.Client, n0.Reader, c.Clk, cfg, nil)
	rp.Events = n0.Events
	leader := c.Leader()
	if leader.I != 0 {
		rp = repair.New(leader.Node.FSM, leader.Node, leader.Prober, leader.Client, leader.Reader, c.Clk, cfg, nil)
		rp.Events = leader.Events
	}
	if _, err := rp.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if !hasSeq(leader.Events.Kinds(hashOf(t, r.Hash)), meta.EvScrubOK) {
		t.Fatalf("no scrub_ok on the leader: %v", leader.Events.Kinds(hashOf(t, r.Hash)))
	}
	// a dead slot holder: degraded, slot not live, no HEAD attempted; a ring-less node
	// still reports the state but says the ring is off
	victim := r.Slots[0].Node - 1
	if victim == 0 {
		victim = r.Slots[1].Node - 1
	}
	c.Kill(victim)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	reps, err = admin.DescribeChunks(ctx, n0.Node.FSM, n0.Prober, n0.Client, nil, r.Hash)
	if err != nil || len(reps) != 1 {
		t.Fatal(err)
	}
	r = reps[0]
	if r.State != "degraded" || r.LiveSlots != 2 || r.EventRing || r.Events != nil {
		t.Fatalf("after kill %+v", r)
	}
	for _, s := range r.Slots {
		if s.Node-1 == victim && (s.Live || s.Present != nil) {
			t.Fatalf("dead slot %+v", s)
		}
	}
	// unknown hash and the zero chunk
	if reps, err = admin.DescribeChunks(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "00000000000000000000000000000000ffffffffffffffffffffffffffffffff"); err != nil || reps[0].Found || reps[0].State != "unknown" {
		t.Fatalf("unknown: %v %+v", err, reps)
	}
	if reps, err = admin.DescribeChunks(ctx, n0.Node.FSM, n0.Prober, n0.Client, n0.Events, "0000000000000000000000000000000000000000000000000000000000000000"); err != nil || reps[0].State != "zero" {
		t.Fatalf("zero: %v %+v", err, reps)
	}
}

func hashOf(t *testing.T, hexs string) meta.Hash {
	t.Helper()
	var h meta.Hash
	for i := 0; i < 32; i++ {
		var b byte
		for _, c := range hexs[2*i : 2*i+2] {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				b |= byte(c-'a') + 10
			default:
				t.Fatalf("bad hex %q", hexs)
			}
		}
		h[i] = b
	}
	return h
}

// hasSeq reports whether want appears in kinds as a subsequence, in order.
func hasSeq(kinds []meta.EventKind, want ...meta.EventKind) bool {
	j := 0
	for _, k := range kinds {
		if j < len(want) && k == want[j] {
			j++
		}
	}
	return j == len(want)
}
