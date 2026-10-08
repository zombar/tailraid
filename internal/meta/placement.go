package meta

import (
	"hash/fnv"
	"math"
	"sort"
)

// Candidate is a node eligible to receive a shard. Weight is the node's Capacity.
type Candidate struct {
	Idx    NodeIdx
	Weight uint64
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
		out = append(out, Candidate{Idx: n.Idx, Weight: n.Capacity})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
	return out
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
