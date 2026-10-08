package cluster_test

import (
	"bytes"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// Two always-on voters and two laptops (non-voters) on 2+2: every chunk's data slots sit
// on the voters, so the volume stays readable with both laptops asleep (docs/03
// §Voter-first slots).
// Invariant: I-77
func TestCluster_ReadableWithNonVotersAsleep(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 2, testharness.NonVoters(2, 3))
	defer c.Close()
	data := testharness.Pattern(5*int(testharness.ChunkSize)+123, 21)
	c.WriteFile(0, "/f", data)
	c.WriteFile(1, "/g", testharness.Pattern(3*int(testharness.ChunkSize), 22)) // from the other voter too
	c.Settle()
	voters := map[meta.NodeIdx]bool{}
	for i, nd := range c.Nodes {
		if c.Voter(i) {
			voters[nd.Idx()] = true
		}
	}
	chunks := 0
	c.Nodes[0].Node.FSM.ForEachChunk(func(ch meta.Chunk) bool {
		chunks++
		for i := 0; i < int(ch.K); i++ {
			if n := ch.Placement[i]; !voters[n] {
				t.Fatalf("chunk %x data slot %d on non-voter %d (placement %v)", ch.Hash[:4], i, n, ch.Placement)
			}
		}
		return true
	})
	if chunks < 8 {
		t.Fatalf("only %d chunks committed", chunks)
	}
	// both laptops asleep: reads from either voter still succeed, cold cache included
	c.Kill(2)
	c.Kill(3)
	c.Probe()
	for _, gw := range []int{0, 1} {
		got, err := c.ReadFileCold(gw, "/f")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read /f from voter n%d with laptops asleep: %v", gw, err)
		}
	}
	// and a write still commits with k acks from the voters (degraded parity, docs/00)
	c.WriteFile(0, "/h", []byte("written with the laptops closed"))
	if got, err := c.ReadFileCold(0, "/h"); err != nil || string(got) != "written with the laptops closed" {
		t.Fatalf("read /h: %q %v", got, err)
	}
}
