package meta

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func meshKey(b byte) MeshKey {
	var k MeshKey
	for i := range k {
		k[i] = b + byte(i)
	}
	return k
}

// The mesh ops and fields are codec 13 and only it; a row without them keeps its old
// version; a snapshot frame 11 decodes to a state without them.
// Invariant: I-326
func TestMeshOps_Codec13(t *testing.T) {
	k := meshKey(3)
	dev := Node{ID: "abcdefabcdefabcd", Name: "pike", Kind: KindDevice, Addr: "100.70.3.2", RaftPort: 1, RPCPort: 2, Key: k, MeshPort: 20483, Endpoints: []string{"192.168.1.2:20483", "[2001:db8::1]:20483"}}
	for _, c := range []Command{
		{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: k, Now: 5}},
		{Op: OpPendingDelete, PendingDelete: &PendingDelete{Key: k, Now: 6}},
		{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: dev}},
		{Op: OpMetaSet, MetaSet: &MetaSet{MeshSubnet: "100.70.3.0/24"}},
	} {
		if v := CommandCodec(c); v != commandVersionV13 || CodecMesh != commandVersionV13 || CodecVersion < commandVersionV13 {
			t.Fatalf("op %d: version %d", c.Op, v)
		}
		b := MustEncodeCommand(c)
		dec, err := DecodeCommand(b)
		if err != nil || !reflect.DeepEqual(dec, c) {
			t.Fatalf("op %d round trip: %v\n got %+v\nwant %+v", c.Op, err, dec, c)
		}
		for _, v := range []uint8{commandVersion, commandVersionV7, commandVersionV11, 12} {
			old := append([]byte(nil), b...)
			old[0] = v
			if _, err := DecodeCommand(old); err == nil {
				t.Errorf("op %d decoded in version %d", c.Op, v)
			}
		}
	}
	// a device row without mesh fields is still the oldest version
	plain := Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "abcdefabcdefabcd", Kind: KindDevice, Addr: "100.64.0.1"}}}
	if v := CommandCodec(plain); v != commandVersion {
		t.Fatalf("a plain device row is version %d", v)
	}
	if v := CommandCodec(Command{Op: OpMetaSet, MetaSet: &MetaSet{JoinToken: "x"}}); v != commandVersion {
		t.Fatalf("a plain MetaSet is version %d", v)
	}
	// a result carries the address only when set, after the older optional fields
	for _, res := range []Result{{Addr: "100.70.3.9"}, {Ino: 4, SnapID: 2, Addr: "100.70.3.9"}, {Version: 7, Addr: "100.70.3.1"}, {Ino: 1}} {
		got, err := DecodeResult(EncodeResult(res))
		if err != nil || !reflect.DeepEqual(got, res) {
			t.Fatalf("result %+v: %+v %v", res, got, err)
		}
	}
}

// The subnet is set once; an introduce gets the next host, the same key the same host,
// a key an active row holds that row's address; the join's upsert consumes the pending
// row; the delete expires one; the bounds hold.
// Invariant: I-319, I-320, I-322
func TestPendingApply_Table(t *testing.T) {
	f := NewFSM()
	apply := func(c Command) Result {
		t.Helper()
		return applyOne(t, f, c)
	}
	k1, k2, k3 := meshKey(1), meshKey(2), meshKey(3)
	if r := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: k1, Now: 1}}); !errors.Is(r.Err, ErrNoSubnet) {
		t.Fatalf("before a subnet: %v", r.Err)
	}
	for _, bad := range []string{"not a prefix", "fd00::/64", "100.70.0.0/16"} {
		if r := apply(Command{Op: OpMetaSet, MetaSet: &MetaSet{MeshSubnet: bad}}); !errors.Is(r.Err, ErrBadCommand) {
			t.Fatalf("subnet %q: %v", bad, r.Err)
		}
	}
	if r := apply(Command{Op: OpMetaSet, MetaSet: &MetaSet{MeshSubnet: "100.70.3.77/24"}}); r.Err != nil {
		t.Fatal(r.Err)
	}
	if m := f.Meta(); m.MeshSubnet != "100.70.3.0/24" || m.NextMeshHost != 2 || m.MeshPrefix().String() != "100.70.3.0/24" {
		t.Fatalf("meta after the subnet: %+v", m)
	}
	if r := apply(Command{Op: OpMetaSet, MetaSet: &MetaSet{MeshSubnet: "100.70.4.0/24"}}); !errors.Is(r.Err, ErrBadCommand) {
		t.Fatalf("a second subnet: %v", r.Err)
	}
	if r := apply(Command{Op: OpMetaSet, MetaSet: &MetaSet{MeshSubnet: "100.70.3.0/24"}}); r.Err != nil {
		t.Fatalf("the same subnet again: %v", r.Err)
	}
	r1 := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: k1, Now: 10}})
	r2 := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: k2, Now: 11}})
	again := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: k1, Now: 12}})
	if r1.Err != nil || r2.Err != nil || again.Err != nil || r1.Addr != "100.70.3.2" || r2.Addr != "100.70.3.3" || again.Addr != r1.Addr {
		t.Fatalf("allocation: %+v %+v %+v", r1, r2, again)
	}
	if p := f.Pending(); len(p) != 2 || p[0].Key != k1 || p[0].At != 10 || p[1].Addr != "100.70.3.3" {
		t.Fatalf("pending %+v", p)
	}
	if p, ok := f.PendingByAddr("100.70.3.3"); !ok || p.Key != k2 {
		t.Fatalf("PendingByAddr: %+v %v", p, ok)
	}
	if _, ok := f.PendingByAddr("100.70.3.9"); ok {
		t.Fatal("a free address has a pending row")
	}
	// the join: a device row with the key consumes the pending row
	dev := Node{ID: "abcdefabcdefabcd", Name: "pike", Kind: KindDevice, Addr: r1.Addr, RaftPort: 1, RPCPort: 2, Key: k1, MeshPort: 20483, Endpoints: []string{"203.0.113.9:20483"}}
	if r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: dev}}); r.Err != nil {
		t.Fatal(r.Err)
	}
	if p := f.Pending(); len(p) != 1 || p[0].Key != k2 {
		t.Fatalf("after the join: %+v", p)
	}
	if row, ok := f.NodeByKey(k1); !ok || row.Addr != r1.Addr || row.MeshPort != 20483 || len(row.Endpoints) != 1 {
		t.Fatalf("NodeByKey: %+v %v", row, ok)
	}
	if _, ok := f.NodeByKey(MeshKey{}); ok {
		t.Fatal("the zero key found a row")
	}
	// the member's key introduced again: its own address, no new row
	if r := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: k1, Now: 20}}); r.Err != nil || r.Addr != r1.Addr || len(f.Pending()) != 1 {
		t.Fatalf("a member's introduce: %+v", r)
	}
	// expiry
	if r := apply(Command{Op: OpPendingDelete, PendingDelete: &PendingDelete{Key: k2, Now: 21}}); r.Err != nil || len(f.Pending()) != 0 {
		t.Fatalf("delete: %v", r.Err)
	}
	if r := apply(Command{Op: OpPendingDelete, PendingDelete: &PendingDelete{Key: k2, Now: 22}}); !errors.Is(r.Err, ErrNoPending) {
		t.Fatalf("delete twice: %v", r.Err)
	}
	if _, err := EncodeCommand(Command{Op: OpPendingAdd}); !errors.Is(err, ErrBadCommand) {
		t.Fatalf("an op without a payload: %v", err)
	}
	for _, c := range []Command{
		{Op: OpPendingAdd, PendingAdd: &PendingAdd{Now: 1}},
		{Op: OpPendingDelete, PendingDelete: &PendingDelete{Now: 1}},
	} {
		if r := apply(c); !errors.Is(r.Err, ErrBadCommand) {
			t.Fatalf("op %d without a key: %v", c.Op, r.Err)
		}
	}
	// endpoints bounds on a row: {max, max+1}, an empty one, an overlong one
	many := make([]string, MaxEndpoints+1)
	for i := range many {
		many[i] = "10.0.0.1:1"
	}
	okRow := dev
	okRow.Endpoints = many[:MaxEndpoints]
	if r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: okRow}}); r.Err != nil {
		t.Fatalf("%d endpoints: %v", MaxEndpoints, r.Err)
	}
	for _, eps := range [][]string{many, {""}, {string(bytes.Repeat([]byte{'x'}, MaxEndpoint+1))}} {
		bad := dev
		bad.Endpoints = eps
		if r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: bad}}); !errors.Is(r.Err, ErrBadCommand) {
			t.Fatalf("%d endpoints (%q): %v", len(eps), eps[0][:min(4, len(eps[0]))], r.Err)
		}
	}
	// the lowest free host: k2's row went, so .3 is free again while .2 is pike's
	if r := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: k3, Now: 30}}); r.Err != nil || r.Addr != "100.70.3.3" {
		t.Fatalf("the freed host: %+v", r)
	}
}

// Hosts: the lowest free one is allocated, a pending row that expired or was deleted
// frees its host, a member's address (removed or not) is never reused, the subnet
// fills at 253 pending rows and empties again as they go, and a token rotation ends
// every pending row at once (docs/20 §3, §4).
// Invariant: I-319, I-320
func TestPendingApply_HostsReusedAndRotationEndsPending(t *testing.T) {
	f := NewFSM()
	apply := func(c Command) Result { t.Helper(); return applyOne(t, f, c) }
	if r := apply(Command{Op: OpMetaSet, MetaSet: &MetaSet{JoinToken: "abcdefghabcdefgh", MeshSubnet: "100.70.3.0/24"}}); r.Err != nil {
		t.Fatal(r.Err)
	}
	// a member at .2 and a removed member at .3: neither host is ever handed out again
	for i, st := range []NodeState{Active, Removed} {
		r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: fmt.Sprintf("m%d", i), Name: fmt.Sprintf("m%d", i), Kind: KindDevice, Addr: fmt.Sprintf("100.70.3.%d", i+2), RaftPort: 1, RPCPort: 2, Key: meshKey(byte(i + 1))}}})
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		if st == Removed {
			if r := apply(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: NodeIdx(r.Ino), State: Removed, Now: 1}}); r.Err != nil {
				t.Fatal(r.Err)
			}
		}
	}
	key := func(i int) MeshKey {
		var k MeshKey
		k[0], k[1], k[2] = 0xee, byte(i>>8), byte(i)
		return k
	}
	// every host from .4 to .254: 251 pending rows, then the subnet is full
	for i := range 251 {
		r := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: key(i), Now: 10}})
		if r.Err != nil || r.Addr != fmt.Sprintf("100.70.3.%d", i+4) {
			t.Fatalf("pending row %d: %+v", i, r)
		}
	}
	if r := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: key(900), Now: 11}}); !errors.Is(r.Err, ErrMeshFull) {
		t.Fatalf("a full subnet: %v", r.Err)
	}
	// one row expires: its host, and no other, is handed out again
	if r := apply(Command{Op: OpPendingDelete, PendingDelete: &PendingDelete{Key: key(100), Now: 12}}); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: key(900), Now: 13}}); r.Err != nil || r.Addr != "100.70.3.104" {
		t.Fatalf("the freed host: %+v", r)
	}
	if r := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: key(901), Now: 14}}); !errors.Is(r.Err, ErrMeshFull) {
		t.Fatalf("full again: %v", r.Err)
	}
	// a rotation ends every pending row; the same token again ends nothing
	if r := apply(Command{Op: OpMetaSet, MetaSet: &MetaSet{JoinToken: "zzzzzzzzzzzzzzzz"}}); r.Err != nil || len(f.Pending()) != 0 {
		t.Fatalf("after the rotation: %d pending rows, %v", len(f.Pending()), r.Err)
	}
	if r := apply(Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: key(1), Now: 15}}); r.Err != nil || r.Addr != "100.70.3.4" {
		t.Fatalf("after the rotation the hosts are free again: %+v", r)
	}
	if r := apply(Command{Op: OpMetaSet, MetaSet: &MetaSet{JoinToken: "zzzzzzzzzzzzzzzz"}}); r.Err != nil || len(f.Pending()) != 1 {
		t.Fatalf("the same token again ended a pending row: %d", len(f.Pending()))
	}
}

// The mesh fields survive a snapshot byte for byte; a frame 11 decodes without them;
// Clone copies them.
// Invariant: I-326
func TestMeshState_Snapshot(t *testing.T) {
	f := NewFSM()
	applyOne(t, f, Command{Op: OpMetaSet, MetaSet: &MetaSet{MeshSubnet: "100.70.3.0/24"}})
	applyOne(t, f, Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: meshKey(9), Now: 1}})
	applyOne(t, f, Command{Op: OpPendingAdd, PendingAdd: &PendingAdd{Key: meshKey(2), Now: 2}})
	dev := Node{ID: "abcdefabcdefabcd", Name: "pike", Kind: KindDevice, Addr: "100.70.3.1", RaftPort: 1, RPCPort: 2, Key: meshKey(5), MeshPort: 20483, Endpoints: []string{"192.168.1.2:20483"}}
	applyOne(t, f, Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: dev}})
	var a, b bytes.Buffer
	if err := EncodeState(&a, f.st); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeState(bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.MeshSubnet != "100.70.3.0/24" || got.Meta.NextMeshHost != 4 || len(got.Pending) != 2 || got.Pending[meshKey(9)].Addr != "100.70.3.2" {
		t.Fatalf("decoded meta %+v pending %d", got.Meta, len(got.Pending))
	}
	if row := got.Nodes[1]; row.Key != meshKey(5) || row.MeshPort != 20483 || len(row.Endpoints) != 1 {
		t.Fatalf("decoded row %+v", row)
	}
	if err := EncodeState(&b, got); err != nil || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("re-encoding differs: %v", err)
	}
	if !reflect.DeepEqual(got, f.st) {
		t.Fatalf("round trip\n got %+v\nwant %+v", got, f.st)
	}
	c := f.st.Clone()
	c.Pending[meshKey(9)].Addr = "x"
	c.Nodes[1].Endpoints[0] = "y"
	if f.st.Pending[meshKey(9)].Addr == "x" || f.st.Nodes[1].Endpoints[0] == "y" {
		t.Fatal("Clone shares the mesh fields")
	}
	var old bytes.Buffer
	if err := encodeStateVersion(&old, f.st, stateVersionV11); err != nil {
		t.Fatal(err)
	}
	o, err := DecodeState(bytes.NewReader(old.Bytes()))
	if err != nil || len(o.Pending) != 0 || o.Meta.MeshSubnet != "" || !o.Nodes[1].Key.IsZero() || o.Nodes[1].Endpoints != nil {
		t.Fatalf("frame 11: %v %+v %+v", err, o.Meta, o.Nodes[1])
	}
	// a pending table out of order is refused
	raw := a.Bytes()
	k2, k9 := meshKey(2), meshKey(9)
	i := bytes.Index(raw, k2[:])
	j := bytes.Index(raw, k9[:])
	if i < 0 || j < 0 || i > j {
		t.Fatalf("pending keys not in order in the frame: %d %d", i, j)
	}
	swapped := append([]byte(nil), raw...)
	copy(swapped[i:], k9[:])
	copy(swapped[j:], k2[:])
	if _, err := DecodeState(bytes.NewReader(swapped)); err == nil {
		t.Fatal("a frame with pending rows out of order decoded")
	}
}

// One active device row per key: a fresh row that repeats another's key is refused, a
// row keeps its own on an update, and a removed row's key is free again (docs/20 §3).
// Invariant: I-318
func TestNodeUpsert_RefusesDuplicateKeyOrAddr(t *testing.T) {
	f := NewFSM()
	apply := func(c Command) Result { t.Helper(); return applyOne(t, f, c) }
	k1, k2 := meshKey(1), meshKey(2)
	first := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "n1", Name: "one", Kind: KindDevice, Addr: "100.70.3.1", Key: k1}}})
	if first.Err != nil {
		t.Fatal(first.Err)
	}
	if r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "n2", Name: "two", Kind: KindDevice, Addr: "100.70.3.2", Key: k1}}}); !errors.Is(r.Err, ErrExist) {
		t.Errorf("a fresh row with the same key: %v, want ErrExist", r.Err)
	}
	if r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "n3", Name: "three", Kind: KindDevice, Addr: "100.70.3.3", Key: k2}}}); r.Err != nil {
		t.Fatalf("another key: %v", r.Err)
	}
	// the row's own update (a re-key to a new node ID) passes
	if r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{Idx: NodeIdx(first.Ino), ID: "n1b", Name: "one", Kind: KindDevice, Addr: "100.70.3.1", Key: k1}}}); r.Err != nil {
		t.Fatalf("the row's own update: %v", r.Err)
	}
	if r := apply(Command{Op: OpNodeSetState, NodeSetState: &NodeSetState{Idx: NodeIdx(first.Ino), State: Removed, Now: 1}}); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: "n9", Name: "nine", Kind: KindDevice, Addr: "100.70.3.9", Key: k1}}}); r.Err != nil {
		t.Fatalf("a removed row's key on a fresh row: %v", r.Err)
	}
	// keyless rows (the rigs under dev.mesh_fake) share an address as they always did
	for _, id := range []string{"f1", "f2"} {
		if r := apply(Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: id, Name: id, Kind: KindDevice, Addr: "127.0.0.1"}}}); r.Err != nil {
			t.Fatalf("keyless row %s at a shared address: %v", id, r.Err)
		}
	}
}

// Every mesh sentinel has a wire code, so a forwarded PendingAdd's refusal reaches the
// introducing member as the error it was and never as an empty address.
// Invariant: I-319
func TestErrorCode_MeshSentinels(t *testing.T) {
	for _, e := range []error{ErrMeshFull, ErrNoSubnet, ErrNoPending, ErrPendingFul} {
		c := ErrorCode(e)
		if c == 0 {
			t.Errorf("%v has code 0", e)
			continue
		}
		if got, ok := ErrorFromCode(c); !ok || !errors.Is(got, e) {
			t.Errorf("ErrorFromCode(%d) = %v, %v; want %v", c, got, ok, e)
		}
	}
}
