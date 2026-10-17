package cluster_test

import (
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// During a rolling upgrade the leader may see a member that reads codec 15 while the
// gateway does not yet: the Commit carrying a copy's attributes is refused with ErrCodec,
// proposed again without them in the same flush, and a SetAttr lands them, so every node
// holds the copy's mode, owner and times either way.
// Invariant: I-401
func TestCluster_AttrsLandWhileMemberOld(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	src := time.Date(2020, 2, 2, 2, 2, 2, 0, time.UTC)
	old := func() uint8 { return meta.CodecHeal } // a member that reads codec 15
	for _, tc := range []struct {
		name             string
		leaderOld, gwOld bool
	}{
		{"/new.bin", false, false},       // the Commit carries them
		{"/leader-old.bin", true, false}, // refused with ErrCodec, retried without them, then a SetAttr
		{"/gateway-old.bin", true, true}, // never sent: a SetAttr after the Commit
	} {
		leader := c.Leader()
		var nd *testharness.ClusterNode
		for _, n := range c.Nodes {
			if n.Node != leader.Node {
				nd = n
				break
			}
		}
		if tc.leaderOld {
			leader.Node.MinCodec = old
		}
		if tc.gwOld {
			nd.Node.MinCodec = old
		}
		fh, err := nd.FS.OpenFile(tc.name, os.O_RDWR|os.O_CREATE, 0o644)
		must(t, err)
		_, err = fh.(interface {
			WriteAt([]byte, int64) (int, error)
		}).WriteAt(testharness.Pattern(5000, 1), 9)
		must(t, err)
		must(t, fh.Close())
		must(t, nd.FS.Chmod(tc.name, 0o600))
		must(t, nd.FS.Chtimes(tc.name, src, src))
		must(t, nd.Flush.FlushAll(ctx))
		leader.Node.MinCodec, nd.Node.MinCodec = leader.Prober.MinCodec, nd.Prober.MinCodec
		c.Settle()
		for i := range c.Nodes {
			in, err := c.Nodes[i].Node.FSM.Resolve([]string{tc.name[1:]})
			must(t, err)
			if in.Mode&0o777 != 0o600 || in.Mtime != src.UnixNano() || in.Size != 5009 {
				t.Fatalf("%s on n%d: %+v", tc.name, i, in)
			}
		}
	}
}
