package meta

import "slices"

// HomeView is what home placement (docs/14 §7) needs from the node table, built once per
// pass: the home candidates (every Active target counted live), their domains and which
// targets are S3 nodes. A chunk's home depends on its hash and this view only, never on
// liveness (I-294).
type HomeView struct {
	cands   []Candidate
	dom     Domains
	s3      map[NodeIdx]bool
	domains int // distinct domains among the home candidates
}

// NewHomeView builds the view of a node table.
func NewHomeView(nodes []Node) *HomeView {
	v := &HomeView{
		cands: Candidates(nodes, func(NodeIdx) bool { return true }),
		dom:   DomainsOf(nodes),
		s3:    map[NodeIdx]bool{},
	}
	seen := map[NodeIdx]bool{}
	for _, c := range v.cands {
		if !seen[c.domain()] {
			seen[c.domain()] = true
			v.domains++
		}
	}
	for i := range nodes {
		if nodes[i].Kind == KindS3 {
			v.s3[nodes[i].Idx] = true
		}
	}
	return v
}

// Same reports whether o gives every chunk the same home as v: the same candidates,
// weights, voter bits and domains (a membership change re-evaluates the gate, I-295).
func (v *HomeView) Same(o *HomeView) bool { return slices.Equal(v.cands, o.cands) }

// Domain is n's failure domain: its host for a disk, itself otherwise.
func (v *HomeView) Domain(n NodeIdx) NodeIdx { return v.dom.Of(n) }

// S3 reports whether n is an S3 node, which a placement move never targets (I-294).
func (v *HomeView) S3(n NodeIdx) bool { return v.s3[n] }

// Home is the placement a k+m stripe of h gets when every Active target is live: what a
// write lands on with nothing down (I-294).
func (v *HomeView) Home(h Hash, k, m int) []NodeIdx { return PlaceKM(h, v.cands, k, m) }

// Away lists the slots of place whose target is not in home for the slot's class (data
// below k, parity from k), in slot order. Zero slots are not away (they are missing), nor
// are slots on an S3 node: a bucket's shard is never moved for placement (I-294).
func (v *HomeView) Away(place []NodeIdx, k int, home []NodeIdx) []int {
	var out []int
	for i, n := range place {
		if n == 0 || v.s3[n] {
			continue
		}
		if !inClass(home, k, i, n) {
			out = append(out, i)
		}
	}
	return out
}

// FillHome fills the zero slots of place, in slot order, each with a target of its home
// class that holds no slot of place, passes can, and keeps its domain within the host cap
// counting the slots place holds (I-296). It returns the slots it filled; the others stay 0
// for FillPlacement.
func (v *HomeView) FillHome(place []NodeIdx, k int, home []NodeIdx, can func(NodeIdx) bool) []int {
	held := map[NodeIdx]bool{}
	count := map[NodeIdx]int{}
	for _, n := range place {
		if n != 0 {
			held[n] = true
			count[v.dom.Of(n)]++
		}
	}
	// the cap of a move toward home is the home's own count for each host, or
	// ceil(n/D) where that is more: where a host has too few targets the second walk of
	// Place gave another host more, and reaching that home is not a cap break (docs/14 §7)
	limit := map[NodeIdx]int{}
	for _, n := range home {
		if n != 0 {
			limit[v.dom.Of(n)]++
		}
	}
	base := hostCap(len(place), v.domains)
	var filled []int
	for i := range place {
		if place[i] != 0 {
			continue
		}
		lo, hi := classRange(home, k, i)
		for _, t := range home[lo:hi] {
			if t == 0 || held[t] || !can(t) || count[v.dom.Of(t)] >= max(base, limit[v.dom.Of(t)]) {
				continue
			}
			place[i] = t
			held[t] = true
			count[v.dom.Of(t)]++
			filled = append(filled, i)
			break
		}
	}
	return filled
}

// FillAway moves the away slots of a chunk home in one step (docs/14 §7): place is the
// placement with every away slot zeroed, orig the placement as it is and away the slots
// zeroed. Each zero slot goes to a home target of its class through FillHome, an away
// slot no target can take keeps its holder, and a slot bound for the holder of a slot
// that keeps it is given back too, since a target never holds two slots of a chunk
// (I-9), until the step is consistent. It returns the slots that move, in slot order.
func (v *HomeView) FillAway(place, orig []NodeIdx, away []int, k int, home []NodeIdx, can func(NodeIdx) bool) []int {
	moved := map[int]bool{}
	for _, i := range v.FillHome(place, k, home, can) {
		moved[i] = true
	}
	for _, i := range away {
		if place[i] == 0 {
			place[i] = orig[i]
		}
	}
	for again := true; again; {
		again = false
		for i := range place {
			if !moved[i] {
				continue
			}
			for j, n := range place {
				if j != i && n == place[i] && !moved[j] {
					place[i] = orig[i] // its target is a holder that stays
					delete(moved, i)
					again = true
					break
				}
			}
		}
	}
	if len(moved) == 0 {
		return nil
	}
	out := make([]int, 0, len(moved))
	for i := range moved {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// SlotMove is one slot a home move takes from one target to another (docs/14 §7).
type SlotMove struct {
	Slot     int
	From, To NodeIdx
}

// Moves lists the away slots of c that FillAway would move now, with dest deciding which
// targets can take a shard (live, not suspect, room): an away slot whose home cannot take
// it does not move (L-220), nor one bound for the holder of such a slot, and no slot
// moves onto an S3 node (I-294). A chunk whose slots are home, a tombstone, or a
// placement not of its own (k, m) returns nil.
func (v *HomeView) Moves(c Chunk, dest func(NodeIdx) bool) []SlotMove {
	if c.Refs == 0 || len(c.Placement) != int(c.K)+int(c.M) {
		return nil
	}
	k := int(c.K)
	home := v.Home(c.Hash, k, int(c.M))
	away := v.Away(c.Placement, k, home)
	if len(away) == 0 {
		return nil
	}
	place := append([]NodeIdx(nil), c.Placement...)
	for _, i := range away {
		place[i] = 0
	}
	var out []SlotMove
	for _, i := range v.FillAway(place, c.Placement, away, k, home, func(n NodeIdx) bool { return !v.s3[n] && dest(n) }) {
		out = append(out, SlotMove{Slot: i, From: c.Placement[i], To: place[i]})
	}
	return out
}

// Movable is the slot indices of Moves.
func (v *HomeView) Movable(c Chunk, dest func(NodeIdx) bool) []int {
	moves := v.Moves(c, dest)
	if moves == nil {
		return nil
	}
	out := make([]int, 0, len(moves))
	for _, mv := range moves {
		out = append(out, mv.Slot)
	}
	return out
}

// classRange is the index range of home that slot i's class covers.
func classRange(home []NodeIdx, k, i int) (int, int) {
	k = min(k, len(home))
	if i < k {
		return 0, k
	}
	return k, len(home)
}

// inClass reports whether n is one of home's targets for slot i's class.
func inClass(home []NodeIdx, k, i int, n NodeIdx) bool {
	lo, hi := classRange(home, k, i)
	return slices.Contains(home[lo:hi], n)
}
