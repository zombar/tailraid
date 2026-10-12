package cluster_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/testharness"
)

// clusterMirror is a mirror disk of a harness cluster's node host (docs/16): the test
// plays the host's sync worker (§3), and every gateway reads the disk through it as a
// vfs.MirrorSource (§6).
type clusterMirror struct {
	c       *testharness.Cluster
	host    int
	idx     meta.NodeIdx
	name    string
	m       *mirror.Mirror
	offline atomic.Bool
}

func newClusterMirror(t *testing.T, c *testharness.Cluster, host int) *clusterMirror {
	t.Helper()
	cm := &clusterMirror{c: c, host: host, name: fmt.Sprintf("n%d/m1", host)}
	id := fmt.Sprintf("mirror-n%d", host)
	testharness.Eventually(t, func() bool {
		res, _, err := c.Leader().Node.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: id, Name: cm.name, Kind: meta.KindMirror, Host: c.Nodes[host].Idx()}}})
		if err == nil && res.Err == nil {
			cm.idx = meta.NodeIdx(res.Ino)
			return true
		}
		return false
	}, "mirror row registered")
	dir := filepath.Join(t.TempDir(), "mirror")
	cid := hex.EncodeToString(c.CID[:])
	if err := mirror.Create(sysfs.OS{}, dir, mirror.Marker{ClusterID: cid, MirrorID: id}); err != nil {
		t.Fatal(err)
	}
	var err error
	if cm.m, err = mirror.Open(sysfs.OS{}, dir, cid); err != nil {
		t.Fatal(err)
	}
	cm.attach()
	return cm
}

// attach gives every running gateway the mirror (a restart builds a new vfs.FS).
func (cm *clusterMirror) attach() {
	for i := range cm.c.Nodes {
		if cm.c.Alive(i) {
			cm.c.Nodes[i].FS.SetMirrors(cm)
		}
	}
}

func (cm *clusterMirror) get(m meta.NodeIdx) (*mirror.Mirror, error) {
	if m != cm.idx || cm.offline.Load() {
		return nil, fmt.Errorf("mirror %d is offline", m)
	}
	return cm.m, nil
}

func (cm *clusterMirror) Tree(_ context.Context, m meta.NodeIdx, h meta.Hash) (mirror.Tree, error) {
	mm, err := cm.get(m)
	if err != nil {
		return mirror.Tree{}, err
	}
	return mm.Tree(h)
}

func (cm *clusterMirror) Object(_ context.Context, m meta.NodeIdx, h meta.Hash) ([]byte, error) {
	mm, err := cm.get(m)
	if err != nil {
		return nil, err
	}
	return mm.Object(h)
}

func (cm *clusterMirror) Online(m meta.NodeIdx) bool {
	_, err := cm.get(m)
	return err == nil
}

// begin proposes a SnapshotBegin through the leader and returns its row and the clone
// the host's FSM handed its hook at the begin's index (§3).
func (cm *clusterMirror) begin(name string) (meta.Snapshot, *meta.State, error) {
	host := cm.c.Nodes[cm.host].Node
	var mu sync.Mutex
	var clone *meta.State
	host.FSM.SetSnapshotHook(func(sn meta.Snapshot, _ meta.NodeIdx, _ string, cl func() *meta.State) {
		if sn.Mirror == cm.idx {
			mu.Lock()
			clone = cl()
			mu.Unlock()
		}
	})
	defer host.FSM.SetSnapshotHook(nil)
	res, index, err := cm.c.Leader().Node.Propose(ctx, meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: cm.idx, Name: name, Now: cm.c.Clk.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return meta.Snapshot{}, nil, err
	}
	if err := host.FSM.WaitApplied(ctx, index); err != nil {
		return meta.Snapshot{}, nil, err
	}
	sn, _ := host.FSM.SnapshotRow(res.SnapID)
	mu.Lock()
	defer mu.Unlock()
	if clone == nil {
		return sn, nil, errors.New("the host's hook took no clone")
	}
	return sn, clone, nil
}

// sync copies the clone to the mirror through the host's reader, as the host's worker.
func (cm *clusterMirror) sync(sn meta.Snapshot, clone *meta.State) (mirror.Manifest, error) {
	rd := cm.c.Nodes[cm.host].Reader
	return cm.m.WriteSnapshot(ctx, mirror.StateSource(clone, rd.ReadChunk), uint64(meta.RootIno), mirror.Manifest{
		ID: uint32(sn.ID), Name: sn.Name, Index: sn.Index, At: sn.At, Auto: sn.Auto, ChunkSize: clone.Vol.ChunkSize,
	})
}

// end proposes the SnapshotEnd of a sync: complete with the manifest's sizes, or failed.
func (cm *clusterMirror) end(sn meta.Snapshot, man *mirror.Manifest) error {
	e := &meta.SnapshotEnd{ID: sn.ID, Now: cm.c.Clk.Now().UnixNano()}
	if man != nil {
		e.OK, e.Root, e.Files, e.Bytes, e.NewBytes, e.Missing = true, meta.Hash(man.Root), man.Files, man.Bytes, man.NewBytes, uint32(len(man.Missing))
	}
	res, _, err := cm.c.Leader().Node.Propose(ctx, meta.Command{Op: meta.OpSnapshotEnd, SnapshotEnd: e})
	if err == nil {
		err = res.Err
	}
	return err
}

// path is name inside snapshot snap on the mounts (§6).
func (cm *clusterMirror) path(snap, name string) string {
	return "/.snapshots/" + strings.ReplaceAll(cm.name, "/", "-") + "/" + snap + name
}

func (cm *clusterMirror) take(t *testing.T, name string) meta.Snapshot {
	t.Helper()
	sn, clone, err := cm.begin(name)
	if err != nil {
		t.Fatalf("begin %s: %v", name, err)
	}
	man, err := cm.sync(sn, clone)
	if err != nil {
		t.Fatalf("sync %s: %v", name, err)
	}
	must(t, cm.end(sn, &man))
	return sn
}

// A snapshot is the volume as of its begin's index on every gateway: what changed after
// the begin and before the copy ran is not in it, and everything committed before is,
// byte for byte (docs/16 §3, §6).
// Invariant: I-179, I-185
func TestCluster_SnapshotReadsAsOfIndexOnEveryGateway(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	cs := int(testharness.ChunkSize)
	before := map[string][]byte{
		"/a":     testharness.Pattern(2*cs+cs/2, 1),
		"/dir/b": testharness.Pattern(cs/3, 2),
		"/c":     testharness.Pattern(cs, 3),
	}
	mkdirRetry(t, c, "/dir")
	for name, b := range before {
		c.WriteFile(0, name, b)
	}
	c.Settle()
	cm := newClusterMirror(t, c, 1)
	sn, clone, err := cm.begin("s1")
	if err != nil {
		t.Fatal(err)
	}
	// the volume moves on through other gateways before the copy runs
	newA := testharness.Pattern(cs+7, 9)
	c.WriteFile(2, "/a", newA)
	must(t, c.Nodes[2].FS.Remove("/c"))
	c.WriteFile(0, "/later", []byte("written after the snapshot began"))
	c.Settle()
	man, err := cm.sync(sn, clone)
	if err != nil {
		t.Fatal(err)
	}
	must(t, cm.end(sn, &man))
	c.Settle()
	if row, _ := c.Leader().Node.FSM.SnapshotRow(sn.ID); row.State != meta.SnapComplete || row.Files != 3 || row.Index != sn.Index {
		t.Fatalf("row %+v", row)
	}
	for i := range c.Nodes {
		for name, want := range before {
			got, err := c.ReadFile(i, cm.path("s1", name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("n%d snapshot %s: %v (differs at %d)", i, name, err, testharness.FirstDiff(got, want))
			}
		}
		if _, err := c.Nodes[i].FS.Stat(cm.path("s1", "/later")); err == nil {
			t.Fatalf("n%d: a file created after the begin is in the snapshot", i)
		}
		if got, err := c.ReadFile(i, "/a"); err != nil || !bytes.Equal(got, newA) {
			t.Fatalf("n%d live /a: %v", i, err)
		}
		ents, err := c.Nodes[i].FS.ReadDir("/.snapshots/" + strings.ReplaceAll(cm.name, "/", "-"))
		if err != nil || len(ents) != 1 || ents[0].Name() != "s1" {
			t.Fatalf("n%d lists %v %v", i, ents, err)
		}
	}
}

// While a sync runs, GC collects nothing tombstoned after its begin, so the copy reads
// every chunk the snapshot references even when the volume overwrote and purged them
// meanwhile; once the sync ends the hold ends and the next GC collects them, and the
// snapshot still reads exact, from the mirror (docs/16 §4).
// Invariant: I-181
// Lifecycle: L-218
func TestCluster_OverwriteDuringSync_SnapshotExact(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	cs := int(testharness.ChunkSize)
	old := testharness.Pattern(3*cs, 4)
	gone := testharness.Pattern(cs+5, 5)
	c.WriteFile(0, "/a", old)
	c.WriteFile(0, "/gone", gone)
	c.Settle()
	cm := newClusterMirror(t, c, 0)
	oldChunks := append(c.ChunksOf(0, "/a"), c.ChunksOf(0, "/gone")...)
	sn, clone, err := cm.begin("held")
	if err != nil {
		t.Fatal(err)
	}
	c.Clk.Advance(time.Second) // every tombstone below is younger than the begin
	c.WriteFile(1, "/a", testharness.Pattern(3*cs, 6))
	must(t, c.Nodes[1].FS.Remove("/gone"))
	c.Settle()
	purgeTrash(t, c)
	gcPass := func() repair.Result {
		t.Helper()
		leader := c.Leader()
		c.Clk.Advance(11 * time.Minute)
		c.Probe()
		res, err := repair.NewGC(leader.Node.FSM, leader.Node, leader.Client, leader.Prober, c.Clk, 10*time.Minute, nil).RunOnce(ctx)
		must(t, err)
		c.Settle()
		return res
	}
	if res := gcPass(); res.Victims != 0 {
		t.Fatalf("GC collected %d chunks while a sync held them", res.Victims)
	}
	for _, ch := range oldChunks {
		if _, ok := c.Leader().Node.FSM.GetChunk(ch.Hash); !ok {
			t.Fatalf("chunk %x collected during the sync", ch.Hash[:6])
		}
	}
	man, err := cm.sync(sn, clone)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Missing) != 0 {
		t.Fatalf("the sync missed %d chunks", len(man.Missing))
	}
	must(t, cm.end(sn, &man))
	c.Settle()
	if res := gcPass(); res.Victims != len(oldChunks) {
		t.Fatalf("GC after the sync collected %d chunks, want the %d it held", res.Victims, len(oldChunks))
	}
	for i := range c.Nodes {
		for name, want := range map[string][]byte{"/a": old, "/gone": gone} {
			got, err := c.ReadFile(i, cm.path("held", name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("n%d snapshot %s after GC: %v", i, name, err)
			}
		}
	}
}

// The catalogue and the snapshot counter travel in the Raft snapshot: a node that was
// away while snapshots were begun, ended, marked for deletion and imported installs a
// snapshot and holds the same catalogue as the leader, and the next begin takes the same
// ID on every node (docs/01 §Binary codec, docs/16 §5).
// Invariant: I-184
func TestCluster_CatalogueSurvivesSnapshotInstall(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1, testharness.SnapshotThreshold(40))
	defer c.Close()
	c.Kill(2)
	cm := newClusterMirror(t, c, 0)
	a := cm.take(t, "a")
	cm.take(t, "b")
	cm.take(t, "c")
	propose := func(cmd meta.Command) {
		t.Helper()
		res, _, err := c.Leader().Node.Propose(ctx, cmd)
		if err == nil {
			err = res.Err
		}
		must(t, err)
	}
	propose(meta.Command{Op: meta.OpSnapshotDelete, SnapshotDelete: &meta.SnapshotDelete{ID: a.ID, Now: c.Clk.Now().UnixNano()}})
	propose(meta.Command{Op: meta.OpSnapshotImport, SnapshotImport: &meta.SnapshotImport{Mirror: cm.idx, After: 40, Name: "old", At: 1, Auto: true, Root: meta.Hash{3}, Files: 1, Now: c.Clk.Now().UnixNano()}})
	if _, _, err := cm.begin("syncing"); err != nil { // a row still syncing at the install
		t.Fatal(err)
	}
	for i := range 200 {
		mkdirRetry(t, c, fmt.Sprintf("/d%03d", i))
	}
	c.Restart(2)
	c.Settle()
	want := c.Leader().Node.FSM.Snapshots()
	if len(want) != 5 {
		t.Fatalf("leader catalogue %+v", want)
	}
	for i := range c.Nodes {
		if got := c.Nodes[i].Node.FSM.Snapshots(); !reflect.DeepEqual(got, want) {
			t.Fatalf("n%d catalogue\n got %+v\nwant %+v", i, got, want)
		}
	}
	for _, sn := range want {
		if sn.State == meta.SnapSyncing {
			must(t, cm.end(sn, nil))
		}
	}
	next, _, err := cm.begin("next")
	if err != nil {
		t.Fatal(err)
	}
	c.Settle()
	if next.ID != 43 { // the import took 41 (above its After 40), the syncing begin 42
		t.Fatalf("next ID %d, want 43", next.ID)
	}
	for i := range c.Nodes {
		if row, ok := c.Nodes[i].Node.FSM.SnapshotRow(next.ID); !ok || row.Name != "next" {
			t.Fatalf("n%d: next %+v %v", i, row, ok)
		}
	}
}
