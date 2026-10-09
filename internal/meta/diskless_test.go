package meta

import (
	"testing"
	"time"
)

// A device row is never a candidate, whatever its Capacity or liveness; its disks are.
// Invariant: I-118
func TestCandidates_DevicesNeverTargets(t *testing.T) {
	nodes := topology([]int{2, 0, 1}, allVoters(3), 0) // devices 1, 4, 5; disks 2, 3, 6
	nodes[0].Capacity, nodes[3].Capacity = 10, 10      // a legacy budget on a device row is not a target
	cands := Candidates(nodes, allLive)
	want := []NodeIdx{2, 3, 6}
	if len(cands) != len(want) {
		t.Fatalf("candidates %+v, want %v", cands, want)
	}
	for i, c := range cands {
		if c.Idx != want[i] || c.Weight != 2 || !c.Voter {
			t.Fatalf("candidate %d = %+v, want disk %d weight 2 voter", i, c, want[i])
		}
		if c.Domain == 0 || c.Domain == c.Idx {
			t.Fatalf("candidate %d = %+v: domain must be its host", i, c)
		}
	}
	// the diskless host 4 contributes nothing even as the only live device
	only := Candidates(nodes, func(n NodeIdx) bool { return n == 4 })
	if len(only) != 0 {
		t.Fatalf("a diskless host is a candidate: %+v", only)
	}
}

// Usage sums disk budgets only: a device's legacy Capacity is not storage.
// Invariant: I-118
func TestUsage_DisksOnly(t *testing.T) {
	nodes := topology([]int{2, 1}, allVoters(2), 0) // 3 disks of 2 bytes each
	nodes[0].Capacity, nodes[3].Capacity = 100, 100 // devices with a legacy budget of 100: not storage
	vol := Volume{K: 1, M: 0}
	total, _, free := Usage(vol, nodes, Stats{})
	if total != 6 || free != 6 {
		t.Fatalf("Usage total %d free %d, want 6 and 6", total, free)
	}
	nodes = append(nodes, Node{Idx: 9, Kind: KindS3, State: Active, Capacity: 4})
	if total, _, _ := Usage(vol, nodes, Stats{}); total != 10 {
		t.Fatalf("Usage with an S3 node %d, want 10", total)
	}
}

// A host with no disks counts for nothing in the automatic policy, the tolerance and the
// live-target precondition: four hosts with one disk each plus a diskless voter is 3+1.
// Invariant: I-118
func TestAutoPolicy_DisklessHostNotCounted(t *testing.T) {
	with := topology([]int{1, 1, 1, 1, 0}, allVoters(5), 0)
	without := topology([]int{1, 1, 1, 1}, allVoters(4), 0)
	k1, m1 := AutoPolicy(with)
	k2, m2 := AutoPolicy(without)
	if k1 != k2 || m1 != m2 {
		t.Fatalf("policy with a diskless voter %d+%d, without %d+%d", k1, m1, k2, m2)
	}
	d1, h1 := Tolerance(with, k1, m1)
	d2, h2 := Tolerance(without, k2, m2)
	if d1 != d2 || h1 != h2 {
		t.Fatalf("tolerance with a diskless voter %d/%d, without %d/%d", d1, h1, d2, h2)
	}
	// a cluster of diskless hosts has no targets: no policy is allowed, and the rule
	// still answers 1+0 (the init value) rather than failing
	none := topology([]int{0, 0, 0}, allVoters(3), 0)
	if k, m := AutoPolicy(none); k != 1 || m != 0 {
		t.Fatalf("policy with no disks %d+%d, want 1+0", k, m)
	}
	if err := PolicyAllowed(none, liveAll{}, 1, 0); err == nil {
		t.Fatal("1+0 allowed with no live target")
	}
	if err := PolicyAllowed(with, liveAll{}, k1, m1); err != nil {
		t.Fatalf("%d+%d not allowed with four disks: %v", k1, m1, err)
	}
}

type liveAll struct{}

func (liveAll) IsLive(NodeIdx) bool           { return true }
func (liveAll) DeadFor(NodeIdx) time.Duration { return 0 }
