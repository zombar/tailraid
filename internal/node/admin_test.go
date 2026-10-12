package node

import (
	"context"
	"log/slog"
	"path"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/stats"

	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

var ctx = context.Background()

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// opsFor builds the admin operations of one harness node as Serve does.
func opsFor(c *testharness.Cluster, nd *testharness.ClusterNode) *adminOps {
	cfg := repair.DefaultConfig()
	cfg.RateBytes = 0
	r := repair.New(nd.Node.FSM, nd.Node, nd.Prober, nd.Client, nd.Reader, c.Clk, cfg, nil)
	r.Events = nd.Events
	ops := &adminOps{
		node: nd.Node, prober: nd.Prober, repairer: r, client: nd.Client, rd: nd.Reader,
		ring: nd.Events, self: nd.Idx(), clk: c.Clk, log: slog.Default(),
		disks: &diskSet{clusterID: c.CID, hostID: nd.ID, host: nd.Idx(), log: slog.Default()},
	}
	// the harness node's default disk is this host's one open disk (docs/14 §9)
	if nd.Store != nil {
		row, _ := nd.Node.FSM.Node(nd.Target())
		ops.disks.disks = append(ops.disks.disks, &localDisk{idx: nd.Target(), id: row.ID, store: nd.Store})
	}
	return ops
}

// On a cluster where writes, a dedup hit, an unlink and a flush all happened, fsck finds
// nothing on any node and check (with a full scrub) is OK.
// Invariant: I-44
func TestFsck_CleanClusterNoFindings(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	data := testharness.Pattern(2*int(testharness.ChunkSize)+100, 41)
	c.WriteFile(0, "/a", data)
	c.WriteFile(1, "/b", data) // dedup
	c.WriteFile(2, "/c", testharness.Pattern(500, 42))
	c.Settle()
	must(t, c.Nodes[0].FS.Remove("/c"))
	c.Settle()
	for _, nd := range c.Nodes {
		raw, err := opsFor(c, nd).AdminOp(ctx, rpc.AdminOp{Op: "fsck"})
		must(t, err)
		rep := raw.(admin.FsckReport)
		if !rep.OK || len(rep.Violations) != 0 || rep.OrphanShards != 0 {
			t.Fatalf("fsck on n%d: %+v", nd.I, rep)
		}
	}
	leader := c.Leader()
	raw, err := opsFor(c, leader).AdminOp(ctx, rpc.AdminOp{Op: "check", Scrub: true, Files: true})
	must(t, err)
	chk := raw.(admin.CheckReport)
	if !chk.OK || chk.Scan.ScrubBad != 0 || chk.Scan.Unrepairable != 0 || chk.Scan.Scrubbed == 0 || len(chk.Files) != 0 {
		t.Fatalf("check --scrub: %+v", chk)
	}
}

// fsck counts shard files the chunk table does not place on this node, and files in the
// shard tree that are not shards at all; both are informational, not violations.
// Invariant: I-44
func TestFsck_CountsOrphanShards(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/a", testharness.Pattern(int(testharness.ChunkSize), 43))
	c.Settle()
	nd := c.Nodes[0]
	stray := testharness.Pattern(100, 44)
	var h meta.Hash
	copy(h[:], stray)
	_, err := nd.Store.Put(chunk.Key(h, 2, 1, 0), stray) // no chunk entry for this hash
	must(t, err)
	raw, err := opsFor(c, nd).AdminOp(ctx, rpc.AdminOp{Op: "fsck"})
	must(t, err)
	rep := raw.(admin.FsckReport)
	if !rep.OK || len(rep.Violations) != 0 || rep.OrphanShards != 1 {
		t.Fatalf("fsck with one orphan: %+v", rep)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// `token rotate` replaces the join token through a MetaSet that leaves the cluster ID
// untouched; the new token is what every node's status then shows.
// Invariant: I-48
func TestToken_Rotate(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	leader := c.Leader()
	before := leader.Node.FSM.Meta()
	raw, err := opsFor(c, leader).AdminOp(ctx, rpc.AdminOp{Op: "rotate_token"})
	must(t, err)
	tok := raw.(map[string]string)["join_token"]
	if len(tok) != 8 || tok == before.JoinToken {
		t.Fatalf("new token %q (old %q)", tok, before.JoinToken)
	}
	c.Settle()
	for _, nd := range c.Nodes {
		m := nd.Node.FSM.Meta()
		if m.JoinToken != tok || m.ClusterID != before.ClusterID {
			t.Fatalf("n%d meta %+v", nd.I, m)
		}
	}
}

// The stats_seal admin op writes this node's unsealed rows into the volume at once and
// reports how many; it needs no leader (a per-node op).
// Invariant: I-192
func TestAdminOp_StatsSeal(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	nd := c.Nodes[1] // a follower
	ops := opsFor(c, nd)
	ops.stats = &stats.Service{
		Name: "n1", Clk: c.Clk, Keep: stats.DefaultKeep,
		Writer: &stats.Writer{Vol: stats.VFS{FS: nd.FS, St: nd.Node.FSM, Rd: nd.Reader}, Name: "n1"},
	}
	h := c.Clk.Now().UTC().Truncate(time.Hour)
	// the rows must lie in the past: on a fake clock frozen in the first 20 s of an hour the
	// seal (which cuts at now) left them unsealed (CI, macOS, a run across 21:00)
	if d := h.Add(30 * time.Second).Sub(c.Clk.Now()); d > 0 {
		c.Clk.Advance(d)
	}
	for s := 0; s < 30; s += 10 {
		ops.stats.Writer.Add(stats.Row{T: h.Add(time.Duration(s) * time.Second).Unix(), N: "n1", M: map[string]float64{"load1": 1}})
	}
	raw, err := ops.AdminOp(context.Background(), rpc.AdminOp{Op: "stats_seal", Rollup: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.(map[string]any)["sealed_rows"]; got != 3 {
		t.Fatalf("sealed_rows %v", got)
	}
	if n := ops.stats.Writer.Unsealed(); n != 0 {
		t.Fatalf("%d rows still unsealed", n)
	}
	seg := path.Join(stats.Root, "10s", "n1", stats.SegmentName(stats.Raw, h, "n1"))
	if _, err := c.ReadFile(0, seg); err != nil {
		t.Fatalf("the segment on another gateway: %v", err)
	}
	// --rollup wrote today's rollups so far
	day, _ := stats.Minute.Period(h)
	if _, err := c.ReadFile(0, path.Join(stats.Root, "1m", "n1", stats.SegmentName(stats.Minute, day, "n1"))); err != nil {
		t.Fatalf("today's minute rollup: %v", err)
	}
	// nothing to seal: zero rows, no error
	if raw, err := ops.AdminOp(context.Background(), rpc.AdminOp{Op: "stats_seal"}); err != nil || raw.(map[string]any)["sealed_rows"] != 0 {
		t.Fatalf("empty seal: %v %v", raw, err)
	}
}
