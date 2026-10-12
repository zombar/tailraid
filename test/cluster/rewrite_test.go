package cluster_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// Two clients replace one file through two gateways at once, each truncating it and
// writing its own payload front to back in chunk-sized pieces, with the gateways'
// flushes interleaved in every order the steps allow: every gateway then reads exactly
// one of the payloads, whole, and every FSM holds the same state, the rewrite ids
// included.
// Invariant: I-223
func TestCluster_TwoRewrites_OneWholeWins(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	cs := int(testharness.ChunkSize)
	// each step: which gateway writes its next piece and then flushes
	orders := [][]int{
		{0, 1, 0, 1, 0, 1, 0}, // A starts, B starts, A goes on superseded, B goes on
		{0, 0, 1, 1, 1, 0, 0}, // A writes two pieces first
		{1, 0, 0, 0, 0, 1, 1}, // B starts first, A starts later and writes the rest
		{0, 1, 1, 1, 1, 0, 0}, // B finishes entirely inside A's copy
	}
	for n, order := range orders {
		path := fmt.Sprintf("/replaced-%d.bin", n)
		writeAt(t, c, 0, path, 0, testharness.Pattern(3*cs, 99)) // the file both replace
		flushAll(t, c, 0)
		payload := [2][]byte{testharness.Pattern(4*cs+17, byte(2*n+1)), testharness.Pattern(3*cs+5, byte(2*n+2))}
		var handles [2]interface {
			io.Writer
			io.Closer
		}
		next := [2]int{}
		for _, g := range order {
			if handles[g] == nil {
				fh, err := c.Nodes[g].FS.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
				must(t, err)
				handles[g] = fh
			}
			p := payload[g]
			if next[g] < len(p) {
				end := min(next[g]+cs, len(p))
				if _, err := handles[g].Write(p[next[g]:end]); err != nil {
					t.Fatal(err)
				}
				next[g] = end
			}
			flushAll(t, c, g)
		}
		for g := range 2 { // each copy writes what is left of its payload
			if next[g] < len(payload[g]) {
				if _, err := handles[g].Write(payload[g][next[g]:]); err != nil {
					t.Fatal(err)
				}
			}
			must(t, handles[g].Close())
			flushAll(t, c, g)
		}
		var first []byte
		for i := range c.Nodes {
			got, err := c.ReadFileCold(i, path)
			if err != nil {
				t.Fatalf("order %v: n%d: %v", order, i, err)
			}
			if !bytes.Equal(got, payload[0]) && !bytes.Equal(got, payload[1]) {
				t.Fatalf("order %v: n%d reads %d bytes that are neither payload whole (A %d, B %d bytes; differs from A at %d, from B at %d)",
					order, i, len(got), len(payload[0]), len(payload[1]), testharness.FirstDiff(got, payload[0]), testharness.FirstDiff(got, payload[1]))
			}
			if first == nil {
				first = got
			} else if !bytes.Equal(got, first) {
				t.Fatalf("order %v: n%d reads another payload than n0", order, i)
			}
		}
	}
	checkFSMsAgree(t, c)
}

// checkFSMsAgree fails unless every node's FSM encodes to the same state (I-4).
func checkFSMsAgree(t *testing.T, c *testharness.Cluster) {
	t.Helper()
	c.Settle()
	var ref []byte
	for i := range c.Nodes {
		snap, err := c.Nodes[i].Node.FSM.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		st := snap.(interface{ State() *meta.State }).State()
		var buf bytes.Buffer
		must(t, meta.EncodeState(&buf, st))
		if v := meta.CheckInvariants(st); len(v) != 0 {
			t.Fatalf("n%d FSM invariants: %v", i, v)
		}
		if enc := buf.Bytes()[13:]; ref == nil {
			ref = enc
		} else if !bytes.Equal(ref, enc) {
			t.Fatalf("n%d FSM state diverges from n0", i)
		}
	}
}

// While a member still reads codec 8 a replacing copy commits as plain writes and
// nothing fails; once every member reads codec 9 the next one is a rewrite.
// Invariant: I-223
func TestCluster_Codec9RefusedWhileMemberOld(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	setMin := func(v uint8) {
		for _, nd := range c.Nodes {
			nd.Node.MinCodec = func() uint8 { return v }
		}
	}
	defer func() {
		for _, nd := range c.Nodes {
			nd.Node.MinCodec = nil
		}
	}()
	inode := func(path string) meta.Inode {
		t.Helper()
		in, err := c.Nodes[0].Node.FSM.Resolve(meta.SplitPath(path))
		must(t, err)
		return in
	}
	setMin(meta.CodecRewrite - 1)
	data := testharness.Pattern(2*int(testharness.ChunkSize)+3, 40)
	c.WriteFile(0, "/old", data)
	c.Settle()
	if in := inode("/old"); in.RewriteAt != 0 {
		t.Fatalf("a commit while a member reads codec 8 started a rewrite: %+v", in)
	}
	setMin(meta.CodecVersion)
	c.WriteFile(0, "/new", data)
	c.Settle()
	if in := inode("/new"); in.RewriteAt == 0 {
		t.Fatal("after the update the replacing copy was no rewrite")
	}
	for i := range c.Nodes {
		for _, p := range []string{"/old", "/new"} {
			if got, err := c.ReadFile(i, p); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("n%d read of %s: %v", i, p, err)
			}
		}
	}
}
