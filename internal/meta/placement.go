package meta

import (
	"hash/fnv"
	"math"
	"sort"
)

// Candidate is a node eligible to receive a shard. Weight is the node's Capacity; Voter
// marks an always-on device, which the data slots prefer (docs/03 §Voter-first slots).
type Candidate struct {
	Idx    NodeIdx
	Weight uint64
	Voter  bool
}

// Candidates filters the node table to placement candidates: Active, live, with
// non-zero capacity. Both device and S3 nodes qualify. The result is sorted by Idx.
func Candidates(nodes []Node, live func(NodeIdx) bool) []Candidate {
	out := make([]Candidate, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if n.State != Active || n.Capacity == 0 || n.Idx == 0 || !live(n.Idx) {
			continue
		}
		out = append(out, Candidate{Idx: n.Idx, Weight: n.Capacity, Voter: n.Voter && n.Kind == KindDevice})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
	return out
}

// PlaceKM places a k+m stripe: the k data slots by rendezvous over the voter candidates
// when at least k voters are present (otherwise over all candidates), then the m parity
// slots by rendezvous over the candidates not yet used. Unfilled slots are 0. With no
// voter candidate the result equals Place(h, cands, k+m); it is a pure function of
// (h, candidate set, k, m) (I-8, I-77).
func PlaceKM(h Hash, cands []Candidate, k, m int) []NodeIdx {
	place := make([]NodeIdx, max(k, 0)+max(m, 0))
	FillPlacement(h, place, cands, k)
	return place
}

// FillPlacement fills every zero slot of place from cands (the caller has already
// excluded the nodes place holds): zero data slots (index < k) from voters first, zero
// parity slots from anyone left. Slots for which no candidate remains stay 0. Placement
// by hash keeps it the same answer on every node (I-8).
func FillPlacement(h Hash, place []NodeIdx, cands []Candidate, k int) {
	needData, needParity := 0, 0
	for i, n := range place {
		if n != 0 {
			continue
		}
		if i < k {
			needData++
		} else {
			needParity++
		}
	}
	var voters []Candidate
	for _, c := range cands {
		if c.Voter {
			voters = append(voters, c)
		}
	}
	data := Place(h, voters, needData)
	if len(data) < needData { // too few voters: data slots fall back to everyone
		data = Place(h, cands, needData)
	}
	parity := Place(h, ExcludeIdx(cands, data), needParity)
	di, pi := 0, 0
	for i := range place {
		if place[i] != 0 {
			continue
		}
		if i < k {
			if di < len(data) {
				place[i] = data[di]
				di++
			}
		} else if pi < len(parity) {
			place[i] = parity[pi]
			pi++
		}
	}
}

// Place selects up to n distinct nodes for the shards of chunk h by weighted rendezvous
// hashing: score_i = -Weight_i / ln(u_i) with u_i derived from fnv64a(h ‖ idx). The
// result is sorted by score descending (ties by Idx ascending) and is a pure function of
// (h, set of candidates, n) regardless of input order (I-8). Shard i goes to result[i].
// With fewer than n candidates the result is shorter; duplicate Idx entries count once.
func Place(h Hash, cands []Candidate, n int) []NodeIdx {
	if n <= 0 || len(cands) == 0 {
		return nil
	}
	type scored struct {
		idx   NodeIdx
		score float64
	}
	seen := make(map[NodeIdx]bool, len(cands))
	sc := make([]scored, 0, len(cands))
	var buf [34]byte
	copy(buf[:32], h[:])
	for _, c := range cands {
		if c.Idx == 0 || c.Weight == 0 || seen[c.Idx] {
			continue
		}
		seen[c.Idx] = true
		buf[32] = byte(c.Idx)
		buf[33] = byte(c.Idx >> 8)
		f := fnv.New64a()
		f.Write(buf[:])
		// FNV-1a has almost no avalanche on its last input bytes: hashes for adjacent
		// idx values differ by a small multiple of the FNV prime, which made the weight
		// term dominate. The murmur3 finalizer fixes that while keeping the function pure.
		x := fmix64(f.Sum64())
		// Map to (0,1) strictly: use the top 53 bits plus one half, so u is never 0 or 1.
		u := (float64(x>>11) + 0.5) / float64(uint64(1)<<53)
		sc = append(sc, scored{idx: c.Idx, score: -float64(c.Weight) / math.Log(u)})
	}
	sort.Slice(sc, func(i, j int) bool {
		if sc[i].score != sc[j].score {
			return sc[i].score > sc[j].score
		}
		return sc[i].idx < sc[j].idx
	})
	if n > len(sc) {
		n = len(sc)
	}
	out := make([]NodeIdx, n)
	for i := range out {
		out[i] = sc[i].idx
	}
	return out
}

// fmix64 is the MurmurHash3 64-bit finalizer.
func fmix64(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// ExcludeIdx returns cands without the nodes listed in held (zero entries ignored). Used
// when re-placing failed slots so a node never receives two shards of one stripe (I-9).
func ExcludeIdx(cands []Candidate, held []NodeIdx) []Candidate {
	skip := make(map[NodeIdx]bool, len(held))
	for _, h := range held {
		if h != 0 {
			skip[h] = true
		}
	}
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if !skip[c.Idx] {
			out = append(out, c)
		}
	}
	return out
}
