package meta

import (
	"testing"

	"github.com/zombar/tailraid/internal/testseed"
)

// vcands builds v voter candidates (idx 1..v) and s non-voter candidates after them.
func vcands(v, s int) []Candidate {
	out := make([]Candidate, 0, v+s)
	for i := 1; i <= v+s; i++ {
		out = append(out, Candidate{Idx: NodeIdx(i), Weight: 100, Voter: i <= v})
	}
	return out
}

func isVoter(c []Candidate, idx NodeIdx) bool {
	for _, x := range c {
		if x.Idx == idx {
			return x.Voter
		}
	}
	return false
}

func distinctNonZero(t *testing.T, p []NodeIdx, what string) {
	t.Helper()
	seen := map[NodeIdx]bool{}
	for _, n := range p {
		if n == 0 {
			continue
		}
		if seen[n] {
			t.Fatalf("%s: node %d twice in %v", what, n, p)
		}
		seen[n] = true
	}
}

// Invariant: I-77, I-9
func TestPlaceKM_VoterFirst_DataSlots(t *testing.T) {
	for v := 0; v <= 5; v++ {
		for s := 0; s <= 5; s++ {
			c := vcands(v, s)
			for k := 1; k <= 3; k++ {
				for m := 0; m <= 2; m++ {
					if k+m > v+s {
						continue
					}
					for i := uint64(0); i < 50; i++ {
						h := hashN(i*131 + uint64(v*7+s))
						p := PlaceKM(h, c, k, m)
						if len(p) != k+m {
							t.Fatalf("v=%d s=%d k=%d m=%d: len %d, want %d", v, s, k, m, len(p), k+m)
						}
						distinctNonZero(t, p, "PlaceKM")
						for _, n := range p {
							if n == 0 {
								t.Fatalf("v=%d s=%d k=%d m=%d: unfilled slot with enough candidates: %v", v, s, k, m, p)
							}
						}
						if v >= k {
							for _, n := range p[:k] {
								if !isVoter(c, n) {
									t.Fatalf("v=%d s=%d k=%d m=%d: data slot on non-voter %d in %v", v, s, k, m, n, p)
								}
							}
						}
						if v < k || s == 0 { // too few voters, or everyone is a voter: the plain rendezvous order
							if want := Place(h, c, k+m); !equalIdx(p, want) {
								t.Fatalf("v=%d s=%d k=%d m=%d: %v, want Place %v", v, s, k, m, p, want)
							}
						}
					}
				}
			}
		}
	}
	// fewer candidates than k+m: unfilled slots are 0, the rest distinct
	p := PlaceKM(hashN(9), vcands(1, 1), 2, 2)
	if len(p) != 4 || p[0] == 0 || p[1] == 0 || p[2] != 0 || p[3] != 0 {
		t.Fatalf("short candidates: %v", p)
	}
}

// Invariant: I-8, I-77
func TestPlaceKM_VoterFirst_NNodeDeterminism(t *testing.T) {
	r := testseed.Seed(t)
	base := vcands(3, 4)
	for i := uint64(0); i < 2000; i++ {
		h := hashN(i)
		want := PlaceKM(h, base, 2, 2)
		for n := 0; n < 4; n++ {
			if got := PlaceKM(h, shuffled(r, base), 2, 2); !equalIdx(got, want) {
				t.Fatalf("hash %d: node %d placed %v, node 0 placed %v", i, n, got, want)
			}
		}
	}
}

// Invariant: I-77
func TestFillPlacement_KeepsSlotClass(t *testing.T) {
	all := vcands(3, 3) // voters 1..3, non-voters 4..6
	const k, m = 2, 2
	for i := uint64(0); i < 200; i++ {
		h := hashN(i)
		// a fresh fill equals PlaceKM
		fresh := make([]NodeIdx, k+m)
		FillPlacement(h, fresh, all, k, nil)
		if want := PlaceKM(h, all, k, m); !equalIdx(fresh, want) {
			t.Fatalf("hash %d: fresh fill %v, PlaceKM %v", i, fresh, want)
		}
		// a data slot refills from voters, a parity slot from anyone; held nodes are never reused
		place := []NodeIdx{0, 2, 0, 5} // voter 2 holds data slot 1, non-voter 5 holds parity slot 3
		held := []NodeIdx{2, 5}
		FillPlacement(h, place, ExcludeIdx(all, held), k, nil)
		distinctNonZero(t, place, "refill")
		if place[0] == 0 || !isVoter(all, place[0]) {
			t.Fatalf("hash %d: data slot refilled with %d (want a voter) in %v", i, place[0], place)
		}
		if place[2] == 0 || place[2] == 2 || place[2] == 5 {
			t.Fatalf("hash %d: parity slot %v", i, place)
		}
	}
	// with no voter left, a data slot falls back to the non-voters
	place := []NodeIdx{0, 1, 0, 0}
	FillPlacement(hashN(1), place, ExcludeIdx(vcands(1, 3), []NodeIdx{1}), k, nil)
	distinctNonZero(t, place, "fallback")
	if place[0] == 0 || place[2] == 0 || place[3] == 0 {
		t.Fatalf("fallback left a slot empty: %v", place)
	}
}

// Candidates carries the voter bit for devices only.
// Invariant: I-77
func TestCandidates_VoterBit(t *testing.T) {
	nodes := []Node{
		{Idx: 1, Kind: KindDevice, State: Active, Capacity: 10, Voter: true},
		{Idx: 2, Kind: KindDevice, State: Active, Capacity: 10, Voter: false},
		{Idx: 3, Kind: KindS3, State: Active, Capacity: 10, Voter: true}, // meaningless on a bucket
	}
	got := Candidates(nodes, func(NodeIdx) bool { return true })
	if len(got) != 3 || !got[0].Voter || got[1].Voter || got[2].Voter {
		t.Fatalf("candidates %+v", got)
	}
}
