package cluster_test

import (
	"bytes"
	"context"
	"path"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/stats"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// statsVolume is a node's volume adapter as serve wires it: the vfs for writes, a reader
// with no chunk cache for reads.
func statsVolume(nd *testharness.ClusterNode) stats.VFS {
	return stats.VFS{FS: nd.FS, St: nd.Node.FSM, Rd: vfs.NewReader(nd.Node.FSM, nd.Prober, nd.Client, nil, nil)}
}

// A sealed segment is flushed before its name is visible: once the writer on one gateway
// returns and the followers have applied the metadata, another gateway reads the whole
// segment by its path (its bytes were committed before the rename, so never a zero-length
// file), through its own vfs and through a cache-less reader, and no temp remains (I-192).
// Invariant: I-192
func TestStats_SealVisibleFromSecondGateway(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	ctx := context.Background()
	w := &stats.Writer{Vol: statsVolume(c.Nodes[0]), Name: "n0"}
	h := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	for s := 0; s < 3600; s += 10 {
		w.Add(stats.Row{T: h.Add(time.Duration(s) * time.Second).Unix(), N: "n0", M: map[string]float64{"cpu": float64(s) / 3600}})
	}
	if err := w.Seal(ctx, h.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c.Settle() // the followers apply the Create, Commit and Rename; the bytes were committed before the name
	seg := path.Join(stats.Root, "10s", "n0", "2026-10-11T14-n0.jsonl.gz")
	got, err := c.ReadFile(1, seg)
	if err != nil {
		t.Fatalf("second gateway: %v", err)
	}
	rows, err := stats.Decode(bytes.NewReader(got))
	if err != nil || len(rows) != 360 || rows[359].M["cpu"] == 0 {
		t.Fatalf("second gateway decoded %d rows: %v", len(rows), err)
	}
	cold, err := statsVolume(c.Nodes[2]).ReadFile(ctx, seg)
	if err != nil || !bytes.Equal(cold, got) {
		t.Fatalf("cache-less read: %v (%d vs %d bytes)", err, len(cold), len(got))
	}
	names, err := statsVolume(c.Nodes[1]).List(ctx, path.Join(stats.Root, "10s", "n0"))
	if err != nil || len(names) != 1 || names[0] != "2026-10-11T14-n0.jsonl.gz" {
		t.Fatalf("listing %v %v: no temp may remain", names, err)
	}
	if _, err := c.ReadFile(1, path.Join(stats.Root, "10s", "n0", ".2026-10-11T14-n0.jsonl.gz.tailraid-stats")); err == nil {
		t.Fatal("the temp outlived the rename")
	}
}

// The leader's retention removes a segment whose period ended more than its tier's keep
// ago whoever wrote it, a member no longer in the cluster included, through the same
// volume; a young segment stays (I-194).
// Invariant: I-194
func TestStats_Retention_RemovedMemberSegment(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	old := path.Join(stats.Root, "10s", "gone", "2026-10-01T14-gone.jsonl.gz")
	young := path.Join(stats.Root, "10s", "n0", "2026-10-11T10-n0.jsonl.gz")
	var buf bytes.Buffer
	_ = stats.Encode(&buf, []stats.Row{{T: now.Unix(), N: "gone"}})
	for _, p := range []string{old, young} {
		if err := statsVolume(c.Nodes[1]).WriteFile(ctx, p, buf.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	c.Settle()
	w := &stats.Writer{Vol: statsVolume(c.Nodes[0]), Name: "n0"}
	n, err := w.Retain(ctx, now, stats.DefaultKeep)
	if err != nil || n != 1 {
		t.Fatalf("removed %d: %v", n, err)
	}
	c.Settle()
	if _, err := c.ReadFile(2, old); err == nil {
		t.Fatal("the removed member's old segment is still there")
	}
	if _, err := c.ReadFile(2, young); err != nil {
		t.Fatalf("the young segment went: %v", err)
	}
}

// A gateway counts every byte its clients read and wrote through its files, at any
// offset, and nothing another gateway's clients did (the bytes_r and bytes_w of a node
// row, I-191).
// Invariant: I-191
func TestFS_ClientBytes_Counted(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	r0, w0 := c.Nodes[0].FS.ClientBytes()
	if r0 != 0 || w0 != 0 {
		t.Fatalf("fresh gateway counts %d %d", r0, w0)
	}
	f, err := c.Nodes[0].FS.Create("/f")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(4096, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(testharness.Pattern(5000, 3)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, w := c.Nodes[0].FS.ClientBytes(); w != 5000 {
		t.Fatalf("written %d, want 5000", w)
	}
	c.Settle()
	if _, err := c.ReadFile(0, "/f"); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Nodes[0].FS.ClientBytes(); r != 9096 {
		t.Fatalf("read %d, want 9096 (the hole included)", r)
	}
	if r1, w1 := c.Nodes[1].FS.ClientBytes(); r1 != 0 || w1 != 0 {
		t.Fatalf("another gateway counted %d %d", r1, w1)
	}
}
