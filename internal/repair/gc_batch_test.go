package repair_test

import (
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

// A backlog of tombstones drains in batches of GC.Batch, one GCDelete entry each, within
// one RunOnce: a large rm -rf never becomes one long apply.
//
// Invariant: I-103
func TestGC_DrainsBacklogInBatches(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	const files = 5
	for i := range files {
		ino := f.Create(string(rune('a' + i)))
		if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), byte(10+i))); err != nil {
			t.Fatal(err)
		}
		if err := f.Flusher().FlushInode(ctx, ino); err != nil {
			t.Fatal(err)
		}
	}
	tomb := f.Clk.Now()
	for i := range files {
		f.MustApply(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: string(rune('a' + i)), Now: tomb.UnixNano()}})
	}
	f.Clk.Set(tomb.Add(11 * time.Minute))
	gc := repair.NewGC(f.FSM, f, f, f, f.Clk, 10*time.Minute, nil)
	gc.Batch = 2
	res, err := gc.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Victims != files {
		t.Fatalf("victims %d, want %d collected across batches of 2", res.Victims, files)
	}
	if st := f.FSM.Stats(); st.Tombstoned != 0 {
		t.Fatalf("%d tombstones left", st.Tombstoned)
	}
}
