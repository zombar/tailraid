package vfs_test

import (
	"errors"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// A flush with no Active disk or S3 row in the node table fails with ErrFull (the client
// sees ENOSPC); the same failure with a disk that exists but is dead stays plain
// ErrTooFewAcks (EIO), since the data has somewhere to go once the disk returns.
// Invariant: I-120
func TestFlusher_NoTargets_ErrorWrapsErrFull(t *testing.T) {
	cases := []struct {
		name string
		prep func(f *testharness.Fabric)
		full bool
	}{
		{"no disk at all", func(f *testharness.Fabric) {
			f.MustApply(meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: f.Target(1), State: meta.Removed, Now: 2}})
		}, true},
		{"a draining disk is not a target either", func(f *testharness.Fabric) {
			f.MustApply(meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: f.Target(1), State: meta.Draining, Now: 2}})
		}, true},
		{"a dead disk", func(f *testharness.Fabric) { f.SetDown(1, true) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testharness.NewFabric(t, 1, 1, 0)
			tc.prep(f)
			ino := f.Create("a")
			must(t, f.Ov.Write(ino, 0, 0, testharness.Pattern(4096, 3)))
			err := f.Flusher().FlushInode(ctx, ino)
			if !errors.Is(err, vfs.ErrTooFewAcks) {
				t.Fatalf("flush: %v, want ErrTooFewAcks", err)
			}
			if errors.Is(err, chunk.ErrFull) != tc.full {
				t.Fatalf("flush: %v; wraps ErrFull %v, want %v", err, !tc.full, tc.full)
			}
			if len(f.Ov.Dirty()) == 0 {
				t.Fatal("the extent was dropped")
			}
		})
	}
}
