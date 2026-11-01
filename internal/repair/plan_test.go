package repair

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testseed"
)

// randomTopology is hosts of one to three disks each, and sometimes an S3 node, as the
// node table lists them.
func randomTopology(rng interface{ Intn(int) int }) []meta.Node {
	var nodes []meta.Node
	next := meta.NodeIdx(1)
	hosts := 3 + rng.Intn(4)
	for range hosts {
		host := next
		next++
		nodes = append(nodes, meta.Node{Idx: host, Kind: meta.KindDevice, State: meta.Active, Voter: rng.Intn(2) == 0})
		for d := 0; d <= rng.Intn(3); d++ {
			nodes = append(nodes, meta.Node{Idx: next, Kind: meta.KindDisk, State: meta.Active, Host: host, Capacity: uint64(1+rng.Intn(4)) << 40})
			next++
		}
	}
	if rng.Intn(2) == 0 {
		nodes = append(nodes, meta.Node{Idx: next, Kind: meta.KindS3, State: meta.Active, Capacity: 1 << 40})
	}
	return nodes
}

// A home move never plans, and never commits, a placement that names a target twice,
// whichever homes can take a shard and whichever PUTs fail: a slot that keeps its holder
// must not find that holder placed on another slot as well (on the real cluster every
// home move failed as a malformed SetPlacement, re-PUT its shards and kept the gate open).
// Invariant: I-311
func TestPlanSlots_HomeMoveNeverNamesATargetTwice(t *testing.T) {
	rng := testseed.Seed(t)
	tried := 0
	for iter := range 20000 {
		nodes := randomTopology(rng)
		var targets []meta.NodeIdx
		for _, n := range nodes {
			if n.Kind.Target() {
				targets = append(targets, n.Idx)
			}
		}
		k := uint8(2 + rng.Intn(2))
		m := uint8(1 + rng.Intn(2))
		if int(k)+int(m) > len(targets) {
			continue
		}
		h := meta.Hash(sha256.Sum256(fmt.Appendf(nil, "%d", rng.Int63())))
		place := make([]meta.NodeIdx, int(k)+int(m))
		for i, j := range rng.Perm(len(targets))[:len(place)] {
			place[i] = targets[j]
		}
		c := meta.Chunk{Hash: h, K: k, M: m, Refs: 1, Placement: place}
		hv := meta.NewHomeView(nodes)
		away := hv.Away(place, int(k), hv.Home(h, int(k), int(m)))
		if len(away) == 0 {
			continue
		}
		tried++
		refused := map[meta.NodeIdx]bool{}
		for _, n := range targets {
			refused[n] = rng.Intn(3) == 0
		}
		dest := func(n meta.NodeIdx) bool { return !refused[n] }
		live := map[meta.NodeIdx]bool{}
		for _, n := range nodes {
			live[n.Idx] = true
		}
		issues := make([]meta.SlotIssue, 0, len(away))
		kept := append([]meta.NodeIdx(nil), place...)
		for _, i := range away {
			issues = append(issues, meta.SlotIssue{Idx: i, Reason: meta.ReasonAway, Readable: true})
			kept[i] = 0
		}
		plan := planSlots(c, issues, kept, false, k, m, hv, nodes, live, dest)
		if !meta.ValidPlacement(k, m, plan.place) {
			t.Fatalf("iteration %d: planned %v from %v (away %v)", iter, plan.place, place, away)
		}
		// the PUT loop: a failed PUT empties its slot, or gives a still-served slot back
		got := append([]meta.NodeIdx(nil), plan.place...)
		var failed []int
		for i, n := range got {
			if n == 0 || plan.unchanged[i] || rng.Intn(2) == 0 {
				continue
			}
			got[i] = 0
			if plan.readableBad[i] {
				failed = append(failed, i)
			}
		}
		keepServed(place, got, plan.unchanged, failed)
		if !meta.ValidPlacement(k, m, got) {
			t.Fatalf("iteration %d: after failed PUTs %v (planned %v from %v, away %v)", iter, got, plan.place, place, away)
		}
	}
	if tried < 1000 {
		t.Fatalf("only %d chunks had away slots: the test proves little", tried)
	}
}

// The plan of a home move is what the sweep counted: every away slot Moves lists goes to
// the target Moves names and every other away slot keeps its holder, so a chunk counted
// movable is one the pass moves (docs/14 §7).
// Invariant: I-294, I-311
func TestPlanSlots_AgreesWithHomeMoves(t *testing.T) {
	rng := testseed.Seed(t)
	tried, moved := 0, 0
	for iter := range 20000 {
		nodes := randomTopology(rng)
		var targets []meta.NodeIdx
		for _, n := range nodes {
			if n.Kind.Target() {
				targets = append(targets, n.Idx)
			}
		}
		k := uint8(2 + rng.Intn(2))
		m := uint8(1 + rng.Intn(2))
		if int(k)+int(m) > len(targets) {
			continue
		}
		h := meta.Hash(sha256.Sum256(fmt.Appendf(nil, "%d", rng.Int63())))
		place := make([]meta.NodeIdx, int(k)+int(m))
		for i, j := range rng.Perm(len(targets))[:len(place)] {
			place[i] = targets[j]
		}
		c := meta.Chunk{Hash: h, K: k, M: m, Refs: 1, Placement: place}
		hv := meta.NewHomeView(nodes)
		away := hv.Away(place, int(k), hv.Home(h, int(k), int(m)))
		if len(away) == 0 {
			continue
		}
		tried++
		refused := map[meta.NodeIdx]bool{}
		for _, n := range targets {
			refused[n] = rng.Intn(3) == 0
		}
		dest := func(n meta.NodeIdx) bool { return !refused[n] }
		live := map[meta.NodeIdx]bool{}
		for _, n := range nodes {
			live[n.Idx] = true
		}
		issues := make([]meta.SlotIssue, 0, len(away))
		kept := append([]meta.NodeIdx(nil), place...)
		for _, i := range away {
			issues = append(issues, meta.SlotIssue{Idx: i, Reason: meta.ReasonAway, Readable: true})
			kept[i] = 0
		}
		moves := hv.Moves(c, dest)
		plan := planSlots(c, issues, kept, false, k, m, hv, nodes, live, dest)
		to := map[int]meta.NodeIdx{}
		for _, mv := range moves {
			to[mv.Slot] = mv.To
		}
		if len(moves) > 0 {
			moved++
		}
		for _, i := range away {
			want, ok := to[i]
			if !ok {
				want = place[i]
			}
			if plan.place[i] != want || plan.unchanged[i] == ok {
				t.Fatalf("iteration %d: slot %d planned %v (unchanged %v) from %v, moves %v (plan %v)", iter, i, plan.place[i], plan.unchanged[i], place, moves, plan.place)
			}
		}
	}
	if tried < 1000 || moved < 100 {
		t.Fatalf("only %d chunks with away slots, %d with moves", tried, moved)
	}
}
