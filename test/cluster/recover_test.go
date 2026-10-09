package cluster_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// After a lost majority, Recover on the one survivor makes it the sole voter: it leads
// again, every file whose shard it holds reads back exactly, a file whose shard died
// with the others is EIO rather than garbage, and new writes land.
//
// Invariant: I-129
func TestCluster_RecoverAfterLostMajority(t *testing.T) {
	c := testharness.NewCluster(t, 3, 1, 0) // 1+0: each chunk lives on one node, so some files survive one node
	defer c.Close()
	files := map[string][]byte{}
	for i := range 9 {
		p := fmt.Sprintf("/f%d", i)
		files[p] = testharness.Pattern(int(testharness.ChunkSize)/2+i, byte(1+i))
		c.WriteFile(0, p, files[p])
	}
	c.Settle()
	self := c.Nodes[0].Idx()
	onSelf := map[string]bool{}
	for p := range files {
		for _, ch := range c.ChunksOf(0, p) {
			onSelf[p] = ch.Placement[0] == self
		}
	}
	survive := 0
	for _, ok := range onSelf {
		if ok {
			survive++
		}
	}
	if survive == 0 || survive == len(files) {
		t.Fatalf("%d of %d files on the survivor; the test needs a mix", survive, len(files))
	}
	c.Kill(1)
	c.Kill(2)
	c.Kill(0)
	if err := c.Recover(0, []int{0}); err != nil {
		t.Fatal(err)
	}
	c.Restart(0)
	testharness.Eventually(t, func() bool { return c.Nodes[0].Node.IsLeader() }, "the recovered node leads alone")
	c.Settle()
	if nodes := c.Nodes[0].Node.FSM.Nodes(); len(nodes) != 3 {
		t.Fatalf("the member table is the metadata, not the Raft configuration: %d nodes", len(nodes))
	}
	for p, want := range files {
		got, err := c.ReadFileCold(0, p)
		switch {
		case onSelf[p] && (err != nil || !bytes.Equal(got, want)):
			t.Fatalf("%s (shard on the survivor): %v", p, err)
		case !onSelf[p] && !errors.Is(err, vfs.ErrIO):
			t.Fatalf("%s (shard lost with the others): %v, want EIO", p, err)
		}
	}
	data := testharness.Pattern(300, 42)
	c.WriteFile(0, "/after", data)
	c.Settle()
	if got, err := c.ReadFileCold(0, "/after"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("write after recovery: %v", err)
	}
	// the dead members are still rows of the table: the operator removes or re-joins them
	for i := 1; i < 3; i++ {
		if _, ok := c.Nodes[0].Node.FSM.Node(c.Nodes[i].Idx()); !ok {
			t.Fatalf("n%d vanished from the member table", i)
		}
	}
}
