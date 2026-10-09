package meta

import "testing"

// A device row with a budget, or a slot that names a device, marks a state from before
// devices stopped being targets; a disks-only state is clean (docs/14 §9).
// Invariant: I-119
func TestLegacyDeviceTargets_Table(t *testing.T) {
	chunks := func(placements ...[]NodeIdx) func(func(Chunk) bool) {
		return func(fn func(Chunk) bool) {
			for _, p := range placements {
				if !fn(Chunk{Placement: p}) {
					return
				}
			}
		}
	}
	clean := topology([]int{1, 1}, allVoters(2), 0) // devices 1, 3; disks 2, 4
	budgeted := topology([]int{1, 1}, allVoters(2), 0)
	budgeted[0].Capacity, budgeted[2].Capacity = 5, 5
	cases := []struct {
		name    string
		nodes   []Node
		chunks  func(func(Chunk) bool)
		devices int
		slots   int
	}{
		{"disks only, slots on disks", clean, chunks([]NodeIdx{2, 4}), 0, 0},
		{"a device with a budget", budgeted, chunks([]NodeIdx{2, 4}), 2, 0},
		{"a slot on a device", clean, chunks([]NodeIdx{1, 4}, []NodeIdx{2, 3}), 0, 2},
		{"a degraded slot (0) is not a device", clean, chunks([]NodeIdx{0, 4}), 0, 0},
		{"a removed device with a budget is history", append(clean, Node{Idx: 9, Kind: KindDevice, State: Removed, Capacity: 7}), nil, 0, 0},
		{"no chunks at all", clean, nil, 0, 0},
	}
	for _, tc := range cases {
		devs, slots := LegacyDeviceTargets(tc.nodes, tc.chunks)
		if len(devs) != tc.devices || slots != tc.slots {
			t.Errorf("%s: %d devices, %d slots; want %d, %d", tc.name, len(devs), slots, tc.devices, tc.slots)
		}
	}
}

// HasTargets is whether a chunk can be placed anywhere: an Active disk of an Active
// device, or an Active S3 node; a device row alone is nothing (docs/14 §9).
// Invariant: I-120
func TestHasTargets_Table(t *testing.T) {
	disk := Node{Idx: 2, Kind: KindDisk, State: Active, Capacity: 2, Host: 1}
	cases := []struct {
		name  string
		nodes []Node
		want  bool
	}{
		{"empty", nil, false},
		{"a device alone", []Node{{Idx: 1, Kind: KindDevice, State: Active}}, false},
		{"two devices, no disks", []Node{{Idx: 1, Kind: KindDevice, State: Active}, {Idx: 2, Kind: KindDevice, State: Active}}, false},
		{"a device with a disk", []Node{devices(1, 0)[0], disk}, true},
		{"a draining disk", []Node{devices(1, 0)[0], {Idx: 2, Kind: KindDisk, State: Draining, Capacity: 2, Host: 1}}, false},
		{"a disk of a draining host", []Node{{Idx: 1, Kind: KindDevice, State: Draining}, disk}, false},
		{"a disk of a removed host", []Node{{Idx: 1, Kind: KindDevice, State: Removed}, disk}, false},
		{"an S3 node", []Node{devices(1, 0)[0], {Idx: 2, Kind: KindS3, State: Active, Capacity: 2}}, true},
		{"a device with a legacy budget", []Node{{Idx: 1, Kind: KindDevice, State: Active, Capacity: 9}}, false},
	}
	for _, tc := range cases {
		if got := HasTargets(tc.nodes); got != tc.want {
			t.Errorf("%s: HasTargets = %v, want %v", tc.name, got, tc.want)
		}
	}
}
