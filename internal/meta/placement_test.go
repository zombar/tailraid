package meta

import (
	"encoding/binary"
	"math/rand"
	"testing"
)

func hashN(i uint64) Hash {
	var h Hash
	binary.LittleEndian.PutUint64(h[:], i)
	binary.LittleEndian.PutUint64(h[8:], i*0x9E3779B97F4A7C15)
	return h
}

func cands(weights ...uint64) []Candidate {
	out := make([]Candidate, len(weights))
	for i, w := range weights {
		out[i] = Candidate{Idx: NodeIdx(i + 1), Weight: w}
	}
	return out
}

func shuffled(r *rand.Rand, c []Candidate) []Candidate {
	out := append([]Candidate(nil), c...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// Invariant: I-8
func TestPlace_DeterministicAcrossNodes(t *testing.T) {
	seed := int64(1)
	r := rand.New(rand.NewSource(seed))
	base := cands(1, 1, 2, 3, 5, 8, 13)
	for i := uint64(0); i < 10000; i++ {
		h := hashN(i)
		want := Place(h, base, 3)
		for node := 0; node < 7; node++ {
			got := Place(h, shuffled(r, base), 3)
			if !equalIdx(got, want) {
				t.Fatalf("seed %d hash %d node %d: %v != %v", seed, i, node, got, want)
			}
		}
	}
}

// Invariant: I-9
func TestPlace_DistinctNodes(t *testing.T) {
	c := cands(1, 1, 1, 1, 1, 1)
	for i := uint64(0); i < 5000; i++ {
		p := Place(hashN(i), c, 6)
		if len(p) != 6 {
			t.Fatalf("len %d", len(p))
		}
		seen := map[NodeIdx]bool{}
		for _, n := range p {
			if n == 0 || seen[n] {
				t.Fatalf("hash %d: placement %v not distinct/non-zero", i, p)
			}
			seen[n] = true
		}
	}
}

func TestPlace_FewerCandidatesThanN(t *testing.T) {
	c := cands(1, 2)
	p := Place(hashN(1), c, 5)
	if len(p) != 2 {
		t.Fatalf("len %d want 2", len(p))
	}
	if got := Place(hashN(1), nil, 3); len(got) != 0 {
		t.Fatalf("no candidates: %v", got)
	}
	if got := Place(hashN(1), c, 0); len(got) != 0 {
		t.Fatalf("n=0: %v", got)
	}
}

func TestPlace_DuplicateCandidateIdxCollapses(t *testing.T) {
	c := []Candidate{{Idx: 1, Weight: 1}, {Idx: 1, Weight: 1}, {Idx: 2, Weight: 1}}
	p := Place(hashN(3), c, 3)
	if len(p) != 2 || p[0] == p[1] {
		t.Fatalf("got %v", p)
	}
}

// Invariant: I-33
func TestPlace_CapacityWeighting(t *testing.T) {
	c := cands(1, 2) // node 2 has twice the weight
	const N = 100000
	count := map[NodeIdx]int{}
	for i := uint64(0); i < N; i++ {
		count[Place(hashN(i), c, 1)[0]]++
	}
	ratio := float64(count[2]) / float64(count[1])
	if ratio < 1.8 || ratio > 2.2 {
		t.Fatalf("weight ratio 2:1 produced %d:%d (%.3f)", count[2], count[1], ratio)
	}
	// Three nodes 1:1:2 → share of node 3 ≈ 50 %.
	c3 := cands(1, 1, 2)
	count = map[NodeIdx]int{}
	for i := uint64(0); i < N; i++ {
		count[Place(hashN(i), c3, 1)[0]]++
	}
	if share := float64(count[3]) / N; share < 0.45 || share > 0.55 {
		t.Fatalf("node 3 share %.3f", share)
	}
}

// Invariant: I-33
func TestPlace_StabilityOnRemove(t *testing.T) {
	full := cands(1, 1, 1, 1, 1)
	without := full[:4] // remove node 5
	for i := uint64(0); i < 5000; i++ {
		a := Place(hashN(i), full, 3)
		b := Place(hashN(i), without, 3)
		had := false
		for _, n := range a {
			if n == 5 {
				had = true
			}
		}
		if !had && !equalIdx(a, b) {
			t.Fatalf("hash %d: placement %v changed to %v although node 5 was not used", i, a, b)
		}
		if had {
			// Nodes other than 5 keep their relative order; only 5's slot is replaced.
			ai, bi := 0, 0
			for ai < len(a) && bi < len(b) {
				if a[ai] == 5 {
					ai++
					continue
				}
				if a[ai] != b[bi] {
					// b may have inserted the newcomer anywhere after 5's removal; allow that only
					// if the newcomer is the node that was not in a.
					inA := false
					for _, n := range a {
						if n == b[bi] {
							inA = true
						}
					}
					if inA {
						t.Fatalf("hash %d: order of surviving nodes changed: %v -> %v", i, a, b)
					}
					bi++
					continue
				}
				ai++
				bi++
			}
		}
	}
}

// Invariant: I-33
func TestPlace_StabilityOnAdd(t *testing.T) {
	before := cands(1, 1, 1, 1, 1, 1, 1, 1, 1) // 9 nodes
	after := cands(1, 1, 1, 1, 1, 1, 1, 1, 1, 1)
	const N = 20000
	moved := 0
	for i := uint64(0); i < N; i++ {
		if Place(hashN(i), before, 1)[0] != Place(hashN(i), after, 1)[0] {
			moved++
		}
	}
	frac := float64(moved) / N
	if frac > 0.10+0.02 || frac < 0.10-0.02 {
		t.Fatalf("adding 1 of 10 nodes moved %.3f of first-choice slots, want ≈0.10", frac)
	}
}

// Invariant: I-33
func TestPlace_StabilityOnMembershipChange(t *testing.T) {
	TestPlace_StabilityOnRemove(t)
	TestPlace_StabilityOnAdd(t)
}

func TestCandidates_Filter(t *testing.T) {
	nodes := []Node{
		{Idx: 1, State: Active, Kind: KindDevice, Capacity: 10},
		{Idx: 2, State: Draining, Kind: KindDevice, Capacity: 10},
		{Idx: 3, State: Removed, Kind: KindDevice, Capacity: 10},
		{Idx: 4, State: Active, Kind: KindDevice, Capacity: 0},
		{Idx: 5, State: Active, Kind: KindS3, Capacity: 7},
		{Idx: 6, State: Active, Kind: KindDevice, Capacity: 3}, // dead
	}
	live := func(i NodeIdx) bool { return i != 6 }
	got := Candidates(nodes, live)
	want := []Candidate{{Idx: 1, Weight: 10}, {Idx: 5, Weight: 7}}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	// Input order does not matter; output is sorted by Idx.
	rev := append([]Node(nil), nodes...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	got2 := Candidates(rev, live)
	for i := range want {
		if got2[i] != want[i] {
			t.Fatalf("order-dependent: %v", got2)
		}
	}
}

// Invariant: I-8
func FuzzPlace_Determinism(f *testing.F) {
	f.Add(uint64(1), uint8(3), uint8(2), int64(7))
	f.Fuzz(func(t *testing.T, hseed uint64, ncand uint8, n uint8, shuf int64) {
		if ncand > 32 || n > 40 {
			return
		}
		c := make([]Candidate, ncand)
		for i := range c {
			c[i] = Candidate{Idx: NodeIdx(i + 1), Weight: uint64(i%5) + 1}
		}
		h := hashN(hseed)
		want := Place(h, c, int(n))
		r := rand.New(rand.NewSource(shuf))
		got := Place(h, shuffled(r, c), int(n))
		if !equalIdx(got, want) {
			t.Fatalf("%v != %v", got, want)
		}
		if int(n) <= int(ncand) && len(want) != int(n) || int(n) > int(ncand) && len(want) != int(ncand) {
			t.Fatalf("len %d for n=%d ncand=%d", len(want), n, ncand)
		}
		seen := map[NodeIdx]bool{}
		for _, x := range want {
			if x == 0 || seen[x] {
				t.Fatalf("not distinct: %v", want)
			}
			seen[x] = true
		}
	})
}

func BenchmarkPlace_10Nodes(b *testing.B) {
	c := cands(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	for i := 0; i < b.N; i++ {
		Place(hashN(uint64(i)), c, 3)
	}
}

// Invariant: I-9
func TestExcludeIdx(t *testing.T) {
	c := cands(1, 1, 1, 1)
	got := ExcludeIdx(c, []NodeIdx{2, 0, 4})
	want := []Candidate{{Idx: 1, Weight: 1}, {Idx: 3, Weight: 1}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v", got)
	}
	if n := len(ExcludeIdx(c, nil)); n != 4 {
		t.Fatalf("nil held: %d", n)
	}
	// Re-placing after excluding held nodes never repeats one of them.
	for i := uint64(0); i < 2000; i++ {
		held := Place(hashN(i), c, 2)
		for _, n := range Place(hashN(i), ExcludeIdx(c, held), 2) {
			if n == held[0] || n == held[1] {
				t.Fatalf("hash %d: re-placement %v overlaps held %v", i, n, held)
			}
		}
	}
}
