package vfs

import "github.com/zombar/tailraid/internal/meta"

// Test-only views of the flusher's retry bookkeeping.

// BackoffEntries is the number of inodes with a pending retry delay.
func (f *Flusher) BackoffEntries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.backoff)
}

// ScheduleRetry is scheduleRetry, for a test that wants an entry to exist.
func (f *Flusher) ScheduleRetry(ino meta.Ino) { f.scheduleRetry(ino) }

// Touched is touched: the chunk indices a plan commits, for the boundary tests.
func (f *Flusher) Touched(plan *FlushPlan, base meta.Inode) []uint64 {
	idxs, _ := f.touched(plan, base, false)
	return idxs
}

// MinSize returns the truncation point when a truncate is pending (tests only).
func (o *Overlay) MinSize(ino meta.Ino) (uint64, bool) {
	d, _ := o.get(ino, 0, false)
	if d == nil {
		return 0, false
	}
	defer d.mu.Unlock()
	return d.minSize, true
}

// PrefetchHeld is the prefetch calls the readahead table still references, finished or
// not (I-310).
func (r *Reader) PrefetchHeld() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, calls := range r.pf {
		n += len(calls)
	}
	return n
}
