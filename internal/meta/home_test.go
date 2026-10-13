package meta

import (
	"slices"
	"testing"

	"github.com/zombar/tailraid/internal/testseed"
)

// applyHome is what the repairer does with Movable: every away slot is emptied, FillHome
// fills what it can, and the rest keep their holder.
func applyHome(v *HomeView, c Chunk, dest func(NodeIdx) bool) []NodeIdx {
	k := int(c.K)
	home := v.Home(c.Hash, k, int(c.M))
	place := slices.Clone(c.Placement)
	away := v.Away(place, k, home)
	for _, i := range away {
		place[i] = 0
	}
	v.FillHome(place, k, home, func(n NodeIdx) bool { return !v.s3[n] && dest(n) })
	for _, i := range away {
		if place[i] == 0 {
			place[i] = c.Placement[i]
		}
	}
	return place
}

// writePlace is the flusher's placement with the given targets live.
func writePlace(h Hash, nodes []Node, k, m int, live func(NodeIdx) bool) []NodeIdx {
	place := make([]NodeIdx, k+m)
	FillPlacement(h, place, Candidates(nodes, live), k, DomainsOf(nodes))
	return place
}

// The home is the placement a write gets with every Active target live, the same on
// every node whatever the order of its node table.
// Invariant: I-294
func TestHome_EqualsWritePlacement(t *testing.T) {
	for _, nodes := range [][]Node{
		topology([]int{1, 1, 1, 1, 1}, []bool{true, true, true, false, false}, 0),
		topology([]int{3, 2, 1, 2}, allVoters(4), 0),
		topology([]int{4}, allVoters(1), 0),
	} {
		v := NewHomeView(nodes)
		for i := range uint64(500) {
			h := hashN(i)
			if got, want := v.Home(h, 3, 2), writePlace(h, nodes, 3, 2, allLive); !slices.Equal(got, want) {
				t.Fatalf("hash %d: home %v, a write with everything live %v", i, got, want)
			}
		}
	}
}

// Invariant: I-294
func TestHome_NNodeDeterminism(t *testing.T) {
	seed, r := testseed.New(t)
	nodes := topology([]int{3, 2, 2, 1}, []bool{true, true, false, false}, 0)
	ref := NewHomeView(nodes)
	for node := range 5 {
		shuffled := slices.Clone(nodes)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		v := NewHomeView(shuffled)
		for i := range uint64(300) {
			c := Chunk{Hash: hashN(i), K: 3, M: 2, Refs: 1, Placement: writePlace(hashN(i), nodes, 3, 2, func(n NodeIdx) bool { return n != 3 })}
			if a, b := ref.Home(c.Hash, 3, 2), v.Home(c.Hash, 3, 2); !slices.Equal(a, b) {
				t.Fatalf("seed %d node %d hash %d: home %v vs %v", seed, node, i, a, b)
			}
			if a, b := ref.Movable(c, allLive), v.Movable(c, allLive); !slices.Equal(a, b) {
				t.Fatalf("seed %d node %d hash %d: movable %v vs %v", seed, node, i, a, b)
			}
		}
	}
}

// A chunk written while a target was down converges home in one application, keeps the
// host cap and one slot per target, and then has nothing to move: a second pass moves
// nothing (I-294). A home that cannot take a shard is never a reason to move elsewhere.
// Invariant: I-294
// Lifecycle: L-220
func TestHomeMoves_Idempotent(t *testing.T) {
	nodes := topology([]int{2, 2, 2, 1}, []bool{true, true, true, false}, 0)
	v := NewHomeView(nodes)
	dom := DomainsOf(nodes)
	for down := NodeIdx(2); down <= 10; down++ {
		for i := range uint64(200) {
			h := hashN(i)
			c := Chunk{Hash: h, K: 3, M: 2, Refs: 1, Placement: writePlace(h, nodes, 3, 2, func(n NodeIdx) bool { return n != down })}
			// while the target is still down, nothing moves toward it and nothing moves elsewhere
			stuck := applyHome(v, c, func(n NodeIdx) bool { return n != down })
			for j, n := range stuck {
				if n != c.Placement[j] && !inClass(v.Home(h, 3, 2), 3, j, n) {
					t.Fatalf("down %d hash %d: slot %d moved to %d, not its home", down, i, j, n)
				}
			}
			next := applyHome(v, c, allLive)
			if a := v.Away(next, 3, v.Home(h, 3, 2)); len(a) != 0 {
				t.Fatalf("down %d hash %d: %v still away after the move (%v → %v)", down, i, a, c.Placement, next)
			}
			c.Placement = next
			if m := v.Movable(c, allLive); m != nil {
				t.Fatalf("down %d hash %d: a second pass would move %v", down, i, m)
			}
			seen := map[NodeIdx]bool{}
			count := map[NodeIdx]int{}
			for _, n := range next {
				if n == 0 || seen[n] {
					t.Fatalf("down %d hash %d: placement %v has a zero or a repeat", down, i, next)
				}
				seen[n] = true
				count[dom.Of(n)]++
			}
			for d, n := range count {
				if n > hostCap(5, 4) {
					t.Fatalf("down %d hash %d: host %d holds %d slots", down, i, d, n)
				}
			}
		}
	}
}

// Invariant: I-294, I-296
// Lifecycle: L-220
func TestHomeMoves_Table(t *testing.T) {
	// hosts 1, 4, 6 are voters with disks 2,3 / 5 / 7; host 8 is a laptop with disk 9; 10 is a bucket
	nodes := []Node{
		{Idx: 1, Kind: KindDevice, State: Active, Voter: true},
		{Idx: 2, Kind: KindDisk, State: Active, Host: 1, Capacity: 4},
		{Idx: 3, Kind: KindDisk, State: Active, Host: 1, Capacity: 4},
		{Idx: 4, Kind: KindDevice, State: Active, Voter: true},
		{Idx: 5, Kind: KindDisk, State: Active, Host: 4, Capacity: 4},
		{Idx: 6, Kind: KindDevice, State: Active, Voter: true},
		{Idx: 7, Kind: KindDisk, State: Active, Host: 6, Capacity: 4},
		{Idx: 8, Kind: KindDevice, State: Active},
		{Idx: 9, Kind: KindDisk, State: Active, Host: 8, Capacity: 4},
		{Idx: 10, Kind: KindS3, State: Active, Capacity: 4},
	}
	v := NewHomeView(nodes)
	// find a hash whose 2+2 home puts both data slots on voters and a parity slot on 9
	var h Hash
	var home []NodeIdx
	for i := range uint64(10000) {
		home = v.Home(hashN(i), 2, 2)
		if slices.Contains(home[2:], 9) && !slices.Contains(home, 10) {
			h = hashN(i)
			break
		}
	}
	if home == nil || !slices.Contains(home[2:], 9) {
		t.Fatal("no hash with the laptop in its parity")
	}
	// a target of no class of the home, on a voter host under the cap
	var spare NodeIdx
	for _, n := range []NodeIdx{2, 3, 5, 7} {
		if !slices.Contains(home, n) {
			spare = n
			break
		}
	}
	swap := func(p []NodeIdx, i, j int) []NodeIdx { p = slices.Clone(p); p[i], p[j] = p[j], p[i]; return p }
	set := func(p []NodeIdx, i int, n NodeIdx) []NodeIdx { p = slices.Clone(p); p[i] = n; return p }
	not := func(x NodeIdx) func(NodeIdx) bool { return func(n NodeIdx) bool { return n != x } }
	laptopIdx := slices.Index(home, 9)
	for _, tc := range []struct {
		name  string
		c     Chunk
		dest  func(NodeIdx) bool
		moves []int
	}{
		{"at home: nothing", Chunk{Hash: h, K: 2, M: 2, Refs: 1, Placement: home}, allLive, nil},
		{"a tombstone: nothing", Chunk{Hash: h, K: 2, M: 2, Placement: set(home, 0, spare)}, allLive, nil},
		{"data on the laptop, home free: moves", Chunk{Hash: h, K: 2, M: 2, Refs: 1, Placement: swap(home, 0, laptopIdx)}, allLive, []int{0, laptopIdx}},
		{"data slot on a spare voter disk: moves", Chunk{Hash: h, K: 2, M: 2, Refs: 1, Placement: set(home, 1, spare)}, allLive, []int{1}},
		{"home dead: nothing (L-220)", Chunk{Hash: h, K: 2, M: 2, Refs: 1, Placement: set(home, 1, spare)}, not(home[1]), nil},
		{"on the bucket: never away", Chunk{Hash: h, K: 2, M: 2, Refs: 1, Placement: set(home, laptopIdx, 10)}, allLive, nil},
		{"home is the bucket: never moved onto it", Chunk{Hash: h, K: 2, M: 2, Refs: 1, Placement: set(home, laptopIdx, spare)}, allLive, []int{laptopIdx}},
		{"a missing slot is not away", Chunk{Hash: h, K: 2, M: 2, Refs: 1, Placement: set(home, 0, 0)}, allLive, nil},
		{"a placement of the wrong length: nothing", Chunk{Hash: h, K: 2, M: 1, Refs: 1, Placement: home}, allLive, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := v.Movable(tc.c, tc.dest); !slices.Equal(got, tc.moves) {
				t.Fatalf("movable %v, want %v (home %v, placement %v)", got, tc.moves, home, tc.c.Placement)
			}
		})
	}
	// with the bucket home and the bucket the only free home target: nothing moves onto it
	for i := range uint64(10000) {
		hb := v.Home(hashN(i), 2, 2)
		j := slices.Index(hb, 10)
		if j < 2 {
			continue
		}
		var off NodeIdx
		for _, n := range []NodeIdx{2, 3, 5, 7, 9} {
			if !slices.Contains(hb, n) {
				off = n
				break
			}
		}
		c := Chunk{Hash: hashN(i), K: 2, M: 2, Refs: 1, Placement: set(hb, j, off)}
		if got := v.Movable(c, allLive); got != nil {
			t.Fatalf("a slot moved onto the bucket: %v (home %v, placement %v)", got, hb, c.Placement)
		}
		return
	}
	t.Fatal("no hash with the bucket in its parity")
}

// FillHome fills a missing slot from its home class and leaves it for FillPlacement when
// the home target cannot take it (I-296).
// Invariant: I-296
func TestFillHome_RefillsFromHome(t *testing.T) {
	nodes := topology([]int{1, 1, 1, 1, 1}, allVoters(5), 0)
	v := NewHomeView(nodes)
	h := hashN(7)
	home := v.Home(h, 3, 2)
	place := slices.Clone(home)
	place[1], place[4] = 0, 0
	if got := v.FillHome(place, 3, home, allLive); !slices.Equal(got, []int{1, 4}) || !slices.Equal(place, home) {
		t.Fatalf("filled %v to %v, want home %v", got, place, home)
	}
	place = slices.Clone(home)
	place[1] = 0
	if got := v.FillHome(place, 3, home, func(n NodeIdx) bool { return n != home[1] }); got != nil || place[1] != 0 {
		t.Fatalf("filled %v to %v with its home dead", got, place)
	}
}
