package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type allLive struct{}

func (allLive) IsLive(meta.NodeIdx) bool           { return true }
func (allLive) DeadFor(meta.NodeIdx) time.Duration { return 0 }
func (allLive) Class(meta.NodeIdx) meta.PathClass  { return meta.PathLAN }

// `check` is not OK when the scan it ran found more corrupt shards than parity can cover
// (m+1 corrupt, L-106) or anything unrepairable, even though liveness alone looks fine;
// the degraded files are listed from the same derivation status uses.
// Invariant: I-44, I-56
// Lifecycle: L-106
func TestCheck_MPlusOneCorrupt_NotOK(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("f")
	must(t, f.Ov.Write(ino, 0, 4096, testharness.Pattern(int(testharness.ChunkSize), 5)))
	must(t, f.Flusher().FlushInode(ctx, ino))
	clean := admin.Check(f.FSM, allLive{}, 10*time.Minute, repair.Stats{Scanned: 2, Scrubbed: 2}, nil, true)
	if !clean.OK || clean.Degraded != 0 || clean.Unreadable != 0 || len(clean.Files) != 0 {
		t.Fatalf("clean cluster: %+v", clean)
	}
	bad := admin.Check(f.FSM, allLive{}, 10*time.Minute, repair.Stats{Scanned: 2, Scrubbed: 2, ScrubBad: 2, Unrepairable: 1}, nil, true)
	if bad.OK {
		t.Fatalf("m+1 corrupt shards reported OK: %+v", bad)
	}
	// a missing slot in the FSM is degraded and names the file
	c, _ := f.FSM.GetChunk(f.Inode(ino).Chunks[0])
	place := append([]meta.NodeIdx(nil), c.Placement...)
	place[2] = 0
	f.MustApply(meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, K: c.K, M: c.M, Placement: place}})
	deg := admin.Check(f.FSM, allLive{}, 10*time.Minute, repair.Stats{}, nil, true)
	if deg.OK || deg.Degraded != 1 || deg.RepairPending != 1 || deg.Unreadable != 0 || len(deg.Files) != 1 || deg.Files[0] != "/f" {
		t.Fatalf("degraded: %+v", deg)
	}
}
