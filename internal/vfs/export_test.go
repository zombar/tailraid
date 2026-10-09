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
