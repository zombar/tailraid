package node

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// A host's /health lists each registered mirror it has open with the sync under way: the
// file bytes walked of the snapshot's total, and the bytes written so far; once the sync
// ends the progress is gone, and a detached mirror is listed offline by nobody, so every
// other node calls it offline (docs/16 §1).
//
// Invariant: I-188
func TestMirrorSet_HealthCarriesSyncProgress(t *testing.T) {
	r := newMirrorRig(t, pol())
	a := make([]byte, 3*tcs) // three distinct chunks: each one is read
	for i := range a {
		a[i] = byte(i / tcs)
	}
	r.write(meta.RootIno, "a", a)
	r.write(meta.RootIno, "b", bytes.Repeat([]byte{9}, tcs/2))
	var mid []rpc.MirrorHealth
	read := r.s.read
	calls := 0
	r.s.read = func(ctx context.Context, h meta.Hash) ([]byte, error) {
		calls++
		if calls == 2 {
			mid = r.s.health()
		}
		return read(ctx, h)
	}
	r.s.tick(context.Background())
	r.drain()

	if len(mid) != 1 || mid[0].Syncing == nil {
		t.Fatalf("health during the sync = %+v, want the sync listed", mid)
	}
	sy := mid[0].Syncing
	total := uint64(3*tcs + tcs/2)
	if sy.Total != total || sy.Read == 0 || sy.Read >= total || sy.Snapshot == "" || sy.ID == 0 || !mid[0].Online {
		t.Fatalf("progress mid-sync = %+v, want 0 < read < total %d", sy, total)
	}
	r.s.refresh() // the next tick measures the disk
	after := r.s.health()
	if len(after) != 1 || after[0].Syncing != nil || !after[0].Online || after[0].Used == 0 {
		t.Fatalf("health after the sync = %+v", after)
	}
	if err := os.Rename(r.path, r.path+".away"); err != nil {
		t.Fatal(err)
	}
	r.s.refresh()
	if h := r.s.health(); len(h) != 0 {
		t.Fatalf("a detached mirror is listed: %+v", h)
	}
}

type fakeMirrorProbe map[meta.NodeIdx]cluster.MirrorProbe

func (f fakeMirrorProbe) Mirror(idx meta.NodeIdx) (cluster.MirrorProbe, bool) {
	mp, ok := f[idx]
	return mp, ok
}

func (f fakeMirrorProbe) MirrorOnline(idx meta.NodeIdx) bool { return f[idx].Online }

// status lists every mirror of the cluster from the probes, with its host, its path on
// this host only, the use, when it was last seen, the newest complete snapshot and the
// sync under way, and this host's configured paths no row claims (docs/12 §11).
//
// Invariant: I-188
func TestReport_MirrorFields(t *testing.T) {
	seen := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	nodes := []meta.Node{
		{Idx: 1, Name: "pike", Kind: meta.KindDevice},
		{Idx: 2, Name: "atlas", Kind: meta.KindDevice},
		{Idx: 3, Name: "pike/m1", Kind: meta.KindMirror, Host: 1},
		{Idx: 4, Name: "atlas/m1", Kind: meta.KindMirror, Host: 2},
		{Idx: 5, Name: "atlas/m2", Kind: meta.KindMirror, Host: 2},
		{Idx: 6, Name: "gone/m1", Kind: meta.KindMirror, Host: 2, State: meta.Removed},
	}
	snaps := []meta.Snapshot{
		{ID: 1, Mirror: 3, State: meta.SnapComplete, At: 100},
		{ID: 2, Mirror: 3, State: meta.SnapIncomplete, At: 200},
		{ID: 3, Mirror: 3, State: meta.SnapSyncing, At: 300},
		{ID: 4, Mirror: 4, State: meta.SnapDeleting, At: 400},
	}
	pr := fakeMirrorProbe{
		3: {Online: true, Health: rpc.MirrorHealth{Online: true, Used: 10, Free: 20, Total: 40, Syncing: &rpc.SyncHealth{ID: 3, Snapshot: "s3", Read: 1, Total: 4, BytesNew: 2}}, LastSeen: seen},
		4: {Online: false, Health: rpc.MirrorHealth{Reason: "the host is unreachable", Used: 7, Syncing: &rpc.SyncHealth{ID: 9}}, LastSeen: seen},
	}
	local := []admin.MirrorReport{
		{Idx: 3, Name: "pike/m1", Self: true, Path: "/Volumes/Archive/tailraid", Online: true},
		{Self: true, Path: "/Volumes/Spare", Reason: "not registered on this host (tailraid mirror add)"},
	}
	got := clusterMirrors(nodes, snaps, pr, 1, local)
	if len(got) != 4 {
		t.Fatalf("%d mirrors, want the three rows and the unregistered path: %+v", len(got), got)
	}
	pike, atlas, silent, spare := got[0], got[1], got[2], got[3]
	if pike.Name != "pike/m1" || pike.Host != "pike" || !pike.Self || pike.Path != "/Volumes/Archive/tailraid" || !pike.Online ||
		pike.Used != 10 || pike.Free != 20 || pike.Total != 40 || pike.Newest != 200 || pike.LastSeen != seen.UnixNano() ||
		pike.Syncing == nil || pike.Syncing.Percent() != 25 || pike.Syncing.BytesNew != 2 {
		t.Errorf("pike/m1 = %+v", pike)
	}
	if atlas.Host != "atlas" || atlas.Self || atlas.Path != "" || atlas.Online || atlas.Reason != "the host is unreachable" || atlas.Used != 7 || atlas.Syncing != nil || atlas.Newest != 0 {
		t.Errorf("atlas/m1 = %+v (an offline mirror shows no sync, a deleting snapshot is not its newest)", atlas)
	}
	if silent.Name != "atlas/m2" || silent.Online || silent.Reason == "" {
		t.Errorf("a mirror never heard of = %+v", silent)
	}
	if spare.Path != "/Volumes/Spare" || spare.Idx != 0 {
		t.Errorf("unregistered path = %+v", spare)
	}
	b, err := json.Marshal(pike)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"name"`, `"host"`, `"path"`, `"online"`, `"used"`, `"free"`, `"total"`, `"last_seen"`, `"newest"`, `"syncing":{"id":3,"snapshot":"s3","read":1,"total":4,"bytes_new":2}`} {
		if !bytes.Contains(b, []byte(key)) {
			t.Errorf("status JSON %s lacks %s", b, key)
		}
	}
}
