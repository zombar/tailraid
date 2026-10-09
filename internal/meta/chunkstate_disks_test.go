package meta

import (
	"testing"
	"time"
)

// Invariant: I-89
// Lifecycle: L-193, L-194
func TestChunkState_DomainSkew(t *testing.T) {
	vol := Volume{ChunkSize: 1 << 20, K: 2, M: 1}
	// host 1 with disks 2, 3; host 4 with no disk
	nodes := map[NodeIdx]Node{
		1: {Idx: 1, Kind: KindDevice, State: Active, Capacity: 1, Voter: true},
		2: {Idx: 2, Kind: KindDisk, State: Active, Capacity: 1, Host: 1},
		3: {Idx: 3, Kind: KindDisk, State: Active, Capacity: 1, Host: 1},
		4: {Idx: 4, Kind: KindDevice, State: Active, Capacity: 1, Voter: true},
	}
	skewed := Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 2, 3}} // all on host 1
	const fa = 10 * time.Minute
	h := ChunkState(skewed, vol, HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa})
	// cap ceil(3/2) = 2: one slot too many, the highest index moves; still readable
	if !h.Skewed || len(h.Bad) != 1 || h.Bad[0] != (SlotIssue{Idx: 2, Reason: "host over cap", Readable: true}) || h.Readable != 3 {
		t.Fatalf("skewed: %+v", h)
	}
	if h.State(2, 1) != "degraded" {
		t.Fatalf("state %q", h.State(2, 1))
	}
	// host 4 dead: one domain left, the cap cannot be met, so nothing is skewed (L-194)
	h = ChunkState(skewed, vol, HealthInput{Nodes: nodes, Live: fakeLive{down: map[NodeIdx]time.Duration{4: time.Second}}, FillAfter: fa})
	if h.Skewed || len(h.Bad) != 0 || h.State(2, 1) != "committed" {
		t.Fatalf("unfixable: %+v", h)
	}
	// host 4 draining: same
	n4 := nodes[4]
	n4.State = Draining
	nodes[4] = n4
	if h := ChunkState(skewed, vol, HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa}); h.Skewed {
		t.Fatalf("draining host made it fixable: %+v", h)
	}
	n4.State = Active
	nodes[4] = n4
	// within the cap: nothing to do
	ok := Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 4, 2}}
	if h := ChunkState(ok, vol, HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa}); h.Skewed || len(h.Bad) != 0 {
		t.Fatalf("within cap: %+v", h)
	}
	// a slot already bad is not counted twice: host 1 holds 3 but slot 2's disk is draining
	n3 := nodes[3]
	n3.State = Draining
	nodes[3] = n3
	h = ChunkState(skewed, vol, HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa})
	if h.Skewed || len(h.Bad) != 1 || h.Bad[0].Reason != "node draining" {
		t.Fatalf("draining slot counted as skew: %+v", h)
	}
	// a draining host drains its disks with it; a removed host removes them (docs/14 §1)
	n3.State = Active
	nodes[3] = n3
	host := nodes[1]
	host.State = Draining
	nodes[1] = host
	h = ChunkState(Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{4, 2, 3}}, vol, HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa})
	if len(h.Bad) != 2 || h.Bad[0].Reason != "node draining" || h.Bad[1].Reason != "node draining" || !h.Bad[0].Readable {
		t.Fatalf("disks of a draining host: %+v", h)
	}
	host.State = Removed
	nodes[1] = host
	h = ChunkState(Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{4, 2, 3}}, vol, HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa})
	if len(h.Bad) != 2 || h.Bad[0].Reason != "node removed" || h.Readable != 1 {
		t.Fatalf("disks of a removed host: %+v", h)
	}
	host.State = Active
	nodes[1] = host
	// one target per host never skews (I-87 regression)
	flat := map[NodeIdx]Node{1: {Idx: 1, State: Active, Capacity: 1}, 2: {Idx: 2, State: Active, Capacity: 1}, 3: {Idx: 3, State: Active, Capacity: 1}}
	if h := ChunkState(Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 2, 3}}, vol, HealthInput{Nodes: flat, Live: fakeLive{}, FillAfter: fa}); h.Skewed {
		t.Fatal("flat cluster skewed")
	}
}
