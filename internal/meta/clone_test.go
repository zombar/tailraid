package meta

import (
	"bytes"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/testseed"
)

// A clone of ten thousand chunks allocates no chunk: the entries are shared, never
// copied, so a Raft snapshot's clone under the lock costs a pointer map copy (I-383).
// Invariant: I-383
func TestState_Clone_SharesChunkEntries(t *testing.T) {
	const n = 10_000
	st := NewState()
	for i := range n {
		h := hashN(uint64(i) + 1)
		st.Chunks[h] = &Chunk{Hash: h, Size: testCS, K: 3, M: 2, Refs: 1, Placement: []NodeIdx{1, 2, 3, 4, 5}, Sums: []Hash{h, h, h}}
	}
	var c *State
	if allocs := testing.AllocsPerRun(3, func() { c = st.Clone() }); allocs > 100 {
		t.Fatalf("a clone of %d chunks made %.0f allocations", n, allocs)
	}
	h := hashN(1)
	if c.Chunks[h] != st.Chunks[h] {
		t.Fatal("the clone copied a chunk entry")
	}
}

// A snapshot taken at any point holds the state of its index whatever is applied after
// it: random command sequences, a snapshot every few steps, each encoded when taken and
// again after the later applies (I-383, I-104).
// Invariant: I-383, I-104
func TestFSM_SnapshotUnaffectedByLaterCommands(t *testing.T) {
	rng := testseed.Seed(t)
	f := NewFSM()
	gen := &cmdGen{rng: rng, f: f, now: 1000}
	idx := uint64(1)
	f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: MustEncodeCommand(Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})})
	type taken struct {
		st   *State
		enc  []byte
		step int
	}
	var snaps []taken
	for step := range 4000 {
		idx++
		f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: MustEncodeCommand(gen.next())})
		if rng.Intn(25) == 0 {
			sn, err := f.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			st := sn.(*snapshot).st
			snaps = append(snaps, taken{st: st, enc: encodeState(t, st), step: step})
		}
	}
	if len(snaps) < 50 {
		t.Fatalf("only %d snapshots taken", len(snaps))
	}
	for _, s := range snaps {
		if !bytes.Equal(encodeState(t, s.st), s.enc) {
			t.Fatalf("the snapshot taken at step %d changed under later applies", s.step)
		}
	}
}
