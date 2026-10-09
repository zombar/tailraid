package chunk

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/sysfs"
)

// One 1 MiB shard PUT on the real filesystem: hash, temp file, two fsyncs, rename (P10,
// P15). Distinct keys, so nothing short-circuits.
func BenchmarkStore_Put(b *testing.B) {
	st, err := Open(sysfs.OS{}, b.TempDir(), 0)
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(payload) //nolint:gosec // workload
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		if _, err := st.Put(Key(sha256.Sum256(buf[:]), 2, 1, 0), payload); err != nil {
			b.Fatal(err)
		}
	}
}

// A cache hit for a 1 MiB chunk: today a disk read plus a hash (P5).
func BenchmarkCache_Get(b *testing.B) {
	c, err := OpenCache(sysfs.OS{}, b.TempDir(), 1<<30, clock.NewFake(time.Unix(1_700_000_000, 0)))
	if err != nil {
		b.Fatal(err)
	}
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(data) //nolint:gosec // workload
	h := sha256.Sum256(data)
	c.Put(h, data)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		if _, ok := c.Get(h); !ok {
			b.Fatal("miss")
		}
	}
}
