package repair_test

import (
	"slices"
	"testing"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

// A pass visits every chunk only when it sweeps (the first pass as leader, the pass after
// repairs re-placed chunks, every SweepInterval); any other pass visits only the chunks of
// classes that need work and the chunks with a reported slot, so an idle volume costs a
// pass nothing per chunk (docs/06 §4a).
// Invariant: I-379
func TestRepair_PassVisitsOnlyWhereWorkIs(t *testing.T) {
	const n = 12
	f := testharness.NewFabric(t, 3, 2, 1)
	hashes := writeChunks(t, f, n)
	cfg := scrubCfg(0)
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, cfg, nil)
	pass := func(what string, swept bool, scanned, repaired int) {
		t.Helper()
		st, err := r.Scan(ctx)
		if err != nil || st.Swept != swept || st.Scanned != scanned || st.Repaired != repaired {
			t.Fatalf("%s: swept %v, scanned %d, repaired %d, %v; want %v, %d, %d", what, st.Swept, st.Scanned, st.Repaired, err, swept, scanned, repaired)
		}
	}
	pass("first pass as leader", true, n, 0)
	pass("an idle pass", false, 0, 0)
	// one chunk loses a slot: its class needs work, and only it is visited
	c, _ := f.FSM.GetChunk(hashes[3])
	place := slices.Clone(c.Placement)
	place[0] = 0
	f.MustApply(meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, K: c.K, M: c.M, Placement: place}})
	pass("a missing slot", false, 1, 1)
	pass("after a repair: the homes are counted again", true, n, 0)
	pass("idle again", false, 0, 0)
	// a reader's report: that chunk alone, re-verified by a HEAD
	r.ReportBad(chunk.Key(hashes[7], c.K, c.M, 1), 0)
	pass("a reported slot", false, 1, 0)
	f.Clk.Advance(cfg.SweepInterval)
	pass("a SweepInterval later", true, n, 0)
}

// A class whose chunks a pass could do nothing for (a holder dead past fill_after and no
// spare target) is skipped by the passes after it until the view it was judged under
// changes (a target joins) or StuckRetry passes; a failed attempt is no reason to stop.
// Invariant: I-380
func TestRepair_StuckClassWaitsForTheView(t *testing.T) {
	const n = 6
	f := testharness.NewFabric(t, 3, 2, 1)
	writeChunks(t, f, n)
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, scrubCfg(0), nil)
	if _, err := r.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	f.SetDown(f.Targets()[0], true) // dead past every hold-down, and no fourth target to refill on
	pass := func(what string, scanned, repaired int) {
		t.Helper()
		st, err := r.Scan(ctx)
		if err != nil || st.Scanned != scanned || st.Repaired != repaired {
			t.Fatalf("%s: scanned %d, repaired %d, %v; want %d, %d", what, st.Scanned, st.Repaired, err, scanned, repaired)
		}
	}
	pass("the dead holder's class, nowhere to refill", n, 0)
	pass("stuck under the same view", 0, 0)
	f.Clk.Advance(repair.StuckRetry)
	pass("StuckRetry later", n, 0)
	pass("stuck again", 0, 0)
	f.AddNode() // a spare joins: the view changed
	pass("a target joined", n, n)
}
