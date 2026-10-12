package meta

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func init() {
	goldenCommands = append(goldenCommands, []struct {
		name string
		cmd  Command
		hex  string
	}{
		{
			"SnapshotBegin", // codec version 7 (docs/16 §3, I-184)
			Command{Op: OpSnapshotBegin, SnapshotBegin: &SnapshotBegin{Mirror: 5, Name: "nightly", Auto: true, Now: 2}},
			"07 13 0500 07 6e696768746c79 01 0200000000000000",
		},
		{
			"SnapshotEnd",
			Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: 42, OK: true, Root: Hash{0xcc}, Files: 3, Bytes: 1024, NewBytes: 512, Missing: 1, Now: 9}},
			"07 14 2a000000 01 cc" + repeatHex(0, 31) + " 0300000000000000 0004000000000000 0002000000000000 01000000 0900000000000000",
		},
		{
			"SnapshotDelete",
			Command{Op: OpSnapshotDelete, SnapshotDelete: &SnapshotDelete{ID: 42, Done: true, Now: 1}},
			"07 15 2a000000 01 0100000000000000",
		},
		{
			"SnapshotImport",
			Command{Op: OpSnapshotImport, SnapshotImport: &SnapshotImport{Mirror: 5, After: 9, Name: "old", At: 3, Auto: true, Root: Hash{0xcc}, Files: 1, Bytes: 2, NewBytes: 3, Missing: 4, Now: 7}},
			"07 16 0500 09000000 03 6f6c64 0300000000000000 01 cc" + repeatHex(0, 31) + " 0100000000000000 0200000000000000 0300000000000000 04000000 0700000000000000",
		},
	}...)
}

// mirrorFSM is a test FSM with three devices (1–3) and a mirror (4) on device 1.
func mirrorFSM(t *testing.T) (tf *tfsm, mirror NodeIdx) {
	t.Helper()
	tf = newTFSM(t)
	for i := 1; i <= 3; i++ {
		tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "dev" + string(rune('0'+i)), Name: "n" + string(rune('0'+i)), Kind: KindDevice, Addr: "100.64.0.1"}}})
	}
	r := tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "mirror1", Name: "n1/m1", Kind: KindMirror, Host: 1}}})
	return tf, NodeIdx(r.Ino)
}

func (tf *tfsm) begin(m NodeIdx, name string, now int64) SnapID {
	tf.t.Helper()
	return tf.ok(Command{Op: OpSnapshotBegin, SnapshotBegin: &SnapshotBegin{Mirror: m, Name: name, Now: now}}).SnapID
}

func (tf *tfsm) row(id SnapID) Snapshot {
	tf.t.Helper()
	sn, ok := tf.f.SnapshotRow(id)
	if !ok {
		tf.t.Fatalf("snapshot %d missing", id)
	}
	return sn
}

// Invariant: I-179
func TestFSM_SnapshotBegin_Table(t *testing.T) {
	tf, m := mirrorFSM(t)
	disk := NodeIdx(tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "disk1", Name: "n1/1", Kind: KindDisk, Host: 1, Capacity: 1 << 30}}}).Ino)
	begin := func(mirror NodeIdx, name string) Command {
		return Command{Op: OpSnapshotBegin, SnapshotBegin: &SnapshotBegin{Mirror: mirror, Name: name, Now: 50}}
	}
	tf.fails(begin(99, "a"), ErrNoSuchNode)
	tf.fails(begin(disk, "a"), ErrNoSuchNode) // a disk is not a mirror
	tf.fails(begin(1, "a"), ErrNoSuchNode)    // nor is a device

	before := tf.f.state().Clone()
	id := tf.begin(m, "first", 50)
	sn := tf.row(id)
	if id != 1 || sn.Index != tf.idx || sn.At != 50 || sn.State != SnapSyncing || sn.Mirror != m || sn.Name != "first" || sn.Auto {
		t.Fatalf("row %+v (id %d, index %d)", sn, id, tf.idx)
	}
	// nothing but the row and the counter changed: a snapshot copies nothing (I-179)
	after := tf.f.state().Clone()
	after.Snaps, after.NextSnap, after.Applied, after.LastNow = before.Snaps, before.NextSnap, before.Applied, before.LastNow
	if !reflect.DeepEqual(after, before) {
		t.Fatal("SnapshotBegin changed the volume")
	}
	tf.fails(begin(m, "second"), ErrExist) // one sync per mirror
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: id, OK: true, Now: 60}})
	tf.fails(begin(m, "first"), ErrExist) // the name is taken on this mirror
	if id2 := tf.begin(m, "second", 70); id2 != 2 {
		t.Fatalf("second ID %d, want 2: IDs are never reused", id2)
	}
	// another mirror syncs at the same time, and may reuse a name: names are per mirror
	m2 := NodeIdx(tf.ok(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "mirror2", Name: "n2/m1", Kind: KindMirror, Host: 2}}}).Ino)
	tf.begin(m2, "first", 80)
}

// A manifest found with no row becomes one (docs/16 §5): complete or incomplete at once,
// with its own time, under an ID above both NextSnap and every manifest on its disk.
// Invariant: I-183
func TestFSM_SnapshotImport_Table(t *testing.T) {
	tf, m := mirrorFSM(t)
	imp := func(mirror NodeIdx, after SnapID, name string, missing uint32) Command {
		return Command{Op: OpSnapshotImport, SnapshotImport: &SnapshotImport{
			Mirror: mirror, After: after, Name: name, At: 5, Auto: true,
			Root: Hash{9}, Files: 2, Bytes: 30, NewBytes: 10, Missing: missing, Now: 100,
		}}
	}
	tf.fails(imp(99, 0, "a", 0), ErrNoSuchNode)
	tf.fails(imp(1, 0, "a", 0), ErrNoSuchNode) // a device is not a mirror
	tf.fails(imp(m, 0, "a/b", 0), ErrBadCommand)
	tf.fails(imp(m, ^SnapID(0), "a", 0), ErrBadCommand) // no ID above it
	first := tf.begin(m, "taken", 50)
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: first, OK: true, Now: 60}})
	tf.fails(imp(m, 0, "taken", 0), ErrExist)

	before := tf.f.state().Clone()
	id := tf.ok(imp(m, 0, "old", 0)).SnapID
	sn := tf.row(id)
	if id != first+1 || sn.State != SnapComplete || sn.At != 5 || !sn.Auto || sn.Root != (Hash{9}) || sn.Files != 2 || sn.Bytes != 30 || sn.NewBytes != 10 || sn.Index != tf.idx {
		t.Fatalf("imported row %+v (id %d)", sn, id)
	}
	after := tf.f.state().Clone()
	after.Snaps, after.NextSnap, after.Applied, after.LastNow = before.Snaps, before.NextSnap, before.Applied, before.LastNow
	if !reflect.DeepEqual(after, before) {
		t.Fatal("SnapshotImport changed the volume")
	}
	// After lifts the counter past every manifest on the disk, and a later begin follows
	if id := tf.ok(imp(m, 40, "older", 2)).SnapID; id != 41 || tf.row(id).State != SnapIncomplete {
		t.Fatalf("import after 40: id %d %+v", id, tf.row(id))
	}
	if id := tf.ok(imp(m, 3, "low", 0)).SnapID; id != 42 {
		t.Fatalf("an After below NextSnap: id %d, want 42", id)
	}
	if id := tf.begin(m, "next", 200); id != 43 {
		t.Fatalf("begin after the imports: id %d, want 43", id)
	}
	// no hold: an imported row is never syncing, so GC is not clamped by it
	if tf.row(41).State == SnapSyncing {
		t.Fatal("an import is syncing")
	}
}

// Invariant: I-179
func TestFSM_SnapshotName_Boundaries(t *testing.T) {
	tf, m := mirrorFSM(t)
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"", false},
		{"a", true},
		{strings.Repeat("b", MaxSnapName-1), true},
		{strings.Repeat("c", MaxSnapName), true},
		{strings.Repeat("d", MaxSnapName+1), false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"a\x00b", false},
		{"2026-10-12-0900Z", true},
	} {
		cmd := Command{Op: OpSnapshotBegin, SnapshotBegin: &SnapshotBegin{Mirror: m, Name: c.name, Now: 1}}
		if !c.ok {
			tf.fails(cmd, ErrBadCommand)
			continue
		}
		id := tf.ok(cmd).SnapID
		tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: id, OK: true, Now: 1}})
	}
}

// Invariant: I-181
func TestFSM_SnapshotEnd_Table(t *testing.T) {
	tf, m := mirrorFSM(t)
	end := func(id SnapID, ok bool, missing uint32) Command {
		return Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: id, OK: ok, Root: Hash{7}, Files: 10, Bytes: 100, NewBytes: 40, Missing: missing, Now: 9}}
	}
	tf.fails(end(77, true, 0), ErrNotExist)

	a := tf.begin(m, "a", 1)
	tf.ok(end(a, true, 0))
	if sn := tf.row(a); sn.State != SnapComplete || sn.Root != (Hash{7}) || sn.Files != 10 || sn.Bytes != 100 || sn.NewBytes != 40 {
		t.Fatalf("complete row %+v", sn)
	}
	tf.fails(end(a, true, 0), ErrNotExist) // a repeated end is harmless

	b := tf.begin(m, "b", 2)
	tf.ok(end(b, true, 3))
	if sn := tf.row(b); sn.State != SnapIncomplete || sn.Missing != 3 {
		t.Fatalf("incomplete row %+v", sn)
	}

	c := tf.begin(m, "c", 3)
	tf.ok(end(c, false, 0))
	if _, ok := tf.f.SnapshotRow(c); ok {
		t.Fatal("a failed sync left its row")
	}
	tf.begin(m, "c", 4) // the name is free again: the failed sync wrote no manifest
}

// Invariant: I-181
// Lifecycle: L-212, L-213, L-218
func TestFSM_GCDelete_HeldBySyncingRow(t *testing.T) {
	tf, m := mirrorFSM(t)
	f := tf.create(RootIno, "f")
	old, cur := sha("old"), sha("cur")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, old}}, NewChunks: []ChunkDesc{desc(old)}, Now: 10}})
	g := tf.create(RootIno, "g")
	early := sha("early")
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: g, NewSize: testCS, Writes: []ChunkWrite{{0, early}}, NewChunks: []ChunkDesc{desc(early)}, Now: 11}})
	tf.ok(Command{Op: OpUnlink, Unlink: &Unlink{Parent: RootIno, Name: "g", Now: 12}}) // early tombstoned at 12, before the snapshot

	id := tf.begin(m, "s", 20) // the snapshot references old
	// overwrite old after the begin: tombstoned at 30 while the sync runs (L-212)
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 1, NewSize: testCS, Writes: []ChunkWrite{{0, cur}}, NewChunks: []ChunkDesc{desc(cur)}, Now: 30}})

	r := tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 1 << 62}})
	if len(r.Victims) != 1 || r.Victims[0].Hash != early {
		t.Fatalf("victims during the sync %+v, want only the chunk tombstoned before it began", r.Victims)
	}
	if _, ok := tf.f.GetChunk(old); !ok {
		t.Fatal("a chunk the syncing snapshot references was collected (L-218)")
	}
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: id, OK: true, Now: 40}})
	r = tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 1 << 62}})
	if len(r.Victims) != 1 || r.Victims[0].Hash != old {
		t.Fatalf("victims after the end %+v, want the held chunk (L-213)", r.Victims)
	}
	// a failed end releases the hold as well
	tf.begin(m, "t", 50)
	tf.ok(Command{Op: OpCommit, Commit: &Commit{Ino: f, BaseVersion: 2, HasTruncate: true, NewSize: 0, Now: 60}})
	if r := tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 1 << 62}}); len(r.Victims) != 0 {
		t.Fatalf("victims %+v while syncing", r.Victims)
	}
	sn := tf.f.Snapshots()
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: sn[len(sn)-1].ID, OK: false, Now: 70}})
	if r := tf.ok(Command{Op: OpGCDelete, GCDelete: &GCDelete{Before: 1 << 62}}); len(r.Victims) != 1 || r.Victims[0].Hash != cur {
		t.Fatalf("victims after a failed sync %+v", r.Victims)
	}
}

// Invariant: I-183
func TestFSM_SnapshotDelete_TwoStep(t *testing.T) {
	tf, m := mirrorFSM(t)
	del := func(id SnapID, done bool) Command {
		return Command{Op: OpSnapshotDelete, SnapshotDelete: &SnapshotDelete{ID: id, Done: done, Now: 5}}
	}
	tf.fails(del(9, false), ErrNotExist)
	id := tf.begin(m, "a", 1)
	tf.fails(del(id, false), ErrExist) // a syncing row: end it first
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: id, OK: true, Now: 2}})
	tf.fails(del(id, true), ErrExist) // Done only after the mark
	tf.ok(del(id, false))
	if sn := tf.row(id); sn.State != SnapDeleting {
		t.Fatalf("marked row %+v", sn)
	}
	tf.ok(del(id, false)) // a second mark changes nothing
	tf.ok(del(id, true))
	if _, ok := tf.f.SnapshotRow(id); ok {
		t.Fatal("Done left the row")
	}
	tf.fails(del(id, true), ErrNotExist)
}

// Invariant: I-184
func TestFSM_MirrorRow_Table(t *testing.T) {
	tf, m := mirrorFSM(t)
	up := func(n Node) Command { return Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: n}} }
	tf.fails(up(Node{ID: "mx", Kind: KindMirror, Host: 99}), ErrBadCommand)             // no such host
	tf.fails(up(Node{ID: "mx", Kind: KindMirror, Host: m}), ErrBadCommand)              // a mirror is not a host
	tf.fails(up(Node{ID: "mx", Kind: KindMirror, Host: 1, Capacity: 1}), ErrBadCommand) // never a placement target
	tf.fails(up(Node{ID: "mx", Kind: KindMirror, Host: 1, Voter: true}), ErrBadCommand) // never a voter
	tf.fails(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: m, State: Draining, Now: 1}}, ErrBadCommand)

	// re-home: the same ID on another host keeps the row and its index
	r := tf.ok(up(Node{ID: "mirror1", Name: "n1/m1", Kind: KindMirror, Host: 2}))
	if NodeIdx(r.Ino) != m {
		t.Fatalf("re-home allocated %d, want %d", r.Ino, m)
	}
	if n, _ := tf.f.Node(m); n.Host != 2 {
		t.Fatalf("re-homed row %+v", n)
	}

	// a host leaves after its mirrors; a mirror leaves after its sync, taking its rows
	id := tf.begin(m, "a", 1)
	tf.fails(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 2, State: Removed, Now: 2}}, ErrNodeNotEmpty)
	tf.fails(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: m, State: Removed, Now: 2}}, ErrNodeNotEmpty)
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: id, OK: true, Now: 3}})
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: m, State: Removed, Now: 4}})
	if len(tf.f.Snapshots()) != 0 {
		t.Fatal("a removed mirror kept its catalogue rows")
	}
	tf.ok(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: 2, State: Removed, Now: 5}})
}

// Invariant: I-184
func TestCandidates_MirrorNeverATarget(t *testing.T) {
	nodes := []Node{
		{Idx: 1, Kind: KindDevice, State: Active, Voter: true},
		{Idx: 2, Kind: KindDisk, Host: 1, State: Active, Capacity: 100},
		{Idx: 3, Kind: KindMirror, Host: 1, State: Active, Capacity: 1 << 40}, // a capacity a buggy row might carry
	}
	for _, c := range Candidates(nodes, allLive) {
		if c.Idx == 3 {
			t.Fatal("a mirror is a placement candidate")
		}
	}
	if total, _, _ := Usage(Volume{K: 1}, nodes, Stats{}); total != 100 {
		t.Fatalf("usage counts the mirror: total %d", total)
	}
	for _, tg := range targets(nodes) {
		if tg.idx == 3 {
			t.Fatal("the policy counts the mirror")
		}
	}
	if HasTargets(nodes[2:]) {
		t.Fatal("a mirror alone is a target")
	}
	// the FSM refuses a slot on a mirror outright
	tf, m := mirrorFSM(t)
	f := tf.create(RootIno, "f")
	h := sha("x")
	d := ChunkDesc{Hash: h, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, m}}
	tf.fails(Command{Op: OpCommit, Commit: &Commit{Ino: f, NewSize: testCS, Writes: []ChunkWrite{{0, h}}, NewChunks: []ChunkDesc{d}, Now: 1}}, ErrBadCommand)
	tf.commitOne(f, h, true)
	tf.fails(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: h, K: 2, M: 1, Placement: []NodeIdx{1, 2, m}}}, ErrBadCommand)
}

// Invariant: I-184
func TestEncodeCommand_V7Gating(t *testing.T) {
	disk := MustEncodeCommand(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "d", Kind: KindDisk, Host: 1}}})
	mirror := MustEncodeCommand(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "m", Kind: KindMirror, Host: 1}}})
	if disk[0] != commandVersionV2 || mirror[0] != commandVersionV7 {
		t.Fatalf("disk upsert version %d, mirror upsert version %d", disk[0], mirror[0])
	}
	for _, c := range []Command{
		{Op: OpSnapshotBegin, SnapshotBegin: &SnapshotBegin{Mirror: 1, Name: "a"}},
		{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: 1}},
		{Op: OpSnapshotDelete, SnapshotDelete: &SnapshotDelete{ID: 1}},
		{Op: OpSnapshotImport, SnapshotImport: &SnapshotImport{Mirror: 1, Name: "a"}},
	} {
		if v := CommandCodec(c); v != CodecMirrors {
			t.Errorf("op %d is codec %d, want %d", c.Op, v, CodecMirrors)
		}
	}
	bad := map[string][]byte{
		"begin in version 6":         append([]byte{commandVersionV6}, MustEncodeCommand(Command{Op: OpSnapshotBegin, SnapshotBegin: &SnapshotBegin{Mirror: 1, Name: "a"}})[1:]...),
		"mirror upsert in version 2": append([]byte{commandVersionV2}, mirror[1:]...),
		"disk upsert in version 7":   append([]byte{commandVersionV7}, disk[1:]...),
		"unlink in version 7":        append([]byte{commandVersionV7}, MustEncodeCommand(Command{Op: OpUnlink, Unlink: &Unlink{Parent: 1, Name: "a"}})[1:]...),
		"version 8":                  append([]byte{8}, mirror[1:]...),
		"import in version 6":        append([]byte{commandVersionV6}, MustEncodeCommand(Command{Op: OpSnapshotImport, SnapshotImport: &SnapshotImport{Mirror: 1, Name: "a"}})[1:]...),
	}
	for name, in := range bad {
		if _, err := DecodeCommand(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Invariant: I-184, I-22
func TestCodec_CatalogueRoundTrip(t *testing.T) {
	tf, m := mirrorFSM(t)
	a := tf.begin(m, "a", 1)
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: a, OK: true, Root: Hash{1}, Files: 2, Bytes: 3, NewBytes: 4, Missing: 1, Now: 2}})
	b := tf.begin(m, "b", 3)
	tf.ok(Command{Op: OpSnapshotEnd, SnapshotEnd: &SnapshotEnd{ID: b, OK: true, Now: 4}})
	tf.ok(Command{Op: OpSnapshotDelete, SnapshotDelete: &SnapshotDelete{ID: b, Now: 5}})
	tf.begin(m, "c", 6)
	st := tf.f.state()
	enc := encodeState(t, st)
	got, err := DecodeState(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, st) {
		t.Fatalf("catalogue round trip\n got %+v\nwant %+v", got.Snaps, st.Snaps)
	}
	if again := encodeState(t, got); !bytes.Equal(again, enc) {
		t.Fatal("re-encoding the decoded state differs")
	}
	if got.NextSnap != 4 {
		t.Fatalf("NextSnap %d, want 4", got.NextSnap)
	}
}

// Invariant: I-184
func TestCodec_ReadsVersion5Snapshot(t *testing.T) {
	s := sampleState()
	var buf bytes.Buffer
	if err := encodeStateVersion(&buf, s, stateVersionV5); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Snaps) != 0 || got.NextSnap != 1 {
		t.Fatalf("a version 5 snapshot decoded with catalogue %v, NextSnap %d", got.Snaps, got.NextSnap)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("v5 round trip\n got %+v\nwant %+v", got, s)
	}
}

// Invariant: I-184
func TestResult_SnapIDRoundTrip(t *testing.T) {
	for _, res := range []Result{{SnapID: 7}, {Ino: 3, Gen: 4}, {Err: ErrExist}} {
		got, err := DecodeResult(EncodeResult(res))
		if err != nil || got.SnapID != res.SnapID || got.Ino != res.Ino || !errors.Is(got.Err, res.Err) {
			t.Fatalf("result %+v round trip: %+v %v", res, got, err)
		}
	}
	// a result without a SnapID is byte for byte what an older node writes and reads
	if b := EncodeResult(Result{Ino: 3}); len(b) != 1+8+4+1 {
		t.Fatalf("result without a SnapID is %d bytes", len(b))
	}
}
