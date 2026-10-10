package cluster

import (
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// Invariant: I-90
func TestMember_AddDisk_ResolvesToHost(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	self, _ := n.SelfIdx()
	op := rpc.MemberOp{Op: "add_disk", NodeIdx: self, DiskID: "abcd0123abcd0123", Name: "a/1", Capacity: 7}
	if err := n.MemberOp(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := n.MemberOp(ctx, op); err != nil { // a retry updates, never allocates a second row
		t.Fatal(err)
	}
	var disk meta.Node
	count := 0
	for _, nd := range n.FSM.Nodes() {
		if nd.Kind == meta.KindDisk {
			disk = nd
			count++
		}
	}
	if count != 1 || disk.Host != self || disk.ID != op.DiskID || disk.Capacity != 7 || disk.JoinedAt == 0 || disk.Voter {
		t.Fatalf("disk row %+v (count %d)", disk, count)
	}
	// the bootstrap node has no tailnet address yet; give it one so it resolves
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "update_addr", NodeIdx: self, Addr: "100.64.0.9"}); err != nil {
		t.Fatal(err)
	}
	hostAddr, hostSelf, hostOK := n.RPCAddrOf(self)
	addr, isSelf, ok := n.RPCAddrOf(disk.Idx)
	if !hostOK || addr != hostAddr || isSelf != hostSelf || !ok || !isSelf {
		t.Fatalf("RPCAddrOf(disk) = %q %v %v, want the host's %q %v", addr, isSelf, ok, hostAddr, hostSelf)
	}
	if id, ok := n.DiskOf(disk.Idx); !ok || id != op.DiskID {
		t.Fatalf("DiskOf = %q %v", id, ok)
	}
	if _, ok := n.DiskOf(self); ok {
		t.Fatal("a device is not a disk")
	}
	// the leader names disks: a second add with a stale name gets the next free number,
	// so two quick add-disks on one host never share a name (docs/14 §2)
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "add_disk", NodeIdx: self, DiskID: "abcd0123abcd0124", Name: "a/1", Capacity: 7}); err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, nd := range n.FSM.Nodes() {
		if nd.Kind == meta.KindDisk {
			names[nd.Name]++
		}
	}
	if len(names) != 2 || names["a/1"] != 1 {
		t.Fatalf("disk names %v", names)
	}
	// a disk needs an Active device host
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "add_disk", NodeIdx: disk.Idx, DiskID: "ffff0000ffff0000"}); !errors.Is(err, meta.ErrBadCommand) {
		t.Fatalf("disk on a disk: %v", err)
	}
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "add_disk", NodeIdx: self}); !errors.Is(err, ErrBadMemberOp) {
		t.Fatalf("no disk id: %v", err)
	}
}

// set_version rewrites only the version of a row: a node's row names the version its
// serve runs, not the one it joined with (docs/02 §Lifecycle commands).
// Invariant: I-133
func TestMember_SetVersion(t *testing.T) {
	n := bootstrap(t, t.TempDir())
	defer n.Close()
	self, _ := n.SelfIdx()
	before, _ := n.FSM.Node(self)
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "set_version", NodeIdx: self, Version: "v9.9.9"}); err != nil {
		t.Fatal(err)
	}
	after, _ := n.FSM.Node(self)
	if after.Version != "v9.9.9" {
		t.Fatalf("version %q, want v9.9.9", after.Version)
	}
	after.Version = before.Version
	if after != before {
		t.Fatalf("set_version changed more than the version:\n%+v\n%+v", before, after)
	}
	if err := n.MemberOp(ctx, rpc.MemberOp{Op: "set_version", NodeIdx: self}); !errors.Is(err, ErrBadMemberOp) {
		t.Fatalf("empty version: %v, want ErrBadMemberOp", err)
	}
}
