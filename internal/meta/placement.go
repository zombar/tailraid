package meta

import (
	"hash/fnv"
	"math"
	"slices"
	"sort"
)

// Candidate is a target eligible to receive a shard: a disk or an S3 node, never a device
// row (docs/14 §9). Weight is its Capacity; Voter marks a target on an always-on device,
// which the data slots prefer (docs/03 §Voter-first slots); Domain is the failure domain,
// the host for a disk and the target itself otherwise (0 means Idx) (docs/14 §5).
type Candidate struct {
	Idx    NodeIdx
	Weight uint64
	Voter  bool
	Domain NodeIdx
}

func (c Candidate) domain() NodeIdx {
	if c.Domain != 0 {
		return c.Domain
	}
	return c.Idx
}

// Domains maps a disk to its host; every other target is its own domain. The zero value
// (nil) makes every target its own domain.
type Domains map[NodeIdx]NodeIdx

// Of is n's failure domain.
func (d Domains) Of(n NodeIdx) NodeIdx {
	if h, ok := d[n]; ok {
		return h
	}
	return n
}

// DomainsOf builds the domain map of a node table.
func DomainsOf(nodes []Node) Domains {
	d := Domains{}
	for i := range nodes {
		if nodes[i].Kind == KindDisk {
			d[nodes[i].Idx] = nodes[i].Host
		}
	}
	return d
}

// Candidates filters the node table to placement candidates: Active, live, with
// non-zero capacity; a disk also needs its host Active, and takes the host's voter bit
// and the host as its domain. Disk and S3 targets qualify; a device row never does
// (docs/14 §9, I-118). Sorted by Idx.
func Candidates(nodes []Node, live func(NodeIdx) bool) []Candidate {
	hosts := make(map[NodeIdx]*Node, len(nodes))
	for i := range nodes {
		if nodes[i].Kind == KindDevice {
			hosts[nodes[i].Idx] = &nodes[i]
		}
	}
	out := make([]Candidate, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if n.Kind == KindDevice || n.State != Active || n.Capacity == 0 || n.Idx == 0 || !live(n.Idx) {
			continue
		}
		c := Candidate{Idx: n.Idx, Weight: n.Capacity}
		if n.Kind == KindDisk {
			h := hosts[n.Host]
			if h == nil || h.State != Active {
				continue
			}
			c.Voter, c.Domain = h.Voter, h.Idx
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
	return out
}

// PlaceKM places a k+m stripe: the k data slots by rendezvous over the voter candidates
// when at least k voters are present (otherwise over all candidates), then the m parity
// slots by rendezvous over the candidates not yet used, both under the host cap. Unfilled
// slots are 0. With no voter candidate the result equals Place(h, cands, k+m); it is a
// pure function of (h, candidate set, k, m) (I-8, I-77, I-87).
func PlaceKM(h Hash, cands []Candidate, k, m int) []NodeIdx {
	place := make([]NodeIdx, max(k, 0)+max(m, 0))
	FillPlacement(h, place, cands, k, nil)
	return place
}

// FillPlacement fills every zero slot of place from cands (the caller has already
// excluded the targets place holds): zero data slots (index < k) from voters first, zero
// parity slots from anyone left. No domain receives more than ceil(len(place)/D) slots,
// counting the slots already held (dom names their domains), while another choice exists
// (I-87). Slots for which no candidate remains stay 0. Placement by hash keeps it the
// same answer on every node (I-8).
func FillPlacement(h Hash, place []NodeIdx, cands []Candidate, k int, dom Domains) {
	needData, needParity := 0, 0
	count := map[NodeIdx]int{}
	for i, n := range place {
		if n != 0 {
			count[dom.Of(n)]++
			continue
		}
		if i < k {
			needData++
		} else {
			needParity++
		}
	}
	domains := map[NodeIdx]bool{}
	for d := range count {
		domains[d] = true
	}
	var voters []Candidate
	for _, c := range cands {
		domains[c.domain()] = true
		if c.Voter {
			voters = append(voters, c)
		}
	}
	limit := hostCap(len(place), len(domains))
	var data []NodeIdx
	if rv := ranked(h, voters); len(rv) < needData { // too few voters: data slots fall back to everyone
		data = pick(ranked(h, cands), needData, count, limit)
	} else {
		// the cap outranks the vote (I-173): voters under it, then anyone under it, and
		// only then the candidates the cap skipped
		data = pickUnder(rv, needData, count, limit)
		if len(data) < needData {
			data = append(data, pickUnder(ranked(h, ExcludeIdx(cands, data)), needData-len(data), count, limit)...)
		}
		if len(data) < needData {
			data = append(data, pickOver(ranked(h, ExcludeIdx(cands, data)), needData-len(data), count)...)
		}
	}
	parity := pick(ranked(h, ExcludeIdx(cands, data)), needParity, count, limit)
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

// hostCap is ceil(n/d), the most slots of an n-slot stripe one of d domains may hold.
func hostCap(n, d int) int {
	if d <= 0 {
		return n
	}
	return (n + d - 1) / d
}

// pick takes up to n targets from r (in rank order) under the domain cap, then fills any
// slot left from the targets the cap skipped; count holds the slots each domain already
// has and is updated.
func pick(r []Candidate, n int, count map[NodeIdx]int, limit int) []NodeIdx {
	out := pickUnder(r, n, count, limit)
	if len(out) < n {
		out = append(out, pickOver(ExcludeIdx(r, out), n-len(out), count)...)
	}
	return out
}

// pickUnder takes up to n targets from r in rank order whose domain is under the cap.
func pickUnder(r []Candidate, n int, count map[NodeIdx]int, limit int) []NodeIdx {
	var out []NodeIdx
	for _, c := range r {
		if len(out) == n {
			break
		}
		if count[c.domain()] < limit {
			out = append(out, c.Idx)
			count[c.domain()]++
		}
	}
	return out
}

// pickOver takes up to n targets from r in rank order, cap or not.
func pickOver(r []Candidate, n int, count map[NodeIdx]int) []NodeIdx {
	var out []NodeIdx
	for _, c := range r {
		if len(out) == n {
			break
		}
		out = append(out, c.Idx)
		count[c.domain()]++
	}
	return out
}

// ranked orders the distinct, weighted candidates by rendezvous score for h, highest
// first, ties by Idx ascending.
func ranked(h Hash, cands []Candidate) []Candidate {
	type scored struct {
		c     Candidate
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
		_, _ = f.Write(buf[:]) // hash.Hash never fails
		// FNV-1a has almost no avalanche on its last input bytes: hashes for adjacent
		// idx values differ by a small multiple of the FNV prime, which made the weight
		// term dominate. The murmur3 finalizer fixes that while keeping the function pure.
		x := fmix64(f.Sum64())
		// Map to (0,1) strictly: use the top 53 bits plus one half, so u is never 0 or 1.
		u := (float64(x>>11) + 0.5) / float64(uint64(1)<<53)
		sc = append(sc, scored{c: c, score: -float64(c.Weight) / math.Log(u)})
	}
	sort.Slice(sc, func(i, j int) bool {
		if sc[i].score != sc[j].score {
			return sc[i].score > sc[j].score
		}
		return sc[i].c.Idx < sc[j].c.Idx
	})
	out := make([]Candidate, len(sc))
	for i := range sc {
		out[i] = sc[i].c
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

// ExcludeIdx returns cands without the targets listed in held (zero entries ignored).
// Used when re-placing failed slots so a target never receives two shards of one stripe
// (I-9).
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

// DefaultRebalanceSkew is how far (a fraction) a disk may sit from its share of its
// host's slots before the repairer moves slots between the host's disks (docs/14 §7).
const DefaultRebalanceSkew = 0.25

// HostShares is each active disk's expected slot count within its host (docs/14 §7,
// I-172): the host's slots spread over its active disks by capacity, no disk expecting
// more than one slot per chunk (I-9), what a disk cannot take spread over the rest
// (water-filling), as the web UI's Disks block draws it. chunks ≤ 0 means no cap.
func HostShares(nodes []Node, slots map[NodeIdx]int, chunks int) map[NodeIdx]float64 {
	out := map[NodeIdx]float64{}
	for _, disks := range hostDisks(nodes) {
		total := 0.0
		for _, d := range disks {
			total += float64(slots[d.Idx])
		}
		limit := math.Inf(1)
		if chunks > 0 {
			limit = float64(chunks)
		}
		open, left := disks, total
		for len(open) > 0 {
			sumCap := 0.0
			for _, d := range open {
				sumCap += float64(d.Capacity)
			}
			var rest []*Node
			for _, d := range open {
				if left*float64(d.Capacity)/sumCap > limit {
					out[d.Idx] = limit
					left -= limit
				} else {
					rest = append(rest, d)
				}
			}
			if len(rest) == len(open) {
				for _, d := range open {
					out[d.Idx] = left * float64(d.Capacity) / sumCap
				}
				break
			}
			open = rest
		}
	}
	return out
}

// hostDisks groups the active disks with a capacity by host, in index order.
func hostDisks(nodes []Node) map[NodeIdx][]*Node {
	byHost := map[NodeIdx][]*Node{}
	for i := range nodes {
		nd := &nodes[i]
		if nd.Kind == KindDisk && nd.State == Active && nd.Capacity > 0 {
			byHost[nd.Host] = append(byHost[nd.Host], nd)
		}
	}
	for _, disks := range byHost {
		sort.Slice(disks, func(i, j int) bool { return disks[i].Idx < disks[j].Idx })
	}
	return byHost
}

// RebalanceMove is a set of slot moves within one host (docs/14 §7, I-172): N chunks
// holding a slot on From and none on To get that slot moved to To.
type RebalanceMove struct {
	Host, From, To NodeIdx
	N              int
}

// RebalanceMoves lists, for every host where a disk sits above its share, another live
// disk of the host below it, and either is more than skew (a fraction) off its share,
// the move from the disk furthest above to the one furthest below, as many slots as the
// excess and the room allow. Hosts whose disks are all within skew of their shares, and
// hosts with one disk, get nothing: a slot never moves between hosts.
func RebalanceMoves(nodes []Node, slots map[NodeIdx]int, chunks int, skew float64, live func(NodeIdx) bool) []RebalanceMove {
	shares := HostShares(nodes, slots, chunks)
	byHost := hostDisks(nodes)
	hosts := make([]NodeIdx, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	var out []RebalanceMove
	for _, h := range hosts {
		var from, to NodeIdx
		var excess, room, fromShare, toShare float64
		for _, d := range byHost[h] {
			e, s := shares[d.Idx], float64(slots[d.Idx])
			if s > e && s-e > excess {
				from, excess, fromShare = d.Idx, s-e, e
			}
			if s < e && e-s > room && live(d.Idx) {
				to, room, toShare = d.Idx, e-s, e
			}
		}
		if from == 0 || to == 0 || (excess <= fromShare*skew && room <= toShare*skew) {
			continue
		}
		if n := int(math.Floor(min(excess, room))); n >= 1 {
			out = append(out, RebalanceMove{Host: h, From: from, To: to, N: n})
		}
	}
	return out
}
