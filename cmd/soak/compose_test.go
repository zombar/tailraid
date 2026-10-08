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
	e := newComposeEnv("x", 4)
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
