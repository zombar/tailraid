package meta

import (
	"math"
	"testing"
)

// The rebalance moves slots only between disks of one host, from the one furthest above
// its capacity share to the one furthest below, when both are past the skew; a host with
// one disk, a host whose disks are within the skew, and a dead receiver get nothing.
// Invariant: I-172
func TestRebalanceMoves_Table(t *testing.T) {
	disk := func(idx, host NodeIdx, capacity uint64) Node {
		return Node{Idx: idx, Kind: KindDisk, State: Active, Host: host, Capacity: capacity}
	}
	live := func(NodeIdx) bool { return true }
	for _, tc := range []struct {
		name   string
		nodes  []Node
		slots  map[NodeIdx]int
		chunks int
		live   func(NodeIdx) bool
		want   []RebalanceMove
	}{
		{"one disk per host: nothing", []Node{disk(2, 1, 1), disk(4, 3, 100)}, map[NodeIdx]int{2: 20, 4: 20}, 20, live, nil},
		{
			"small and large disk, all on the small",
			[]Node{disk(2, 1, 1), disk(3, 1, 10)},
			map[NodeIdx]int{2: 20, 3: 0},
			20, live,
			[]RebalanceMove{{Host: 1, From: 2, To: 3, N: 18}},
		}, // shares 1.8 and 18.2
		{"within the skew: nothing", []Node{disk(2, 1, 1), disk(3, 1, 1)}, map[NodeIdx]int{2: 11, 3: 9}, 20, live, nil},
		{
			"just past the skew",
			[]Node{disk(2, 1, 1), disk(3, 1, 1)},
			map[NodeIdx]int{2: 13, 3: 7},
			20, live,
			[]RebalanceMove{{Host: 1, From: 2, To: 3, N: 3}},
		},
		{"receiver dead: nothing", []Node{disk(2, 1, 1), disk(3, 1, 10)}, map[NodeIdx]int{2: 20, 3: 0}, 20, func(n NodeIdx) bool { return n != 3 }, nil},
		{"the cap of one slot per chunk", []Node{disk(2, 1, 1), disk(3, 1, 100)}, map[NodeIdx]int{2: 10, 3: 10}, 10, live, nil}, // share 10 and 10
		{
			"two hosts, each its own move",
			[]Node{disk(2, 1, 1), disk(3, 1, 10), disk(5, 4, 10), disk(6, 4, 1)},
			map[NodeIdx]int{2: 20, 3: 0, 5: 0, 6: 20},
			20, live,
			[]RebalanceMove{{Host: 1, From: 2, To: 3, N: 18}, {Host: 4, From: 6, To: 5, N: 18}},
		},
		{"inactive disk ignored", []Node{disk(2, 1, 1), {Idx: 3, Kind: KindDisk, State: Draining, Host: 1, Capacity: 10}}, map[NodeIdx]int{2: 20, 3: 0}, 20, live, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RebalanceMoves(tc.nodes, tc.slots, tc.chunks, DefaultRebalanceSkew, tc.live)
			if len(got) != len(tc.want) {
				t.Fatalf("moves %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("move %d: %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
	// shares: water-filling caps a disk at one slot per chunk and spreads the rest
	shares := HostShares([]Node{disk(2, 1, 1), disk(3, 1, 100)}, map[NodeIdx]int{2: 10, 3: 10}, 10)
	if math.Abs(shares[2]-10) > 1e-9 || math.Abs(shares[3]-10) > 1e-9 {
		t.Fatalf("capped shares %v, want 10 and 10", shares)
	}
	shares = HostShares([]Node{disk(2, 1, 1), disk(3, 1, 3)}, map[NodeIdx]int{2: 4, 3: 4}, 0)
	if math.Abs(shares[2]-2) > 1e-9 || math.Abs(shares[3]-6) > 1e-9 {
		t.Fatalf("uncapped shares %v, want 2 and 6", shares)
	}
}
