package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// serve refuses a restored state in which a device row carries a budget or a chunk slot
// names a device, before any listener opens; a disks-only state passes (docs/14 §9).
// Invariant: I-119
func TestServe_RefusesLegacyState(t *testing.T) {
	cases := []struct {
		name string
		prep func(t *testing.T, f *testharness.Fabric)
		want bool
	}{
		{"disks only", func(*testing.T, *testharness.Fabric) {}, false},
		{"a device row with a budget", func(_ *testing.T, f *testharness.Fabric) {
			nd, _ := f.FSM.Node(1)
			nd.Capacity = 5
			f.MustApply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
		}, true},
		{"a chunk slot on a device", func(t *testing.T, f *testharness.Fabric) {
			ino := f.Create("a")
			if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(4096, 1)); err != nil {
				t.Fatal(err)
			}
			if err := f.Flusher().FlushInode(t.Context(), ino); err != nil {
				t.Fatal(err)
			}
			h := f.Inode(ino).Chunks[0]
			f.MustApply(meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: h, Placement: []meta.NodeIdx{1}, K: 1, M: 0}})
		}, true},
		{"a removed device with a budget is history", func(_ *testing.T, f *testharness.Fabric) {
			r := f.MustApply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{Name: "old", Kind: meta.KindDevice, Capacity: 7}}})
			f.MustApply(meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: meta.NodeIdx(r.Ino), State: meta.Removed, Now: 3}})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 1, 1, 0)
			tc.prep(t, f)
			err := legacyCheck(f.FSM)
			if errors.Is(err, ErrLegacyState) != tc.want {
				t.Fatalf("legacyCheck: %v, want legacy %v", err, tc.want)
			}
			if tc.want && !strings.Contains(err.Error(), "tailraid reset --yes") {
				t.Fatalf("no way out in %v", err)
			}
		})
	}
}
