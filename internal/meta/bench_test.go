package meta

import (
	"fmt"
	"testing"

	"github.com/hashicorp/raft"
)

// bfsm applies commands to a fresh FSM without the per-apply invariant check (benchmarks
// only: the check walks the whole state).
type bfsm struct {
	f   *FSM
	idx uint64
}

func newBFSM(b *testing.B) *bfsm {
	b.Helper()
	bf := &bfsm{f: NewFSM()}
	bf.apply(b, Command{Op: OpSetVolume, SetVolume: &SetVolume{ChunkSize: testCS, K: 2, M: 1, Now: 1}})
	for i := 1; i <= 3; i++ {
		bf.apply(b, Command{Op: OpNodeUpsert, NodeUpsert: &NodeUpsert{Node: Node{ID: fmt.Sprintf("n%d", i), Name: fmt.Sprintf("n%d", i), Kind: KindDevice, State: Active, Capacity: 1 << 40}}})
	}
	return bf
}

func (bf *bfsm) apply(b *testing.B, c Command) Result {
	b.Helper()
	bf.idx++
	res := bf.f.Apply(&raft.Log{Index: bf.idx, Type: raft.LogCommand, Data: EncodeCommand(c)}).(Result)
	if res.Err != nil {
		b.Fatalf("op %d: %v", c.Op, res.Err)
	}
	return res
}

// A file of 16k chunks (16 GiB at 1 MiB) gets one chunk rewritten per commit: the cost of
// Commit must not scale with the file length (P3).
func BenchmarkFSM_Commit_LargeFile(b *testing.B) {
	bf := newBFSM(b)
	const n = 1 << 14
	ino := bf.apply(b, Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: "big", Type: TFile, Mode: 0o644, Now: 1}}).Ino
	writes := make([]ChunkWrite, n)
	news := make([]ChunkDesc, n)
	for i := range writes {
		h := hashN(uint64(i) + 1) // hashN(0) is HashZero
		writes[i] = ChunkWrite{Idx: uint32(i), Hash: h}
		news[i] = ChunkDesc{Hash: h, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}}
	}
	bf.apply(b, Command{Op: OpCommit, Commit: &Commit{Ino: ino, BaseVersion: 0, NewSize: n * testCS, Writes: writes, NewChunks: news, Now: 2}})
	version := uint64(1)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h := hashN(uint64(n + i + 1))
		idx := uint32(i % n)
		c := Command{Op: OpCommit, Commit: &Commit{Ino: ino, BaseVersion: version, NewSize: n * testCS,
			Writes: []ChunkWrite{{Idx: idx, Hash: h}}, NewChunks: []ChunkDesc{{Hash: h, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}}}, Now: 3}}
		bf.apply(b, c)
		version++
	}
}

func bigDir(b *testing.B, bf *bfsm, n int) Ino {
	b.Helper()
	dir := bf.apply(b, Command{Op: OpMkdir, Mkdir: &Mkdir{Parent: RootIno, Name: "d", Mode: 0o755, Now: 1}}).Ino
	for i := 0; i < n; i++ {
		bf.apply(b, Command{Op: OpCreate, Create: &Create{Parent: dir, Name: fmt.Sprintf("f%06d", i), Type: TFile, Mode: 0o644, Now: 1}})
	}
	return dir
}

// LOOKUP of one name in a 10k-entry directory must not copy the directory (P4).
func BenchmarkLookup_LargeDir(b *testing.B) {
	bf := newBFSM(b)
	dir := bigDir(b, bf, 10_000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := bf.f.Lookup(dir, fmt.Sprintf("f%06d", i%10_000)); !ok {
			b.Fatal("missing")
		}
	}
}

// GETATTR of the directory itself: the same copy on every stat.
func BenchmarkGetInode_LargeDir(b *testing.B) {
	bf := newBFSM(b)
	dir := bigDir(b, bf, 10_000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := bf.f.GetInode(dir); !ok {
			b.Fatal("missing")
		}
	}
}

// One READDIR of a 10k-entry directory (every page re-lists it, P7).
func BenchmarkReadDir_10k(b *testing.B) {
	bf := newBFSM(b)
	dir := bigDir(b, bf, 10_000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ents, err := bf.f.ReadDir(dir)
		if err != nil || len(ents) != 10_000 {
			b.Fatal(err)
		}
	}
}

// FSSTAT walks every inode and chunk (P8): 10k files of 10 chunks each.
func BenchmarkFSSTAT(b *testing.B) {
	bf := newBFSM(b)
	for i := 0; i < 10_000; i++ {
		ino := bf.apply(b, Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: fmt.Sprintf("f%06d", i), Type: TFile, Mode: 0o644, Now: 1}}).Ino
		writes := make([]ChunkWrite, 10)
		news := make([]ChunkDesc, 10)
		for j := range writes {
			h := hashN(uint64(i*10 + j + 1))
			writes[j] = ChunkWrite{Idx: uint32(j), Hash: h}
			news[j] = ChunkDesc{Hash: h, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}}
		}
		bf.apply(b, Command{Op: OpCommit, Commit: &Commit{Ino: ino, NewSize: 10 * testCS, Writes: writes, NewChunks: news, Now: 2}})
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if s := bf.f.Stats(); s.Chunks != 100_000 {
			b.Fatalf("stats %+v", s)
		}
	}
}

// A full chunk walk (repair scan, check, GC victims) over 100k chunks (P9).
func BenchmarkForEachChunk_100k(b *testing.B) {
	bf := newBFSM(b)
	for i := 0; i < 100; i++ {
		ino := bf.apply(b, Command{Op: OpCreate, Create: &Create{Parent: RootIno, Name: fmt.Sprintf("f%06d", i), Type: TFile, Mode: 0o644, Now: 1}}).Ino
		writes := make([]ChunkWrite, 1000)
		news := make([]ChunkDesc, 1000)
		for j := range writes {
			h := hashN(uint64(i*1000 + j + 1))
			writes[j] = ChunkWrite{Idx: uint32(j), Hash: h}
			news[j] = ChunkDesc{Hash: h, Size: testCS, K: 2, M: 1, Placement: []NodeIdx{1, 2, 3}}
		}
		bf.apply(b, Command{Op: OpCommit, Commit: &Commit{Ino: ino, NewSize: 1000 * testCS, Writes: writes, NewChunks: news, Now: 2}})
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		n := 0
		bf.f.ForEachChunk(func(Chunk) bool { n++; return true })
		if n != 100_000 {
			b.Fatalf("%d chunks", n)
		}
	}
}
