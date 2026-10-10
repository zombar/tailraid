package meta

import (
	"fmt"
	"slices"
)

// MaxAutoK caps the data shard count the automatic policy chooses.
const MaxAutoK = 6

// target is one placement target of the member table as the policy counts it.
type target struct {
	idx, host NodeIdx
	voter     bool
}

// targets lists the Active targets the policy counts: every Active disk of an Active
// device, and every Active S3 node as a host of its own that never votes. A device row
// itself is never one, so a host with no disks counts for nothing (docs/14 §6, §9, I-118).
func targets(nodes []Node) []target {
	devs := map[NodeIdx]*Node{}
	for i := range nodes {
		if n := &nodes[i]; n.Kind == KindDevice && n.State == Active && n.Idx != 0 {
			devs[n.Idx] = n
		}
	}
	var out []target
	for i := range nodes {
		n := &nodes[i]
		if n.State != Active || n.Idx == 0 {
			continue
		}
		switch n.Kind {
		case KindS3:
			out = append(out, target{idx: n.Idx, host: n.Idx}) // its own domain (docs/14 §5)
		case KindDisk:
			if h := devs[n.Host]; h != nil {
				out = append(out, target{idx: n.Idx, host: h.Idx, voter: h.Voter})
			}
		case KindDevice:
		}
	}
	return out
}

// perHost counts targets per host, in host order.
func perHost(ts []target) []int {
	m := map[NodeIdx]int{}
	for _, t := range ts {
		m[t.host]++
	}
	hosts := make([]NodeIdx, 0, len(m))
	for h := range m {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	out := make([]int, len(hosts))
	for i, h := range hosts {
		out[i] = m[h]
	}
	return out
}

// worstHost is the most slots of an n-slot stripe one host receives when placement fills
// each host up to the cap ceil(n/H) and hosts with spare targets take the rest (Place's
// two passes, docs/14 §5).
func worstHost(counts []int, n int) int {
	if len(counts) == 0 {
		return 0
	}
	limit := hostCap(n, len(counts))
	alloc := make([]int, len(counts))
	rem := n
	for i, c := range counts {
		alloc[i] = min(limit, c)
		rem -= alloc[i]
	}
	for rem > 0 {
		best := -1
		for i, c := range counts {
			if spare := c - alloc[i]; spare > 0 && (best < 0 || spare > counts[best]-alloc[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		alloc[best]++
		rem--
	}
	w := 0
	for _, a := range alloc {
		w = max(w, a)
	}
	return w
}

// AutoPolicy is the erasure policy the member table calls for (docs/03 §Choosing the
// policy, docs/14 §6, I-75, I-88): a pure function of the Active targets on voter hosts v
// and on non-voter hosts s, n = v + s, and of the hosts they sit on. m = 0 for one
// target, 1 for two or three, 2 from four up; k = clamp(v, 1, n − m) capped at
// MaxAutoK, so the data shards fit on the always-on devices; then, with two or more
// hosts, k is lowered until no host holds more than m slots of a stripe, so the policy
// survives the loss of any one host. With one target per host this is the device count
// rule.
func AutoPolicy(nodes []Node) (k, m uint8) {
	ts := targets(nodes)
	v := 0
	for _, t := range ts {
		if t.voter {
			v++
		}
	}
	n := len(ts)
	switch {
	case n <= 1:
		m = 0
	case n <= 3:
		m = 1
	default:
		m = 2
	}
	kk := max(min(max(v, 1), n-int(m), MaxAutoK), 1)
	if counts := perHost(ts); len(counts) >= 2 && m > 0 {
		for kk > 1 && worstHost(counts, kk+int(m)) > int(m) {
			kk--
		}
	}
	return uint8(kk), m
}

// Tolerance is what a (k, m) policy survives on this member table: the loss of any m
// targets (disks), and of floor(m / w) whole hosts, w being the most slots one host
// holds under the host cap; 0 hosts with a single host (docs/14 §6).
func Tolerance(nodes []Node, k, m uint8) (disks, hosts int) {
	counts := perHost(targets(nodes))
	if len(counts) < 2 || m == 0 {
		return int(m), 0
	}
	w := worstHost(counts, int(k)+int(m))
	if w == 0 {
		return int(m), 0
	}
	return int(m), int(m) / w
}

// PolicyAllowed is the precondition for a policy change, manual or automatic: k ≥ 1,
// k+m ≤ 256 and at least k+m live Active targets (disks and S3 nodes), so a full
// placement is possible the moment the policy is set (migration never waits for targets
// that are not there).
func PolicyAllowed(nodes []Node, live Liveness, k, m uint8) error {
	if k < 1 || int(k)+int(m) > 256 {
		return fmt.Errorf("bad policy %d+%d", k, m)
	}
	n := 0
	for _, t := range targets(nodes) {
		if live.IsLive(t.idx) && live.IsLive(t.host) {
			n++
		}
	}
	if int(k)+int(m) > n {
		return fmt.Errorf("policy %d+%d needs %d live active targets, have %d", k, m, int(k)+int(m), n)
	}
	return nil
}
