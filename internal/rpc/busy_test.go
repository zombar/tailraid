package rpc

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// A node whose shard slots are all taken answers 503 busy once a request has queued for
// SlotWait, well inside the caller's header timeout: the caller sees ErrBusy (an
// ErrUnavailable, never ErrNoLeader) and tries another holder, and the node is not
// marked suspect, as it was when the request queued until the caller's own deadline
// fired at the transport level. A slot freed within the wait serves the request.
// Invariant: I-246
func TestServer_BusySlots_AnswerBusy_NotSuspect(t *testing.T) {
	p := bytes.Repeat([]byte{9}, 100)
	cases := []struct {
		name string
		sem  func(s *Server) chan struct{}
		call func(c *Client) error
	}{
		{"GET", func(s *Server) chan struct{} { return s.getSem }, func(c *Client) error { _, err := c.Get(ctx, 1, key(p, 0)); return err }},
		{"background GET", func(s *Server) chan struct{} { return s.bgGetSem }, func(c *Client) error { _, err := c.Get(chunk.Background(ctx), 1, key(p, 0)); return err }},
		{"PUT", func(s *Server) chan struct{} { return s.putSem }, func(c *Client) error { return c.Put(ctx, 1, key(p, 0), p) }},
		{"background PUT", func(s *Server) chan struct{} { return s.bgPutSem }, func(c *Client) error { return c.Put(chunk.Background(ctx), 1, key(p, 0), p) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvWith(t, func(o *Options) { o.PutSlots, o.GetSlots = 1, 1 })
			c := e.client()
			var unreachable atomic.Int32
			c.OnUnreachable = func(meta.NodeIdx) { unreachable.Add(1) }
			sem := tc.sem(e.srv)
			sem <- struct{}{} // every slot of this kind is taken
			done := make(chan error, 1)
			go func() { done <- tc.call(c) }()
			var err error
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
		wait:
			for deadline := time.After(10 * time.Second); ; {
				select {
				case err = <-done:
					break wait
				case <-tick.C:
					e.clk.Advance(SlotWait / 4) // the wait runs on the server's clock
				case <-deadline:
					t.Fatal("the request queued past SlotWait was never answered")
				}
			}
			if !errors.Is(err, ErrBusy) || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNoLeader) {
				t.Fatalf("a full node answered %v, want ErrBusy (an ErrUnavailable)", err)
			}
			if n := unreachable.Load(); n != 0 {
				t.Fatalf("a busy node was reported unreachable %d times", n)
			}
			// a slot freed while the request waits serves it
			go func() { done <- tc.call(c) }()
			<-sem
			if err := <-done; err != nil && !errors.Is(err, chunk.ErrNotFound) {
				t.Fatalf("with the slot freed: %v", err)
			}
		})
	}
}
