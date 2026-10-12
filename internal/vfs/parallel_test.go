package vfs

import (
	"context"
	"errors"
	"testing"
)

// parallel that stops before starting every item because its context ended reports that,
// never success: a flush cancelled at shutdown otherwise handed buildCommit a works slice
// with nil entries and crashed the node (found by TestSMB_ConcurrentCopies_EveryByteLands,
// the first test to cancel Flusher.Run while it flushes).
func TestParallel_StoppedByContext_IsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := 0
	err := parallel(ctx, 1, 4, func(int) error {
		ran++
		return nil
	})
	if ran < 4 && !errors.Is(err, context.Canceled) {
		t.Fatalf("ran %d of 4 items and returned %v, want context.Canceled", ran, err)
	}
}
