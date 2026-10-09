package vfs

import (
	"math/rand"
	"testing"
)

// Random 4 KiB writes into a 64 MiB range: the per-WRITE cost of the extent map (P11).
func BenchmarkExtents_Insert(b *testing.B) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // workload
	var e extents
	b.ReportAllocs()
	for i := range b.N {
		off := uint64(rng.Intn(16384)) * 4096
		e.insert(extent{off: off, n: 4096, walOff: int64(i) * 4096})
		if len(e) > 8192 { // keep the map at a realistic size
			e = e[:0]
		}
	}
}

// coversChunk per chunk of a 64 MiB file with 4096 scattered extents.
func BenchmarkExtents_CoversChunk(b *testing.B) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // workload
	var e extents
	for range 4096 {
		e.insert(extent{off: uint64(rng.Intn(16384)) * 4096, n: 4096})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		e.coversChunk(uint64(i%64), 1<<20, 64<<20)
	}
}
