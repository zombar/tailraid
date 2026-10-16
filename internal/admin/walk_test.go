package admin_test

import (
	"encoding/binary"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// countingLive answers from a fixed view and counts every question by node.
type countingLive struct {
	mu   sync.Mutex
	dead map[meta.NodeIdx]bool
	asks map[meta.NodeIdx]int
}

func (c *countingLive) IsLive(idx meta.NodeIdx) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks[idx]++
	return !c.dead[idx]
}

func (c *countingLive) DeadFor(idx meta.NodeIdx) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks[idx]++
	if c.dead[idx] {
		return time.Hour
	}
	return 0
}

// One status walk asks the liveness source about each member at most once (IsLive and
// DeadFor each), however many slots it judges, and judges every slot by those answers:
// asking per slot made `walk_ms` grow with the volume, 1.1 s every 2 s at 115k chunks on
// a real node (docs/12 §8).
// Invariant: I-375
func TestScanHealth_AsksEachMemberOnce(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	for i, name := range []string{"a", "b", "c", "d"} {
		ino := f.Create(name)
		must(t, f.Ov.Write(ino, 4096, 4096, testharness.Pattern(2*int(testharness.ChunkSize), byte(i+1))))
		must(t, f.Flusher().FlushInode(ctx, ino))
	}
	live := &countingLive{dead: map[meta.NodeIdx]bool{5: true}, asks: map[meta.NodeIdx]int{}}
	v := admin.ScanHealth(f.FSM, live, 10*time.Minute)
	slots := 0
	for _, n := range v.Slots {
		slots += n
	}
	if slots < 12 {
		t.Fatalf("walk saw %d slots, want at least 12: %+v", slots, v.Slots)
	}
	for idx, n := range live.asks {
		if n > 2 {
			t.Errorf("member %d asked %d times in one walk, want at most 2 (IsLive, DeadFor)", idx, n)
		}
	}
	if v.Missing[5] != v.Slots[5] || v.Slots[5] == 0 {
		t.Fatalf("dead member's slots: missing %d of %d", v.Missing[5], v.Slots[5])
	}
	if v.RepairPending == 0 {
		t.Fatalf("slots on a member dead past fill_after not pending: %+v", v)
	}
}

// Health from the placement classes equals the per-chunk derivation it replaced, with a
// dead holder, a missing slot and slots on every target: status counts the same chunks
// without visiting one (I-378).
// Invariant: I-378
func TestScanHealth_FromClassesMatchesEveryChunk(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		ino := f.Create(name)
		must(t, f.Ov.Write(ino, 4096, 4096, testharness.Pattern(3*int(testharness.ChunkSize), byte(i+1))))
		must(t, f.Flusher().FlushInode(ctx, ino))
	}
	c, _ := f.FSM.GetChunk(f.Inode(f.InodeOf(f.FS(), "c")).Chunks[1])
	place := append([]meta.NodeIdx(nil), c.Placement...)
	place[1] = 0
	f.MustApply(meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, K: c.K, M: c.M, Placement: place}})
	dead := f.Targets()[0]
	live := &countingLive{dead: map[meta.NodeIdx]bool{dead: true}, asks: map[meta.NodeIdx]int{}}
	got := admin.ScanHealth(f.FSM, live, 0) // fill_after 0: the dead holder's slots are pending at once

	want := admin.VolumeHealth{Slots: map[meta.NodeIdx]int{}, Missing: map[meta.NodeIdx]int{}, Bytes: map[meta.NodeIdx]uint64{}}
	nodes := map[meta.NodeIdx]meta.Node{}
	for _, nd := range f.FSM.Nodes() {
		nodes[nd.Idx] = nd
	}
	vol := f.FSM.Volume()
	in := meta.HealthInput{Nodes: nodes, Live: live, FillAfter: 0}
	f.FSM.ForEachChunk(func(c meta.Chunk) bool {
		if c.Refs == 0 {
			return true
		}
		for _, s := range c.Placement {
			if s != 0 {
				want.Slots[s]++
				want.Bytes[s] += uint64(ec.ShardLen(int(c.Size), int(c.K)))
				if dead == s {
					want.Missing[s]++
				}
			}
		}
		h := meta.ChunkState(c, vol, in)
		if h.UnderReplicated(c.K, c.M) {
			want.Degraded++
		}
		if h.RepairPending() {
			want.RepairPending++
		}
		if h.Unreadable(c.K) {
			want.Unreadable++
		}
		if h.PolicyStale {
			want.StalePolicy++
		}
		if h.Skewed {
			want.DomainSkew++
		}
		return true
	})
	if got.Degraded != want.Degraded || got.RepairPending != want.RepairPending || got.Unreadable != want.Unreadable ||
		got.StalePolicy != want.StalePolicy || got.DomainSkew != want.DomainSkew || want.Degraded == 0 || want.Unreadable == 0 {
		t.Fatalf("counts: classes %+v, per chunk %+v", got, want)
	}
	if !maps.Equal(got.Slots, want.Slots) || !maps.Equal(got.Missing, want.Missing) || !maps.Equal(got.Bytes, want.Bytes) {
		t.Fatalf("per node: classes %v %v %v, per chunk %v %v %v", got.Slots, got.Missing, got.Bytes, want.Slots, want.Missing, want.Bytes)
	}
}

// BenchmarkScanHealth_100kChunks is the status walk over 100k chunks on five targets: the
// classes, not the chunks, set its cost (I-378).
func BenchmarkScanHealth_100kChunks(b *testing.B) {
	f := meta.NewFSM()
	idx := uint64(0)
	apply := func(c meta.Command) meta.Result {
		idx++
		res := f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: meta.MustEncodeCommand(c)}).(meta.Result)
		if res.Err != nil {
			b.Fatalf("op %d: %v", c.Op, res.Err)
		}
		return res
	}
	apply(meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{ChunkSize: 1 << 20, K: 3, M: 2, Now: 1}})
	for i := 1; i <= 5; i++ {
		apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: fmt.Sprintf("n%d", i), Name: fmt.Sprintf("n%d", i), Kind: meta.KindDevice, State: meta.Active, Capacity: 1 << 50}}})
	}
	ino := apply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: meta.RootIno, Name: "big", Type: meta.TFile, Mode: 0o644, Now: 1}}).Ino
	const n = 100_000
	writes := make([]meta.ChunkWrite, n)
	news := make([]meta.ChunkDesc, n)
	for i := range writes {
		var h meta.Hash
		binary.BigEndian.PutUint64(h[:8], uint64(i)+1)
		writes[i] = meta.ChunkWrite{Idx: uint32(i), Hash: h}
		perm := [][]meta.NodeIdx{{1, 2, 3, 4, 5}, {2, 3, 4, 5, 1}, {5, 4, 3, 2, 1}}[i%3]
		news[i] = meta.ChunkDesc{Hash: h, Size: 1 << 20, K: 3, M: 2, Placement: perm}
	}
	apply(meta.Command{Op: meta.OpCommit, Commit: &meta.Commit{Ino: ino, NewSize: n << 20, Writes: writes, NewChunks: news, Now: 2}})
	b.ResetTimer()
	for range b.N {
		if h := admin.ScanHealth(f, allLive{}, 10*time.Minute); h.Slots[1] != n {
			b.Fatalf("slots on n1: %d", h.Slots[1])
		}
	}
}
