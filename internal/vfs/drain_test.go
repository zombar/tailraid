package vfs

import (
	"testing"
	"time"
)

// The drain rate is the committed bytes a second over the last minute: 0 until two
// samples exist, samples at most every 2 s, and older ones age out of the window.
// Invariant: I-398
func TestFlusher_DrainRate(t *testing.T) {
	f := &Flusher{}
	t0 := time.Unix(1_700_000_000, 0)
	if r := f.DrainRate(); r != 0 {
		t.Fatalf("rate with no samples: %d", r)
	}
	f.sampleDrain(t0)
	if r := f.DrainRate(); r != 0 {
		t.Fatalf("rate with one sample: %d", r)
	}
	f.flushed.Add(10 << 20)
	f.sampleDrain(t0.Add(time.Second)) // within 2 s of the last: not taken
	if r := f.DrainRate(); r != 0 {
		t.Fatalf("a sample within 2 s was taken: rate %d", r)
	}
	f.sampleDrain(t0.Add(10 * time.Second))
	if r := f.DrainRate(); r != 1<<20 {
		t.Fatalf("10 MiB in 10 s: rate %d, want %d", r, 1<<20)
	}
	// an idle minute later the window holds only the idle samples
	for s := 12; s <= 80; s += 2 {
		f.sampleDrain(t0.Add(time.Duration(s) * time.Second))
	}
	if r := f.DrainRate(); r != 0 {
		t.Fatalf("after an idle minute: rate %d, want 0", r)
	}
	if f.Flushed() != 10<<20 {
		t.Fatalf("Flushed %d, want the cumulative 10 MiB", f.Flushed())
	}
}
