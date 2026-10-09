package meta

import (
	"reflect"
	"testing"
	"time"
)

type fakeLive struct {
	down map[NodeIdx]time.Duration // dead for this long; absent = live
}

func (f fakeLive) IsLive(idx NodeIdx) bool           { _, dead := f.down[idx]; return !dead }
func (f fakeLive) DeadFor(idx NodeIdx) time.Duration { return f.down[idx] }

// The one derivation of docs/11 §2, as a table over every rule: each row is a chunk
// plus the node table, liveness, reports and presence, and the state every consumer
// (status, check, describe, repair) must agree on.
// Invariant: I-56
func TestChunkState_Table(t *testing.T) {
	vol := Volume{ChunkSize: 1 << 20, K: 2, M: 1}
	nodes := map[NodeIdx]Node{1: {Idx: 1, State: Active}, 2: {Idx: 2, State: Active}, 3: {Idx: 3, State: Active}, 4: {Idx: 4, State: Draining}, 5: {Idx: 5, State: Removed}}
	full := Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 2, 3}}
	const fa = 10 * time.Minute
	cases := []struct {
		name  string
		c     Chunk
		in    HealthInput
		want  ChunkHealth
		state string
	}{
		{
			"committed", full,
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Readable: 3},
			"committed",
		},
		{
			"tombstoned",
			Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 0, Placement: []NodeIdx{1, 2, 3}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Tombstoned: true, Readable: 3},
			"tombstoned",
		},
		{
			"missing slot",
			Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 0, 3}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Bad: []SlotIssue{{Idx: 1, Reason: "missing"}}, Readable: 2},
			"degraded",
		},
		{
			"dead inside fill_after: kept, not readable, no repair yet", full,
			HealthInput{Nodes: nodes, Live: fakeLive{down: map[NodeIdx]time.Duration{2: fa - time.Second}}, FillAfter: fa},
			ChunkHealth{Readable: 2},
			"degraded",
		},
		{
			"dead at fill_after: bad", full,
			HealthInput{Nodes: nodes, Live: fakeLive{down: map[NodeIdx]time.Duration{2: fa}}, FillAfter: fa},
			ChunkHealth{Bad: []SlotIssue{{Idx: 1, Reason: "node dead"}}, Readable: 2},
			"degraded",
		},
		{
			"two dead inside fill_after: unreadable but not degraded", full,
			HealthInput{Nodes: nodes, Live: fakeLive{down: map[NodeIdx]time.Duration{2: time.Second, 3: time.Second}}, FillAfter: fa},
			ChunkHealth{Readable: 1},
			"unreadable",
		},
		{
			"draining live: bad but readable",
			Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 4, 3}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Bad: []SlotIssue{{Idx: 1, Reason: "node draining", Readable: true}}, Readable: 3},
			"degraded",
		},
		{
			"draining dead: bad, not readable",
			Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 4, 3}},
			HealthInput{Nodes: nodes, Live: fakeLive{down: map[NodeIdx]time.Duration{4: time.Second}}, FillAfter: fa},
			ChunkHealth{Bad: []SlotIssue{{Idx: 1, Reason: "node draining"}}, Readable: 2},
			"degraded",
		},
		{
			"removed live: never readable",
			Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 5, 3}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Bad: []SlotIssue{{Idx: 1, Reason: "node removed"}}, Readable: 2},
			"degraded",
		},
		{
			"reported by a reader", full,
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa, Reported: map[int]bool{2: true}},
			ChunkHealth{Bad: []SlotIssue{{Idx: 2, Reason: "reported"}}, Readable: 2},
			"degraded",
		},
		{
			"HEAD says absent", full,
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa, Present: map[int]bool{0: true, 1: false, 2: true}},
			ChunkHealth{Bad: []SlotIssue{{Idx: 1, Reason: "absent"}}, Readable: 2},
			"degraded",
		},
		{
			"HEAD unknown for a slot counts as present", full,
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa, Present: map[int]bool{0: true}},
			ChunkHealth{Readable: 3},
			"committed",
		},
		{
			"policy stale",
			Chunk{Size: 1 << 20, K: 2, M: 2, Refs: 1, Placement: []NodeIdx{1, 2, 3, 0}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Bad: []SlotIssue{{Idx: 3, Reason: "missing"}}, Readable: 3, PolicyStale: true},
			"degraded",
		},
		{
			"small chunk keeps k=1, follows M",
			Chunk{Size: 100, K: 1, M: 1, Refs: 1, Placement: []NodeIdx{1, 2}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Readable: 2},
			"committed",
		},
		{
			"small chunk with stale M",
			Chunk{Size: 100, K: 1, M: 2, Refs: 1, Placement: []NodeIdx{1, 2, 3}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Readable: 3, PolicyStale: true},
			"degraded",
		},
		{
			"full-size chunk written with k=1 is stale under k=2",
			Chunk{Size: 1 << 20, K: 1, M: 1, Refs: 1, Placement: []NodeIdx{1, 2}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Readable: 2, PolicyStale: true},
			"degraded",
		},
		{
			"unknown node is trusted for state",
			Chunk{Size: 1 << 20, K: 2, M: 1, Refs: 1, Placement: []NodeIdx{1, 2, 9}},
			HealthInput{Nodes: nodes, Live: fakeLive{}, FillAfter: fa},
			ChunkHealth{Readable: 3},
			"committed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ChunkState(c.c, vol, c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v\nwant %+v", got, c.want)
			}
			if s := got.State(c.c.K, c.c.M); s != c.state {
				t.Fatalf("state %q want %q", s, c.state)
			}
			if got.RepairPending() != (len(c.want.Bad) > 0) {
				t.Fatalf("RepairPending %v", got.RepairPending())
			}
		})
	}
}

// Invariant: I-56
func TestSmallChunkRule(t *testing.T) {
	cases := []struct {
		size, cs uint32
		small    bool
	}{
		{0, 1 << 20, true},
		{1, 1 << 20, true},
		{SmallChunkMax - 1, 1 << 20, true},
		{SmallChunkMax, 1 << 20, true},
		{SmallChunkMax + 1, 1 << 20, false},
		{1 << 20, 1 << 20, false},
		// a full chunk is never small, however small the chunk size (test harnesses)
		{64 << 10, 64 << 10, false},
		{(64 << 10) - 1, 64 << 10, true},
		{4 << 10, 64 << 10, true},
	}
	for _, c := range cases {
		if got := IsSmallChunk(c.size, c.cs); got != c.small {
			t.Fatalf("IsSmallChunk(%d, %d) = %v, want %v", c.size, c.cs, got, c.small)
		}
	}
	vol := Volume{ChunkSize: 1 << 20, K: 3, M: 2}
	if k, m := TargetPolicy(Chunk{Size: 100, K: 1, M: 1}, vol); k != 1 || m != 2 {
		t.Fatalf("small: %d+%d", k, m)
	}
	if k, m := TargetPolicy(Chunk{Size: 1 << 20, K: 1, M: 1}, vol); k != 3 || m != 2 {
		t.Fatalf("full k=1: %d+%d", k, m)
	}
	if k, m := TargetPolicy(Chunk{Size: 100, K: 2, M: 1}, vol); k != 3 || m != 2 {
		t.Fatalf("small but striped: %d+%d", k, m)
	}
}
