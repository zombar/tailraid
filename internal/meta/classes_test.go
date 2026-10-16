package meta

import (
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testseed"
)

// Live chunks group by policy and slot set whatever the slot order; a placement change
// moves a chunk between classes, a tombstone takes it out and a re-reference puts it
// back, and a small k = 1 chunk is its own class (it follows Vol.M only).
// Invariant: I-377
func TestClasses_GroupBySlotSet(t *testing.T) {
	tf := newTFSM(t)
	a, b, c := hashN(1), hashN(2), hashN(3)
	ino := tf.create(RootIno, "f")
	tf.commit(ino, 0, 3*testCS, []ChunkWrite{{Idx: 0, Hash: a}, {Idx: 1, Hash: b}, {Idx: 2, Hash: c}}, []ChunkDesc{
		{Hash: a, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}},
		{Hash: b, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{3, 1, 2}},
		{Hash: c, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 4}},
	})
	classes := tf.f.Classes()
	if len(classes) != 2 {
		t.Fatalf("classes %+v, want {1,2,3}×2 and {1,2,4}×1", classes)
	}
	byPlace := map[string]Class{}
	for _, cl := range classes {
		byPlace[fmtSlots(cl.Placement)] = cl
	}
	if cl := byPlace["1,2,3"]; cl.Chunks != 2 || len(tf.f.ClassMembers(cl.Key)) != 2 {
		t.Fatalf("{1,2,3}: %+v", cl)
	}
	// a placement change moves b to {1,2,4}
	tf.ok(Command{Op: OpSetPlacement, SetPlacement: &SetPlacement{Hash: b, K: 2, M: 1, Placement: []NodeIdx{4, 2, 1}}})
	for _, cl := range tf.f.Classes() {
		if want := map[string]int{"1,2,3": 1, "1,2,4": 2}[fmtSlots(cl.Placement)]; cl.Chunks != want {
			t.Fatalf("after re-place: %+v", tf.f.Classes())
		}
	}
	// the file truncated to nothing tombstones every chunk: no class is left
	if r := tf.apply(Command{Op: OpCommit, Commit: &Commit{Ino: ino, BaseVersion: 1, HasTruncate: true, TruncateTo: 0, NewSize: 0, Now: 100}}); r.Err != nil {
		t.Fatal(r.Err)
	}
	if got := tf.f.Classes(); len(got) != 0 {
		t.Fatalf("tombstoned chunks in classes: %+v", got)
	}
	// a re-reference brings the chunk back into its class
	if r := tf.commit(ino, 2, testCS, []ChunkWrite{{Idx: 0, Hash: a}}, nil); r.Err != nil {
		t.Fatal(r.Err)
	}
	if got := tf.f.Classes(); len(got) != 1 || got[0].Chunks != 1 || !slices.Equal(tf.f.ClassMembers(got[0].Key), []Hash{a}) {
		t.Fatalf("re-referenced: %+v", got)
	}
	// a small k = 1 chunk on the same slots is a class of its own
	small := hashN(9)
	if r := tf.commit(ino, 3, testCS+100, []ChunkWrite{{Idx: 1, Hash: small}}, []ChunkDesc{{Hash: small, Size: 100, K: 1, M: 2, Placement: []NodeIdx{1, 2, 3}}}); r.Err != nil {
		t.Fatal(r.Err)
	}
	var smalls int
	for _, cl := range tf.f.Classes() {
		if cl.Small {
			smalls++
			if r := cl.Representative(tf.f.Volume()); !IsSmallChunk(r.Size, tf.f.Volume().ChunkSize) {
				t.Fatalf("small class representative has size %d", r.Size)
			}
		}
	}
	if smalls != 1 {
		t.Fatalf("small classes %d, want 1: %+v", smalls, tf.f.Classes())
	}
}

func fmtSlots(p []NodeIdx) string {
	s := ""
	for i, n := range p {
		if i > 0 {
			s += ","
		}
		s += string(rune('0' + n))
	}
	return s
}

type mapLive struct {
	live map[NodeIdx]bool
	dead map[NodeIdx]time.Duration
}

func (m mapLive) IsLive(n NodeIdx) bool           { return m.live[n] }
func (m mapLive) DeadFor(n NodeIdx) time.Duration { return m.dead[n] }

// A class's representative derives the same counts as every chunk in it, for random
// placements (missing slots, two disks of one host, draining and removed nodes, dead
// holders inside and past fill_after) and policies: what lets `status` judge a class
// once (I-378).
// Invariant: I-378
func TestClasses_RepresentativeMatchesEveryMember(t *testing.T) {
	rng := testseed.Seed(t)
	vol := Volume{ChunkSize: testCS, K: 2, M: 1}
	for round := range 300 {
		nodes, live := randomTable(rng)
		in := HealthInput{Nodes: nodes, Live: live, FillAfter: 10 * time.Minute}
		k, m := uint8(1+rng.Intn(3)), uint8(rng.Intn(3))
		place := make([]NodeIdx, int(k)+int(m))
		for i := range place {
			place[i] = NodeIdx(rng.Intn(9)) // 0 is a missing slot
		}
		size := uint32(testCS)
		if rng.Intn(4) == 0 {
			size = 100
		}
		c := Chunk{Hash: hashN(uint64(round) + 1), Size: size, K: k, M: m, Refs: 1, Placement: slices.Clone(place)}
		rng.Shuffle(len(place), func(i, j int) { place[i], place[j] = place[j], place[i] })
		key, sorted, small := classOf(&c, vol.ChunkSize)
		rep := Class{Key: key, K: k, M: m, Small: small, Placement: sorted}.Representative(vol)
		got, want := ChunkState(rep, vol, in), ChunkState(c, vol, in)
		if got.Readable != want.Readable || len(got.Bad) != len(want.Bad) || got.PolicyStale != want.PolicyStale || got.Skewed != want.Skewed ||
			got.UnderReplicated(k, m) != want.UnderReplicated(k, m) || got.Unreadable(k) != want.Unreadable(k) {
			t.Fatalf("round %d: placement %v: representative %+v, member %+v", round, c.Placement, got, want)
		}
	}
}

// randomTable is three hosts with two disks each and a diskless device, in random states
// and liveness.
func randomTable(rng *rand.Rand) (map[NodeIdx]Node, mapLive) {
	nodes := map[NodeIdx]Node{}
	live := mapLive{live: map[NodeIdx]bool{}, dead: map[NodeIdx]time.Duration{}}
	states := []NodeState{Active, Active, Active, Draining, Removed}
	for h := NodeIdx(1); h <= 3; h++ {
		nodes[h] = Node{Idx: h, Kind: KindDevice, State: states[rng.Intn(len(states))], Capacity: 1 << 40}
	}
	for d := NodeIdx(4); d <= 8; d++ {
		nodes[d] = Node{Idx: d, Kind: KindDisk, Host: 1 + (d-4)%3, State: states[rng.Intn(len(states))], Capacity: 1 << 40}
	}
	for n := range nodes {
		live.live[n] = rng.Intn(4) != 0
		if !live.live[n] {
			live.dead[n] = time.Duration(rng.Intn(20)) * time.Minute
		}
	}
	return nodes, live
}
