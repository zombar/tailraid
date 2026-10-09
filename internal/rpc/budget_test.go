package rpc

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
)

// A shard call's deadline grows with its size: the fixed allowance alone for an empty
// body, and the transfer time at the slowest tailnet path on top, at the payload bounds.
// Invariant: I-83
func TestClient_PutTimeout_Table(t *testing.T) {
	const maxN = int64(chunk.MaxPayload)
	for _, c := range []struct {
		n    int64
		want time.Duration
	}{
		{0, 10 * time.Second},
		{1, 10*time.Second + time.Second/transferRate},
		{maxN - 1, 10*time.Second + time.Duration(maxN-1)*time.Second/transferRate},
		{maxN, 10*time.Second + time.Duration(maxN)*time.Second/transferRate},
		{maxN + 1, 10*time.Second + time.Duration(maxN+1)*time.Second/transferRate},
	} {
		if got := callTimeout(c.n); got != c.want {
			t.Fatalf("callTimeout(%d) = %v, want %v", c.n, got, c.want)
		}
	}
	if got := callTimeout(maxN); got < 4*time.Minute {
		t.Fatalf("a %d-byte shard gets only %v", maxN, got)
	}
}

// The server's concurrent PUT and GET slots keep slots × MaxShard within the budget,
// never fewer than 4 nor more than 32.
// Invariant: I-83
func TestNode_SlotBudget_Table(t *testing.T) {
	for _, c := range []struct {
		maxShard int64
		want     int
	}{{0, 32}, {64 << 10, 32}, {1<<20 + 4096, 32}, {8 << 20, 32}, {16 << 20, 16}, {64<<20 + 4096, 4}, {1 << 40, 4}} {
		got := Slots(c.maxShard)
		if got != c.want {
			t.Fatalf("Slots(%d) = %d, want %d", c.maxShard, got, c.want)
		}
		if c.maxShard > 0 && got > 4 && int64(got)*c.maxShard > SlotBudget {
			t.Fatalf("Slots(%d) = %d exceeds the budget", c.maxShard, got)
		}
	}
}

// stallTransport holds every request until its context ends.
type stallTransport struct{ entered chan struct{} }

func (s *stallTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// A PUT whose caller cancels returns at once with the cancellation, well inside its
// deadline, and leaves no shard behind.
// Invariant: I-83
func TestChunkRPC_PutCancelled_NoShardLeft(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	st := &stallTransport{entered: make(chan struct{}, 1)}
	c.SetTransport(st)
	p := bytes.Repeat([]byte{9}, 1<<12)
	k := key(p, 0)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Put(cctx, 1, k, p) }()
	<-st.entered
	cancel()
	err := <-done
	if !errors.Is(err, ErrUnavailable) || !errors.Is(cctx.Err(), context.Canceled) {
		t.Fatalf("cancelled put: %v", err)
	}
	c.SetTransport(http.DefaultTransport)
	if ok, err := c.Has(ctx, 1, k); err != nil || ok {
		t.Fatalf("shard after a cancelled put: present=%v err=%v", ok, err)
	}
}
