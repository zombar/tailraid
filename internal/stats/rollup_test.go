package stats

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chart"
)

// rawDay is a day of raw rows for one node and the cluster: 10 s apart, with a gap.
func rawDay(seed int64) []Row {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // a fixed data set
	day := utc(2026, 10, 11, 0, 0)
	var rows []Row
	for s := 0; s < 86400; s += 10 {
		if s >= 3600 && s < 7200 { // an hour with no rows: the node was down
			continue
		}
		t := day.Add(time.Duration(s) * time.Second).Unix()
		m := map[string]float64{"cpu": rng.Float64(), "load1": 1 + rng.Float64()}
		if s%20 == 0 {
			m["disk_w"] = rng.Float64() * 1e6 // a rate the OS only sometimes exposes
		}
		rows = append(rows, Row{T: t, N: "jockey", M: m})
		live := 1
		if s >= 36000 && s < 36600 {
			live = 0
		}
		rows = append(rows, Row{T: t, Term: uint64(12 + s/43200), By: "jockey", C: &Cluster{ //nolint:gosec // small
			Degraded: float64(s % 7), Stored: 6e10 + float64(s), Chunks: 48211, Live: map[string]int{"jockey": 1, "honker": live},
			Targets: []chart.Target{{Name: "jockey/1", Host: "jockey", Slots: 15000 + s/100, Capacity: 5e11, Live: true, Active: true}},
		}})
	}
	return rows
}

// The rollup is a pure function of the raw rows: the same bytes from any node, whatever
// the order the rows arrive in.
// Invariant: I-193
func TestRollup_DeterministicAcrossNodes(t *testing.T) {
	rows := rawDay(1)
	a := Rollup(rows, Minute)
	shuffled := append([]Row(nil), rows...)
	rand.New(rand.NewSource(2)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] }) //nolint:gosec // a permutation
	b := Rollup(shuffled, Minute)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatal("the rollup depends on the rows' order")
	}
	// 1440 minutes, minus the dead hour: node rows and cluster rows each
	if n := len(a); n != 2*(1440-60) {
		t.Fatalf("%d rollup rows, want %d", n, 2*(1440-60))
	}
	c := Rollup(rows, Coarse)
	if n := len(c); n != 2*(144-6) {
		t.Fatalf("%d coarse rows, want %d", n, 2*(144-6))
	}
}

// Per interval: the mean of every metric over the rows that carry it, the minimum of
// each node's liveness, the last targets, the highest term; t is the interval's start;
// an interval without rows yields no row.
// Invariant: I-193
func TestRollup_Rules(t *testing.T) {
	day := utc(2026, 10, 11, 0, 0)
	at := func(s int) int64 { return day.Add(time.Duration(s) * time.Second).Unix() }
	rows := []Row{
		{T: at(0), N: "a", M: map[string]float64{"cpu": 1, "x": 10}},
		{T: at(10), N: "a", M: map[string]float64{"cpu": 3}},
		{T: at(50), N: "a", M: map[string]float64{"cpu": 5, "x": 30}},
		{T: at(130), N: "a", M: map[string]float64{"cpu": 7}}, // minute 2; minute 1 is empty
		{T: at(0), Term: 3, By: "a", C: &Cluster{Degraded: 10, Live: map[string]int{"a": 1, "b": 2}, Targets: []chart.Target{{Name: "a/1", Slots: 1}}}},
		{T: at(30), Term: 4, By: "b", C: &Cluster{Degraded: 20, Live: map[string]int{"a": 1, "b": 0}, Targets: []chart.Target{{Name: "a/1", Slots: 2}}}},
		{T: at(40), Term: 4, By: "b", C: &Cluster{Degraded: 30, Live: map[string]int{"a": 0, "b": 1}, Targets: []chart.Target{{Name: "a/1", Slots: 3}}}},
	}
	out := Rollup(rows, Minute)
	if len(out) != 3 {
		t.Fatalf("%d rows, want 3 (node minute 0, node minute 2, cluster minute 0): %+v", len(out), out)
	}
	var node0, node2, cl *Row
	for i := range out {
		switch {
		case out[i].C != nil:
			cl = &out[i]
		case out[i].T == at(0):
			node0 = &out[i]
		case out[i].T == at(120):
			node2 = &out[i]
		}
	}
	if node0 == nil || node2 == nil || cl == nil {
		t.Fatalf("rows: %+v", out)
	}
	if node0.M["cpu"] != 3 || node0.M["x"] != 20 || node0.N != "a" {
		t.Errorf("node minute 0: %+v (want cpu 3 = mean of 1,3,5; x 20 = mean of 10,30)", node0.M)
	}
	if node2.M["cpu"] != 7 || len(node2.M) != 1 {
		t.Errorf("node minute 2: %+v", node2.M)
	}
	if cl.T != at(0) || cl.Term != 4 || cl.By != "b" || cl.C.Degraded != 20 || cl.C.Live["a"] != 0 || cl.C.Live["b"] != 0 || len(cl.C.Targets) != 1 || cl.C.Targets[0].Slots != 3 {
		t.Errorf("cluster minute 0: %+v %+v", cl, cl.C)
	}
	if got := Rollup(nil, Minute); len(got) != 0 {
		t.Errorf("no rows: %d", len(got))
	}
}

// A rollup takes the minimum of each path.<peer> (the worst class the interval saw)
// and the mean of the rest, the round trips included; a peer absent from some rows is
// judged on the rows that carry it.
// Invariant: I-332
func TestRollup_PathMinimum(t *testing.T) {
	day := utc(2026, 10, 11, 0, 0)
	at := func(s int) int64 { return day.Add(time.Duration(s) * time.Second).Unix() }
	rows := []Row{
		{T: at(0), N: "a", M: map[string]float64{"path.b": 3, "rtt.b": 4, "path.c": 1}},
		{T: at(10), N: "a", M: map[string]float64{"path.b": 3, "rtt.b": 6, "path.c": 0}},
		{T: at(20), N: "a", M: map[string]float64{"path.b": 2, "rtt.b": 8, "path.c": 1, "relayed": 100}},
	}
	out := Rollup(rows, Minute)
	if len(out) != 1 {
		t.Fatalf("%d rows", len(out))
	}
	m := out[0].M
	if m["path.b"] != 2 || m["path.c"] != 0 || m["rtt.b"] != 6 || m["relayed"] != 100 {
		t.Errorf("rollup %+v: want path.b 2 (the worst), path.c 0, rtt.b 6 (the mean), relayed 100", m)
	}
}
