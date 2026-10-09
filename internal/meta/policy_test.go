package meta

import (
	"testing"

	"github.com/zombar/tailraid/internal/testseed"
)

// devices builds voters+nonVoters hosts with one disk each (a device row is never a
// target, docs/14 §9): host i is device 2i-1 with disk 2i.
func devices(voters, nonVoters int) []Node {
	var out []Node
	for i := 1; i <= voters+nonVoters; i++ {
		host := NodeIdx(2*i - 1)
		out = append(out,
			Node{Idx: host, Kind: KindDevice, State: Active, Voter: i <= voters},
			Node{Idx: host + 1, Kind: KindDisk, State: Active, Capacity: 1, Host: host})
	}
	return out
}

// Invariant: I-75
func TestAutoPolicy_Table(t *testing.T) {
	// the table of docs/03, then the general rule over the whole grid
	cases := []struct{ v, s, k, m int }{
		{1, 0, 1, 0},
		{1, 1, 1, 1},
		{1, 3, 1, 2},
		{2, 1, 2, 1},
		{3, 0, 2, 1},
		{3, 2, 3, 2},
		{2, 4, 2, 2},
		{0, 1, 1, 0},
		{0, 2, 1, 1},
		{0, 5, 1, 2},
		{2, 0, 1, 1},
		{4, 0, 2, 2},
		{5, 0, 3, 2},
		{8, 0, 6, 2},
		{9, 3, 6, 2},
	}
	for _, c := range cases {
		k, m := AutoPolicy(devices(c.v, c.s))
		if int(k) != c.k || int(m) != c.m {
			t.Errorf("v=%d s=%d: %d+%d, want %d+%d", c.v, c.s, k, m, c.k, c.m)
		}
	}
	for v := range 10 {
		for s := range 10 {
			if v+s == 0 {
				continue
			}
			k, m := AutoPolicy(devices(v, s))
			n := v + s
			if k < 1 || int(k)+int(m) > n || int(k) > MaxAutoK || (v >= 1 && int(k) > v && int(k) != n-int(m)) {
				t.Errorf("v=%d s=%d: %d+%d breaks the rule", v, s, k, m)
			}
			if (n == 1 && m != 0) || (n >= 2 && n <= 3 && m != 1) || (n >= 4 && m != 2) {
				t.Errorf("v=%d s=%d: m = %d", v, s, m)
			}
		}
	}
	// only Active devices count: draining, removed and S3 nodes are ignored
	nodes := devices(3, 1)
	nodes[0].State = Draining
	nodes = append(nodes, Node{Idx: 9, Kind: KindS3, State: Active, Capacity: 1, Voter: true})
	if k, m := AutoPolicy(nodes); k != 2 || m != 1 { // v=2, s=1 → 2+1
		t.Errorf("filtered: %d+%d", k, m)
	}
	if k, m := AutoPolicy(nil); k != 1 || m != 0 {
		t.Errorf("no nodes: %d+%d", k, m)
	}
}

// Invariant: I-75
func TestAutoPolicy_NNodeDeterminism(t *testing.T) {
	r := testseed.Seed(t)
	base := devices(3, 4)
	wk, wm := AutoPolicy(base)
	for i := range 200 {
		sh := append([]Node(nil), base...)
		r.Shuffle(len(sh), func(a, b int) { sh[a], sh[b] = sh[b], sh[a] })
		if k, m := AutoPolicy(sh); k != wk || m != wm {
			t.Fatalf("order %d: %d+%d, want %d+%d", i, k, m, wk, wm)
		}
	}
}
