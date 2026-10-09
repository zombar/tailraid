package cluster_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
)

// Proposals in flight while the leader closes all resolve: each returns a result or an
// error within seconds, and no waiter is left behind on a Raft future that will never be
// answered (raft's FSM loop drops its queue on shutdown; Close drains it first). The
// goroutines this test starts are all gone afterwards, which is what goleak checks here
// and at the package's end.
//
// Invariant: I-95
func TestCluster_CloseWhileApplying_EveryWaiterResolves(t *testing.T) {
	c := testharness.NewCluster(t, 3, 2, 1)
	defer c.Close()
	leader := c.Leader()
	c.Settle()
	before := goleak.IgnoreCurrent() // the survivors' new leader will start replication goroutines of its own: only a stuck apply waiter counts
	const n = 200
	var wg sync.WaitGroup
	results := make(chan error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: fmt.Sprintf("d%03d", i), Mode: 0o755, Now: int64(i + 1)}})
			results <- err
		})
	}
	close(start)
	c.Kill(leader.I) // Close runs while the proposals are being enqueued, dispatched and applied
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a proposal never returned after the leader closed")
	}
	close(results)
	for err := range results {
		if err != nil && !isRetryable(err) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	// every goroutine the proposals and the closed node used is gone
	var last error
	deadline := time.Now().Add(15 * time.Second)
	for {
		last = goleak.Find(before)
		if last == nil || !strings.Contains(last.Error(), "applyLocal") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("an apply waiter outlives the closed leader:\n%v", last)
		}
		<-time.After(100 * time.Millisecond)
	}
}

// isRetryable: the outcomes a closing leader may report (docs/02 §forwarding).
func isRetryable(err error) bool {
	for _, s := range []string{"not leader", "no leader", "unknown outcome", "closing", "shutdown", "leadership lost", "deadline"} {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}
