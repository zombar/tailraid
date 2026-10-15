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
	members := []admin.NodeReport{{Idx: 1, Name: "n1"}, {Idx: 2, Name: "n3"}, {Idx: 3, Name: "n4"}, {Idx: 4, Name: "n2"}}
	if err := resolveIndices(e.nodes, members); err != nil {
		t.Fatal(err)
	}
	want := map[string]meta.NodeIdx{"n1": 1, "n2": 4, "n3": 2, "n4": 3}
	for _, n := range e.nodes {
		if n.ref.Idx != want[n.ref.Name] {
			t.Fatalf("%s has idx %d, want %d", n.ref.Name, n.ref.Idx, want[n.ref.Name])
		}
	}
	if got := e.node(soak.NodeRef{Name: "n2", Idx: 4}); got.ref.RPC != "172.30.0.12:20481" {
		t.Fatalf("node(n2) = %+v", got)
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
