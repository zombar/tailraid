package repair_test

import (
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/testharness"
)

// With repair.relay avoid, a scrub whose every live holder sits behind a relay waits
// for a direct path (deferred, not an error, not scrubbed), one with a direct holder
// reads from that; a home move whose target is relayed waits too, and runs once the
// path is direct again.
// Invariant: I-329
func TestRepair_RelayedPathsDeferred(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	cfg := repair.DefaultConfig()
	cfg.AvoidRelay = true
	r := repair.New(f.FSM, f, f, f, f.ReaderNoCache(), f.Clk, cfg, nil)
	self := f.Self()
	hostOf := map[meta.NodeIdx]meta.NodeIdx{}
	for _, n := range f.FSM.Nodes() {
		if n.Kind == meta.KindDisk {
			hostOf[n.Idx] = n.Host
		}
	}
	// a chunk with no slot on this host: every holder can be put behind a relay
	var c meta.Chunk
	for i := range 64 {
		cand := flushedNamed(t, f, "f", i)
		onSelf := false
		for _, n := range cand.Placement {
			onSelf = onSelf || hostOf[n] == self
		}
		if !onSelf {
			c = cand
			break
		}
	}
	if c.Hash == meta.HashZero {
		t.Fatal("no chunk off this host in 64")
	}
	relayAll := func(class meta.PathClass) {
		for _, n := range c.Placement {
			f.SetClass(n, class)
		}
	}
	relayAll(meta.PathRelay)
	st, err := r.Scrub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Deferred == 0 || st.Errors != 0 {
		t.Fatalf("every holder relayed: %+v, want the chunk deferred without an error", st)
	}
	f.SetClass(c.Placement[0], meta.PathLAN) // one direct holder: the scrub reads what it can
	st, err = r.Scrub(ctx)
	if err != nil || st.Deferred != 0 || st.Errors != 0 {
		t.Fatalf("one direct holder: %+v %v", st, err)
	}
	relayAll(meta.PathLAN)

	// a slot moved off its home to a disk of another host: its home target relayed, the
	// move waits; direct again, it runs
	var spare meta.NodeIdx
	for disk, host := range hostOf {
		used := false
		for _, n := range c.Placement {
			used = used || hostOf[n] == host
		}
		if !used {
			spare = disk
		}
	}
	if spare == 0 {
		t.Fatal("no spare host")
	}
	away := append([]meta.NodeIdx(nil), c.Placement...)
	home := away[2]
	away[2] = spare
	f.MustApply(meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, K: c.K, M: c.M, Placement: away, Expect: c.Placement}})
	if plan := r.PlanHome(); plan.AwaySlots == 0 {
		t.Fatalf("the moved slot is not away: %+v", plan)
	}
	f.SetClass(home, meta.PathRelay)
	r.OpenHome()
	st, err = r.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := f.FSM.GetChunk(c.Hash)
	if st.Homed != 0 || cur.Placement[2] != spare || st.Deferred == 0 {
		t.Fatalf("home target relayed: %+v, placement %v", st, cur.Placement)
	}
	f.SetClass(home, meta.PathLAN)
	r.OpenHome()
	st, err = r.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ = f.FSM.GetChunk(c.Hash)
	if st.Homed != 1 || cur.Placement[2] != home {
		t.Fatalf("home target direct: %+v, placement %v, want slot 2 back on %d", st, cur.Placement, home)
	}
}

// flushedNamed is flushedChunk for a file named name<i> with its own content.
func flushedNamed(t *testing.T, f *testharness.Fabric, name string, i int) meta.Chunk {
	t.Helper()
	ino := f.Create(name + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	if err := f.Ov.Write(ino, 0, 0, testharness.Pattern(int(testharness.ChunkSize), byte(11+i))); err != nil {
		t.Fatal(err)
	}
	if err := f.Flusher().FlushInode(ctx, ino); err != nil {
		t.Fatal(err)
	}
	c, ok := f.FSM.GetChunk(f.Inode(ino).Chunks[0])
	if !ok {
		t.Fatal("no chunk after flush")
	}
	return c
}
