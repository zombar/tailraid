package cluster_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	nfs "github.com/willscott/go-nfs"

	"github.com/zombar/tailraid/internal/nfsgw"
	"github.com/zombar/tailraid/internal/testharness"
)

// rmw overwrites patch at off inside the committed file through gateway i and flushes:
// the review checklist's non-zero-offset write, so a test that wrote a whole file also
// exercises a read-modify-write of an existing chunk. It returns the expected content.
func rmw(t *testing.T, c *testharness.Cluster, i int, path string, data []byte, off int, patch []byte) []byte {
	t.Helper()
	if off+len(patch) > len(data) {
		t.Fatalf("rmw past the end: %d+%d > %d", off, len(patch), len(data))
	}
	writeAt(t, c, i, path, off, patch)
	flushAll(t, c, i)
	out := bytes.Clone(data)
	copy(out[off:], patch)
	return out
}

// The leader dies after the gateway's shards are acknowledged and before the Commit is
// proposed: the proposal's outcome is unknown, the flusher keeps the overlay and the next
// attempt reaches the new leader, the write lands exactly once and every node agrees on
// the file; the old leader rejoins with the same view.
//
// Invariant: I-12, I-29, I-95
func TestCluster_LeaderDiesAfterShardsAcked_CommitLandsOnce(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	leader := c.Leader().I
	gw := (leader + 1) % 3
	cs := int(testharness.ChunkSize)
	old := testharness.Pattern(2*cs, 5)
	c.WriteFile(gw, "/f", old)
	c.Settle()
	off := cs/2 + 7
	patch := testharness.Pattern(100, 9)
	want := bytes.Clone(old)
	copy(want[off:], patch)
	writeAt(t, c, gw, "/f", off, patch)
	var once sync.Once
	c.Nodes[gw].Flush.Hooks.AfterAllShardsAcked = func() { once.Do(func() { c.Kill(leader) }) }
	var err error
	for range 300 { // the first flush proposes to a dead leader; a later one reaches the new leader
		if err = c.Nodes[gw].Flush.FlushAll(ctx); err == nil {
			break
		}
		<-time.After(100 * time.Millisecond)
	}
	must(t, err)
	c.Nodes[gw].Flush.Hooks.AfterAllShardsAcked = nil
	c.Settle()
	for i := range c.Nodes {
		if !c.Alive(i) {
			continue
		}
		got, err := c.ReadFileCold(i, "/f")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("n%d after the leader died mid-flush: %v (first diff %d)", i, err, testharness.FirstDiff(got, want))
		}
	}
	if chunks := c.ChunksOf(gw, "/f"); len(chunks) != 2 {
		t.Fatalf("%d chunks, want 2: the commit landed twice or not at all", len(chunks))
	}
	c.Restart(leader)
	c.Settle()
	if got, err := c.ReadFileCold(leader, "/f"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("old leader after rejoining: %v", err)
	}
}

// A handle issued by one gateway resolves on another, survives a rename made elsewhere
// and a real restart of the issuing gateway, and is STALE everywhere once the file is
// gone, never another file.
//
// Invariant: I-16, I-17
func TestNFS_HandleSurvivesGatewayRestartAndOtherGateway(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	c.WriteFile(0, "/a/f", testharness.Pattern(100, 3))
	c.Settle()
	handler := func(i int) *nfsgw.Handler { return nfsgw.New(c.Nodes[i].FS, c.Nodes[i].Node.FSM) }
	fh := handler(0).ToHandle(nil, []string{"a", "f"})
	if fh == nil {
		t.Fatal("no handle for /a/f")
	}
	pathVia := func(i int) (string, error) {
		_, p, err := handler(i).FromHandle(fh)
		return strings.Join(p, "/"), err
	}
	if p, err := pathVia(1); err != nil || p != "a/f" {
		t.Fatalf("other gateway: %q %v", p, err)
	}
	// renamed through another gateway, then the issuing gateway restarts: the handle
	// names the new path on every gateway
	must(t, c.Nodes[1].FS.Rename("/a/f", "/a/g"))
	c.Settle()
	c.Kill(0)
	c.Restart(0)
	c.Settle()
	for i := range c.Nodes {
		if p, err := pathVia(i); err != nil || p != "a/g" {
			t.Fatalf("n%d after the rename and restart: %q %v", i, p, err)
		}
	}
	// removed, and a new file created: STALE everywhere, never the new file
	must(t, c.Nodes[2].FS.Remove("/a/g"))
	c.WriteFile(2, "/a/h", testharness.Pattern(10, 4))
	c.Settle()
	for i := range c.Nodes {
		_, err := pathVia(i)
		var se *nfs.NFSStatusError
		if !errors.As(err, &se) || se.NFSStatus != nfs.NFSStatusStale {
			t.Fatalf("n%d after the remove: %v, want NFS3ERR_STALE", i, err)
		}
	}
}
