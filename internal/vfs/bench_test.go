package vfs_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// A full flush of 16 entirely dirty chunks (1 MiB at the test chunk size): assemble,
// hash, encode, PUT to the in-process stores, commit (P1, P10, P12, P13).
func BenchmarkFlush_FullChunks(b *testing.B) {
	f := testharness.NewFabric(b, 3, 2, 1)
	cs := int(testharness.ChunkSize)
	fl := f.Flusher()
	data := make([]byte, 16*cs)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		rand.New(rand.NewSource(int64(i))).Read(data) //nolint:gosec // distinct bytes per file, so nothing dedups
		ino := f.Create(fmt.Sprintf("f%d", i))
		b.StartTimer()
		if err := f.Ov.Write(ino, 0, 0, data); err != nil {
			b.Fatal(err)
		}
		if err := fl.FlushInode(context.Background(), ino); err != nil {
			b.Fatal(err)
		}
	}
}

func flushedFile(b *testing.B, f *testharness.Fabric, chunks int) (meta.Inode, []byte) {
	b.Helper()
	cs := int(testharness.ChunkSize)
	data := make([]byte, chunks*cs)
	rand.New(rand.NewSource(1)).Read(data) //nolint:gosec // workload
	ino := f.Create("r")
	if err := f.Ov.Write(ino, 0, 0, data); err != nil {
		b.Fatal(err)
	}
	if err := f.Flusher().FlushInode(context.Background(), ino); err != nil {
		b.Fatal(err)
	}
	return f.Inode(ino), data
}

// Sequential 64 KiB reads of a committed 1 MiB file through the chunk cache (P5).
func BenchmarkRead_Cached(b *testing.B) {
	f := testharness.NewFabric(b, 3, 2, 1)
	in, data := flushedFile(b, f, 16)
	rd := f.Reader()
	buf := make([]byte, 64<<10)
	ctx := context.Background()
	if _, err := rd.ReadAt(ctx, in, buf, 0); err != nil { // warm the cache
		b.Fatal(err)
	}
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		off := int64((i * len(buf)) % len(data))
		if _, err := rd.ReadAt(ctx, in, buf, off); err != nil {
			b.Fatal(err)
		}
	}
}

// The same reads with no cache: every read fetches k shards and decodes.
func BenchmarkRead_Uncached(b *testing.B) {
	f := testharness.NewFabric(b, 3, 2, 1)
	in, data := flushedFile(b, f, 16)
	rd := f.ReaderNoCache()
	buf := make([]byte, 64<<10)
	ctx := context.Background()
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		off := int64((i * len(buf)) % len(data))
		if _, err := rd.ReadAt(ctx, in, buf, off); err != nil {
			b.Fatal(err)
		}
	}
}

// READDIR of a 10k-entry directory through the billy filesystem (what READDIRPLUS
// calls once per page, P7).
func BenchmarkReadDirPlus_10k(b *testing.B) {
	f := testharness.NewFabric(b, 3, 2, 1)
	dir := f.MustApply(meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: "d", Mode: 0o755, Now: 1}}).Ino
	for i := 0; i < 10_000; i++ {
		f.MustApply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: dir, Name: fmt.Sprintf("f%06d", i), Type: meta.TFile, Mode: 0o644, Now: 1}})
	}
	fs := f.FS()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ents, err := fs.ReadDir("/d")
		if err != nil || len(ents) != 10_000 {
			b.Fatal(err)
		}
	}
}
