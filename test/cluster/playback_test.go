package cluster_test

import (
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// A playback report is codec 17: the leader refuses it with ErrCodec while a live member
// reads codec 16, and once every member is current it lands on every node.
// Invariant: I-406
func TestCluster_Codec17RefusedWhileMemberOld(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/films/a.mkv", testharness.Pattern(100, 1))
	c.Settle()
	leader := c.Leader()
	f, err := leader.Node.FSM.Resolve([]string{"films", "a.mkv"})
	must(t, err)
	report := func() meta.Result {
		res, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpPlayback, Playback: &meta.Playback{
			Viewer: "t:pat@example.com", Action: meta.PlayPosition, Inos: []meta.Ino{f.Ino}, Pos: 60_000, Dur: 600_000, On: "n0", Now: c.Clk.Now().UnixNano(),
		}})
		must(t, err)
		return res
	}
	leader.Node.MinCodec = func() uint8 { return meta.CodecAttrs } // one member still reads codec 16
	defer func() { leader.Node.MinCodec = nil }()
	if res := report(); !errors.Is(res.Err, meta.ErrCodec) {
		t.Fatalf("a report with an old member: %v, want ErrCodec", res.Err)
	}
	leader.Node.MinCodec = func() uint8 { return meta.CodecVersion }
	if res := report(); res.Err != nil {
		t.Fatalf("a report after the update: %v", res.Err)
	}
	c.Settle()
	for i := range c.Nodes {
		if r, ok := c.Nodes[i].Node.FSM.PlaybackRow("t:pat@example.com", f.Ino); !ok || r.Pos != 60_000 {
			t.Fatalf("n%d row %+v %v", i, r, ok)
		}
	}
}
