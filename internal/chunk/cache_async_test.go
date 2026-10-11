package chunk

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/sysfs/sysfstest"
)

// eventually polls cond on the wall clock (the package cannot import testharness).
func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// A Put returns once the chunk is in memory and accounted; the disk file follows in the
// background, and a Get meanwhile is served from memory without waiting for the write.
// Invariant: I-204
func TestCache_PutMemoryFirst_DiskLater(t *testing.T) {
	root := t.TempDir()
	ff := sysfstest.New(sysfs.OS{})
	entered, release := ff.BlockAt(sysfstest.StepCreateTemp)
	c, err := OpenCache(ff, root, 1<<20, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := payload(1000, 1)
	h := sha256.Sum256(p)
	c.Put(h, p) // must not block on the disk write
	<-entered   // the writer is inside CreateTemp: nothing is on disk yet
	if !c.Has(h) {
		t.Fatal("Has is false while the write is pending")
	}
	if got, ok := c.Get(h); !ok || !bytes.Equal(got, p) {
		t.Fatal("Get missed while the write is pending")
	}
	if c.Used() != 1000 {
		t.Fatalf("Used %d while pending, want 1000", c.Used())
	}
	if files, _ := filepath.Glob(filepath.Join(root, "cache", "*", "*")); len(files) != 0 {
		t.Fatalf("a file exists before the write: %v", files)
	}
	if c.Writing() != 1 {
		t.Fatalf("Writing %d, want 1", c.Writing())
	}
	release()
	c.Drain()
	if files, _ := filepath.Glob(filepath.Join(root, "cache", "*", "*")); len(files) != 1 {
		t.Fatalf("files after the write: %v", files)
	}
	if c.Writing() != 0 {
		t.Fatalf("Writing %d after Drain", c.Writing())
	}
	// the written file is the real entry: a reopen finds it
	c.Close()
	c2, err := OpenCache(sysfs.OS{}, root, 1<<20, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got, ok := c2.Get(h); !ok || !bytes.Equal(got, p) {
		t.Fatal("entry lost across reopen")
	}
}

// A Get of a pending entry the memory tier no longer holds waits for the write instead of
// missing: the bytes are on their way to disk and a miss would refetch them.
// Invariant: I-204
func TestCache_Get_PendingNotInMemory_WaitsForWrite(t *testing.T) {
	root := t.TempDir()
	ff := sysfstest.New(sysfs.OS{})
	entered, release := ff.BlockAt(sysfstest.StepCreateTemp)
	c, err := OpenCache(ff, root, 1<<20, clock.Real(), WithMemory(0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := payload(1000, 2)
	h := sha256.Sum256(p)
	c.Put(h, p)
	<-entered
	got := make(chan bool, 1)
	go func() {
		b, ok := c.Get(h)
		got <- ok && bytes.Equal(b, p)
	}()
	select {
	case <-got:
		t.Fatal("Get returned before the write finished")
	default:
	}
	release()
	if !<-got {
		t.Fatal("Get missed after the write finished")
	}
}

// An entry evicted while its write is pending leaves no file behind: the writer notices
// the eviction and removes what it wrote, so the directory never holds an untracked file.
// Invariant: I-204
func TestCache_PendingEntryEvictedBeforeWrite_NoFile(t *testing.T) {
	root := t.TempDir()
	ff := sysfstest.New(sysfs.OS{})
	entered, release := ff.BlockAt(sysfstest.StepCreateTemp)
	fc := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c, err := OpenCache(ff, root, 2500, fc)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var hs [][32]byte
	for i := range 3 { // 3000 > 2500: the oldest is evicted while every write is still blocked
		p := payload(1000, int64(i))
		hs = append(hs, sha256.Sum256(p))
		c.Put(hs[i], p)
		fc.Advance(time.Second)
	}
	<-entered
	if c.Has(hs[0]) {
		t.Fatal("entry 0 survived the eviction")
	}
	if c.Used() != 2000 {
		t.Fatalf("Used %d, want 2000", c.Used())
	}
	release()
	c.Drain()
	if _, err := os.Stat(c.path(hs[0])); !os.IsNotExist(err) {
		t.Fatalf("the evicted entry's file exists: %v", err)
	}
	for _, h := range hs[1:] {
		if _, err := os.Stat(c.path(h)); err != nil {
			t.Fatalf("a live entry has no file: %v", err)
		}
	}
	c.Close()
	c2, err := OpenCache(sysfs.OS{}, root, 2500, fc)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if c2.Used() != 2000 {
		t.Fatalf("Used %d after reopen, want 2000", c2.Used())
	}
}

// At most CacheQueue writes are outstanding: a Put that finds the queue full writes
// synchronously instead of queueing a further buffer, so the bytes held for pending
// writes never exceed CacheQueue chunks ({0, 1, max−1, max, max+1} Puts).
// Invariant: I-204
func TestCache_FullQueueWritesSynchronously(t *testing.T) {
	for _, n := range []int{0, 1, CacheQueue - 1, CacheQueue, CacheQueue + 1} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			ff := sysfstest.New(sysfs.OS{})
			entered, release := ff.BlockAt(sysfstest.StepCreateTemp)
			c, err := OpenCache(ff, t.TempDir(), 1<<30, clock.Real())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			var returned atomic.Int32
			for i := range n {
				p := payload(1000, int64(100+i))
				go func() {
					c.Put(sha256.Sum256(p), p)
					returned.Add(1)
				}()
			}
			async := min(n, CacheQueue)
			eventually(t, func() bool { return int(returned.Load()) == async }, "the queued Puts return")
			if w := c.Writing(); w != async {
				t.Fatalf("Writing %d, want %d", w, async)
			}
			if n > CacheQueue {
				// the extra Put is inside its own CreateTemp: a third call entered the
				// blocked step (two writers plus the caller) and it has not returned
				for range min(n, CacheWriters+1) {
					<-entered
				}
				if int(returned.Load()) != async {
					t.Fatalf("%d Puts returned, the synchronous one should still be writing", returned.Load())
				}
			}
			release()
			eventually(t, func() bool { return int(returned.Load()) == n }, "every Put returns")
			c.Drain()
			if c.Used() != int64(n*1000) {
				t.Fatalf("Used %d, want %d", c.Used(), n*1000)
			}
		})
	}
}
