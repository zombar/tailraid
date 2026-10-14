package main

import (
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/soak"
)

// The cluster assigns node indices in join order, which is whatever order the containers
// came up in; the driver takes them from status by name, never from the container number
// (a corrupt or kill aimed at "node 2" must reach the container that is node 2).
func TestCompose_IndicesFromStatusByName(t *testing.T) {
	e := newComposeEnv("x", "docker-compose.yml", 4)
	dev := func(idx meta.NodeIdx, name, addr string) admin.NodeReport {
		return admin.NodeReport{Idx: idx, Name: name, Kind: "device", State: "active", Addr: addr, RPCPort: 20481}
	}
	// the mesh addresses are the cluster's, in join order (docs/20 §3); a disk row and a
	// removed row of the same name never count
	members := []admin.NodeReport{
		dev(1, "n1", "100.100.0.1"), dev(2, "n3", "100.100.0.2"), dev(3, "n4", "100.100.0.3"), dev(4, "n2", "100.100.0.4"),
		{Idx: 5, Name: "n1/1", Kind: "disk", Host: 1},
		{Idx: 6, Name: "n2", Kind: "device", State: "removed", Addr: "100.100.0.9"},
	}
	if got := e.node(soak.NodeRef{Name: "n2"}); got.ref.RPC != "172.30.0.12:20481" {
		t.Fatalf("before status, node(n2) = %+v, want the container's address as its label", got)
	}
	if err := resolveIndices(e.nodes, members); err != nil {
		t.Fatal(err)
	}
	want := map[string]meta.NodeIdx{"n1": 1, "n2": 4, "n3": 2, "n4": 3}
	for _, n := range e.nodes {
		if n.ref.Idx != want[n.ref.Name] {
			t.Fatalf("%s has idx %d, want %d", n.ref.Name, n.ref.Idx, want[n.ref.Name])
		}
	}
	if got := e.node(soak.NodeRef{Name: "n2", Idx: 4}); got.ref.RPC != "100.100.0.4:20481" || got.addr != "100.100.0.4" || got.ip != "172.30.0.12" {
		t.Fatalf("node(n2) = %+v", got)
	}
	// the worker routes by either address: the seed label before, the mesh address after
	for _, addr := range []string{"172.30.0.12:20481", "100.100.0.4:20481"} {
		if ref, ok := e.byRPC(addr); !ok || ref.Name != "n2" {
			t.Fatalf("byRPC(%s) = %+v %v", addr, ref, ok)
		}
	}
	if err := resolveIndices(e.nodes, members[:3]); err == nil {
		t.Fatal("a missing member was not reported")
	}
}

// A disk "<host>/<n>" lives at /disks/d<n> in its host's container (test/soak/entry.sh).
// Invariant: I-86
func TestCompose_DiskDir(t *testing.T) {
	for _, c := range []struct {
		name, want string
		ok         bool
	}{{"n2/1", "/disks/d1", true}, {"n3/2", "/disks/d2", true}, {"n2", "", false}, {"n2/", "", false}} {
		got, err := diskDir(soak.NodeRef{Name: c.name, Host: "n2"})
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
	}
}
