package meta

import "fmt"

// MaxAutoK caps the data shard count the automatic policy chooses.
const MaxAutoK = 6

// AutoPolicy is the erasure policy the member table calls for (docs/03 §Choosing the
// policy, I-75): a pure function of the Active device voter count v and non-voter count
// s, n = v + s. m = 0 for one device, 1 for two or three, 2 from four up; k = clamp(v, 1,
// n − m) capped at MaxAutoK, so the data shards fit on the always-on devices.
func AutoPolicy(nodes []Node) (k, m uint8) {
	v, s := 0, 0
	for i := range nodes {
		n := &nodes[i]
		if n.State != Active || n.Kind != KindDevice || n.Idx == 0 {
			continue
		}
		if n.Voter {
			v++
		} else {
			s++
		}
	}
	n := v + s
	switch {
	case n <= 1:
		m = 0
	case n <= 3:
		m = 1
	default:
		m = 2
	}
	kk := min(max(v, 1), n-int(m), MaxAutoK)
	return uint8(max(kk, 1)), m
}

// PolicyAllowed is the precondition for a policy change, manual or automatic: k ≥ 1,
// k+m ≤ 256 and at least k+m live Active nodes, so a full placement is possible the
// moment the policy is set (migration never waits for nodes that are not there).
func PolicyAllowed(nodes []Node, live Liveness, k, m uint8) error {
	if k < 1 || int(k)+int(m) > 256 {
		return fmt.Errorf("bad policy %d+%d", k, m)
	}
	n := 0
	for i := range nodes {
		if nodes[i].State == Active && live.IsLive(nodes[i].Idx) {
			n++
		}
	}
	if int(k)+int(m) > n {
		return fmt.Errorf("policy %d+%d needs %d live active nodes, have %d", k, m, int(k)+int(m), n)
	}
	return nil
}
