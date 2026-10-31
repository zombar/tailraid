package repair_test

import (
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/ec"
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

// shardNeed is what a refill of one full chunk of the fabric's size needs on a target:
// the shard and the store's header, as PutVerified reserves it.
func shardNeed() int64 { return int64(chunk.HeaderSize + ec.ShardLen(int(testharness.ChunkSize), 2)) }

// gets sums the shard GETs the fabric has served on every host.
func gets(f *testharness.Fabric) int {
	n := 0
	for _, t := range f.Targets() {
		n += f.Gets(t)
	}
	return n
}

// A chunk whose refill has nowhere to go is never read: the plan runs before the read,
// a target without room for the shard by its last probe is no candidate, and a chunk
// with no slot to put is counted no_room and tried for nothing, so its class is stuck
// until a target has room (docs/06 §4, §4a).
// Invariant: I-419
// Lifecycle: L-223
func TestRepair_NoRoom_PlansBeforeRead(t *testing.T) {
	const n = 6
	f := testharness.NewFabric(t, 3, 2, 1)
	hashes := writeChunks(t, f, n)
	f.AddNode() // the one spare every refill would go to
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, scrubCfg(0), nil)
	free := int64(0)
	r.Room = func(meta.NodeIdx) (int64, bool) { return free, true }
	pass := func(what string, scanned, repaired, noRoom int) {
		t.Helper()
		st, err := r.Scan(ctx)
		if err != nil || st.Scanned != scanned || st.Repaired != repaired || st.NoRoom != noRoom {
			t.Fatalf("%s: scanned %d, repaired %d, no_room %d, %v; want %d, %d, %d", what, st.Scanned, st.Repaired, st.NoRoom, err, scanned, repaired, noRoom)
		}
	}
	pass("first pass as leader", n, 0, 0)
	f.SetDown(f.Targets()[0], true)
	var before []meta.Chunk
	for _, h := range hashes {
		c, _ := f.FSM.GetChunk(h)
		before = append(before, c)
	}
	gets0, puts0 := gets(f), f.Puts()
	pass("a dead holder and a spare with no room", n, 0, n)
	if g, p := gets(f), f.Puts(); g != gets0 || p != puts0 {
		t.Fatalf("a refill with nowhere to go moved bytes: %d GETs and %d PUTs", g-gets0, p-puts0)
	}
	for i, h := range hashes {
		if c, _ := f.FSM.GetChunk(h); !slices.Equal(c.Placement, before[i].Placement) {
			t.Fatalf("placement changed with nowhere to put: %v -> %v", before[i].Placement, c.Placement)
		}
	}
	pass("stuck under the same view", 0, 0, 0)
	free = shardNeed() // the spare has room now: the view changed, no StuckRetry to wait out
	pass("room appeared", n, n, 0)
}

// A target that answered ErrFull to a repair PUT is no candidate for the refills of the
// passes within FullHold, home or not: one PUT finds it full, every other chunk of the
// pass is left unread, and the next pass tries it again only once the hold has ended.
// Invariant: I-419
// Lifecycle: L-223
func TestRepair_FullMark_ExcludesTarget(t *testing.T) {
	const n = 6
	f := testharness.NewFabric(t, 3, 2, 1)
	writeChunks(t, f, n)
	spare := f.AddNode()
	cfg := scrubCfg(0)
	cfg.Workers = 1 // one chunk at a time, so the first PUT's answer governs the rest
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, cfg, nil)
	pass := func(what string, scanned, repaired, noRoom int) {
		t.Helper()
		st, err := r.Scan(ctx)
		if err != nil || st.Scanned != scanned || st.Repaired != repaired || st.NoRoom != noRoom {
			t.Fatalf("%s: scanned %d, repaired %d, no_room %d, %v; want %d, %d, %d", what, st.Scanned, st.Repaired, st.NoRoom, err, scanned, repaired, noRoom)
		}
	}
	pass("first pass as leader", n, 0, 0)
	f.SetDown(f.Targets()[0], true)
	f.SetFull(spare, true)
	gets0, puts0 := gets(f), f.Puts()
	pass("the spare answers ErrFull once", n, 0, n-1)
	if g, p := gets(f)-gets0, f.Puts()-puts0; p != 1 || g == 0 || g > 3 {
		t.Fatalf("after the first refusal: %d GETs, %d PUTs; want one chunk read and one PUT", g, p)
	}
	gets0, puts0 = gets(f), f.Puts()
	pass("within the hold: nothing read, nothing put", n, 0, n)
	if g, p := gets(f), f.Puts(); g != gets0 || p != puts0 {
		t.Fatalf("within the hold: %d GETs, %d PUTs", g-gets0, p-puts0)
	}
	pass("stuck under the held view", 0, 0, 0)
	f.Clk.Advance(repair.FullHold + time.Second)
	pass("the hold ended: tried once more", n, 0, n-1)
	if p := f.Puts(); p != puts0+1 {
		t.Fatalf("after the hold: %d PUTs, want one", p-puts0)
	}
	f.SetFull(spare, false)
	f.Clk.Advance(repair.FullHold + time.Second)
	pass("the spare has room", n, n, 0)
}

// The room a refill needs is the shard plus the store's header: a target whose last
// probe reported less is no candidate, one reporting that much or more is, and one whose
// room is unknown (a bucket, an unprobed row) counts as having room.
// Invariant: I-419
func TestRepair_RoomThreshold_Table(t *testing.T) {
	need := shardNeed()
	for _, tc := range []struct {
		name   string
		free   int64
		known  bool
		placed bool
	}{
		{"0", 0, true, false},
		{"1", 1, true, false},
		{"need-1", need - 1, true, false},
		{"need", need, true, true},
		{"need+1", need + 1, true, true},
		{"unknown", 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const n = 2
			f := testharness.NewFabric(t, 3, 2, 1)
			writeChunks(t, f, n)
			f.AddNode()
			r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, scrubCfg(0), nil)
			r.Room = func(meta.NodeIdx) (int64, bool) { return tc.free, tc.known }
			if _, err := r.Scan(ctx); err != nil {
				t.Fatal(err)
			}
			f.SetDown(f.Targets()[0], true)
			gets0 := gets(f)
			st, err := r.Scan(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.placed && (st.Repaired != n || st.NoRoom != 0) {
				t.Fatalf("free %d known %v: repaired %d, no_room %d; want every chunk refilled", tc.free, tc.known, st.Repaired, st.NoRoom)
			}
			if !tc.placed && (st.Repaired != 0 || st.NoRoom != n || gets(f) != gets0) {
				t.Fatalf("free %d known %v: repaired %d, no_room %d, %d GETs; want nothing placed and nothing read", tc.free, tc.known, st.Repaired, st.NoRoom, gets(f)-gets0)
			}
		})
	}
}
