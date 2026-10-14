package cluster_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

// awayIn counts the slots of path's chunks away from their home in node i's view.
func awayIn(c *testharness.Cluster, i int, path string) int {
	hv := meta.NewHomeView(c.Nodes[i].Node.FSM.Nodes())
	n := 0
	for _, ch := range c.ChunksOf(i, path) {
		n += len(hv.Away(ch.Placement, int(ch.K), hv.Home(ch.Hash, int(ch.K), int(ch.M))))
	}
	return n
}

// downThenUp writes path while one non-leader host is dead, so the chunks whose home
// includes it land around it, then brings the host back: the slots are good and away.
func downThenUp(t *testing.T, c *testharness.Cluster, path string, chunks int) []byte {
	t.Helper()
	leader := c.Leader()
	down := (leader.I + 1) % len(c.Nodes)
	c.Kill(down)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	data := testharness.Pattern(chunks*int(testharness.ChunkSize), 11)
	c.WriteFile(leader.I, path, data)
	c.Restart(down)
	c.Settle()
	c.Probe()
	if awayIn(c, leader.I, path) == 0 {
		t.Fatal("setup: nothing away after a write with a host down")
	}
	return data
}

// A volume written while a host was down has its slots away once it returns: the pass
// that finds them opens the gate (moving none), the following passes move them home, a
// batch a pass, every chunk reads exact throughout, and the pass after convergence
// finds nothing away and closes the gate.
// Invariant: I-294, I-295, I-65, I-9
// Lifecycle: L-211
func TestCluster_DegradedWrite_DataSlotsComeHome(t *testing.T) {
	c := testharness.NewCluster(t, 5, 2, 2)
	defer c.Close()
	data := downThenUp(t, c, "/f", 12)
	leader := c.Leader()
	cfg := repairCfg()
	cfg.HomeBatch = 3
	r := newRepairer(c, leader, cfg)
	st, err := r.Scan(ctx)
	must(t, err)
	if st.Homed != 0 || st.Repaired != 0 || !r.Home().Open {
		t.Fatalf("first pass: %+v, gate %+v", st, r.Home())
	}
	passes := 0
	for ; passes < 12; passes++ {
		st, err = r.Scan(ctx)
		must(t, err)
		if st.Homed > cfg.HomeBatch {
			t.Fatalf("pass %d moved %d chunks, batch %d", passes, st.Homed, cfg.HomeBatch)
		}
		got, err := c.ReadFileCold((leader.I+2)%5, "/f")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read after pass %d: %v", passes, err)
		}
		if st.Homed == 0 {
			break
		}
	}
	if n := awayIn(c, leader.I, "/f"); n != 0 || r.Home().Open {
		t.Fatalf("after %d passes %d slots away, gate %+v", passes, n, r.Home())
	}
	st, err = r.Scan(ctx)
	must(t, err)
	if st.Homed != 0 || st.AwaySlots != 0 {
		t.Fatalf("converged volume: %+v", st)
	}
}

// The gate: a threshold the movable bytes never reach moves nothing; repair.home_start 0
// never opens on its own but OpenHome does; a pass that found bad slots holds the next
// pass's home moves back.
// Invariant: I-295
// Lifecycle: L-220
func TestRepair_Home_Gate_Table(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start float64
		open  bool // OpenHome before the passes
		moves bool
	}{
		{"threshold never reached", 1.0, false, false},
		{"home_start 0 stays closed", 0, false, false},
		{"home_start 0, volume rebalance", 0, true, true},
		{"default threshold", repair.DefaultHomeStart, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testharness.NewCluster(t, 5, 2, 2)
			defer c.Close()
			downThenUp(t, c, "/f", 8)
			cfg := repairCfg()
			cfg.HomeStart = tc.start
			r := newRepairer(c, c.Leader(), cfg)
			if tc.open {
				r.OpenHome()
			}
			homed := 0
			for range 3 {
				st, err := r.Scan(ctx)
				must(t, err)
				homed += st.Homed
				if st.MovableBytes == 0 && homed == 0 {
					t.Fatalf("nothing movable: %+v", st)
				}
			}
			if (homed > 0) != tc.moves {
				t.Fatalf("moved %d chunks in three passes, want moves %v (gate %+v)", homed, tc.moves, r.Home())
			}
		})
	}
	t.Run("a pass with bad slots holds home moves back", func(t *testing.T) {
		c := testharness.NewCluster(t, 5, 2, 2)
		defer c.Close()
		downThenUp(t, c, "/f", 8)
		cfg := repairCfg()
		cfg.HomeBatch = 1
		leader := c.Leader()
		r := newRepairer(c, leader, cfg)
		_, err := r.Scan(ctx) // opens the gate
		must(t, err)
		victim := (leader.I + 3) % 5
		c.Kill(victim)
		c.Clk.Advance(cluster.LiveThreshold + time.Second)
		c.Probe()
		c.Clk.Advance(cluster.FillAfter)
		c.Probe()
		st, err := r.Scan(ctx) // repairs the dead host's slots
		must(t, err)
		if st.Repaired+st.Partial == 0 {
			t.Fatalf("the dead host's slots were not repaired: %+v", st)
		}
		st, err = r.Scan(ctx)
		must(t, err)
		if st.Homed != 0 || st.MovableBytes == 0 {
			t.Fatalf("the pass after a repair moved home: %+v", st)
		}
	})
}

// Every refill aims at home: after a drain or a lost shard is repaired with every home
// target live, no chunk is away.
// Invariant: I-296
// Lifecycle: L-193
func TestRepair_RefillGoesHome_Table(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(c *testharness.Cluster, leader *testharness.ClusterNode)
	}{
		{"a draining holder", func(c *testharness.Cluster, leader *testharness.ClusterNode) {
			must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[(leader.I+1)%5].Idx(), State: meta.Draining}))
		}},
		{"a host dead past fill_after, then removed", func(c *testharness.Cluster, leader *testharness.ClusterNode) {
			v := (leader.I + 2) % 5
			c.Kill(v)
			c.Clk.Advance(cluster.LiveThreshold + time.Second)
			c.Probe()
			c.Clk.Advance(cluster.FillAfter)
			c.Probe()
			must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[v].Idx(), State: meta.Draining}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testharness.NewCluster(t, 5, 2, 2)
			defer c.Close()
			leader := c.Leader()
			data := testharness.Pattern(8*int(testharness.ChunkSize), 13)
			c.WriteFile(leader.I, "/f", data)
			if n := awayIn(c, leader.I, "/f"); n != 0 {
				t.Fatalf("setup: %d slots away with every host up", n)
			}
			tc.break_(c, leader)
			c.Settle()
			r := newRepairer(c, leader, repairCfg())
			for range 3 {
				_, err := r.Scan(ctx)
				must(t, err)
			}
			if n := awayIn(c, leader.I, "/f"); n != 0 {
				t.Fatalf("%d slots away after the refills", n)
			}
			got, err := c.ReadFileCold(leader.I, "/f")
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("read: %v", err)
			}
		})
	}
}

// A cap-breaking slot whose home host is dead still moves at once, under the cap, to a
// live host: restoring the host tolerance never waits for home. When the home host
// returns the slot is away, for the gate to bring home.
// Invariant: I-296, I-89
// Lifecycle: L-193
func TestRepair_DomainSkew_HomeDeadFillsUnderCap(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1) // cap 1 slot per host once all three are active
	defer c.Close()
	c.AddDisk(0)
	c.AddDisk(0)
	leader := c.Leader()
	h1, h2 := c.Nodes[1].Idx(), c.Nodes[2].Idx()
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: h1, State: meta.Draining}))
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: h2, State: meta.Draining}))
	c.Settle()
	data := testharness.Pattern(6*int(testharness.ChunkSize), 17)
	c.WriteFile(0, "/skew", data)
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: h1, State: meta.Active}))
	must(t, leader.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: h2, State: meta.Active}))
	c.Settle()
	dead := 2
	if leader.I == 2 {
		dead = 1
	}
	c.Kill(dead)
	c.Clk.Advance(cluster.LiveThreshold + time.Second)
	c.Probe()
	st, err := newRepairer(c, c.Leader(), repairCfg()).Scan(ctx)
	must(t, err)
	if st.Repaired+st.Partial == 0 {
		t.Fatalf("no skewed chunk moved with its home dead: %+v", st)
	}
	for _, ch := range c.ChunksOf(0, "/skew") {
		ph := perHost(c, 0, ch)
		if ph[c.Nodes[0].Idx()] > 2 || ph[c.Nodes[dead].Idx()] != 0 {
			t.Fatalf("placement %v by host %v", ch.Placement, ph)
		}
	}
	got, err := c.ReadFileCold(0, "/skew")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %v", err)
	}
	c.Restart(dead)
	c.Settle()
	c.Probe()
	if awayIn(c, 0, "/skew") == 0 {
		t.Fatal("no slot away once the home host returned")
	}
}

// A pass whose home moves all fail shuts the gate for HomeHold: the next passes move
// nothing and PUT nothing, and the moves resume once the hold has passed and the links
// are back. On the real cluster a gate that stayed open re-PUT the same chunks every
// pass for hours.
// Invariant: I-311
func TestRepair_Home_FailedMovesShutTheGate(t *testing.T) {
	c := testharness.NewCluster(t, 5, 2, 2)
	defer c.Close()
	leader := c.Leader()
	downThenUp(t, c, "/f", 8)
	cfg := repairCfg()
	r := newRepairer(c, leader, cfg)
	st, err := r.Scan(ctx) // opens the gate, moving none
	must(t, err)
	if !r.Home().Open {
		t.Fatalf("setup: gate shut after the first pass: %+v", st)
	}
	leader.Net.Isolate(true) // every shard RPC of the leader fails: no move can read or PUT
	st, err = r.Scan(ctx)
	must(t, err)
	if st.Homed != 0 || r.Home().Open {
		t.Fatalf("a pass of failed moves: %+v, gate %+v", st, r.Home())
	}
	for pass := range 2 {
		st, err = r.Scan(ctx)
		must(t, err)
		if st.Homed != 0 || st.BytesMoved != 0 || r.Home().Open {
			t.Fatalf("held pass %d: %+v, gate %+v", pass, st, r.Home())
		}
	}
	leader.Net.Isolate(false)
	c.Clk.Advance(repair.HomeHold + time.Minute)
	c.Probe()
	homed := 0
	for range 4 {
		st, err = r.Scan(ctx)
		must(t, err)
		homed += st.Homed
	}
	if homed == 0 {
		t.Fatalf("no move after the hold: %+v, gate %+v", st, r.Home())
	}
}
