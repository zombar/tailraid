package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
)

// clientFor is a client routing target 1 (disk d0) to addr over HTTP, recording the
// nodes it reports unreachable.
func clientFor(addr string) (*Client, *[]meta.NodeIdx, *sync.Mutex) {
	c := NewClient([16]byte{1}, func(idx meta.NodeIdx) (string, bool, bool) {
		if idx == 1 {
			return addr, false, true
		}
		return "", false, false
	})
	c.SetDisks(disk0, nil)
	var mu sync.Mutex
	marked := &[]meta.NodeIdx{}
	c.OnUnreachable = func(n meta.NodeIdx) {
		mu.Lock()
		*marked = append(*marked, n)
		mu.Unlock()
	}
	return c, marked, &mu
}

// closedPort is an address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// Only a transport-level failure marks a node unreachable: a refused dial and a header
// timeout do; any HTTP status reply and a request the caller cancelled do not.
// Invariant: I-170
func TestClient_TransportFailureMarksSuspect_Table(t *testing.T) {
	k := chunk.Key([32]byte{7}, 2, 1, 0)
	status := func(code int) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(s.Close)
		return s
	}
	// the handler drains the body first: a server only notices a client leaving once the
	// request is read; stop releases whatever is still blocked before the server closes
	stop := make(chan struct{})
	blocking := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(blocking.Close)
	t.Cleanup(func() { close(stop) })
	for _, tc := range []struct {
		name   string
		addr   string
		call   func(c *Client, ctx context.Context) error
		ctx    func() (context.Context, context.CancelFunc)
		marked bool
	}{
		{"dial refused", closedPort(t), func(c *Client, ctx context.Context) error { _, err := c.Get(ctx, 1, k); return err }, nil, true},
		{"404", status(404).Listener.Addr().String(), func(c *Client, ctx context.Context) error { _, err := c.Get(ctx, 1, k); return err }, nil, false},
		{"507", status(507).Listener.Addr().String(), func(c *Client, ctx context.Context) error { return c.Put(ctx, 1, k, []byte("x")) }, nil, false},
		{"503", status(503).Listener.Addr().String(), func(c *Client, ctx context.Context) error { _, err := c.Has(ctx, 1, k); return err }, nil, false},
		{
			"caller cancel", blocking.Listener.Addr().String(), func(c *Client, ctx context.Context) error { _, err := c.Get(ctx, 1, k); return err },
			func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			}, false,
		},
		{"header timeout", blocking.Listener.Addr().String(), func(c *Client, ctx context.Context) error { return c.Put(ctx, 1, k, []byte("x")) }, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, marked, mu := clientFor(tc.addr)
			if tc.name == "header timeout" {
				c.SetTransport(&http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond})
			}
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			if err := tc.call(c, ctx); err == nil {
				t.Fatal("call succeeded")
			}
			mu.Lock()
			got := len(*marked)
			mu.Unlock()
			if got != map[bool]int{true: 1, false: 0}[tc.marked] {
				t.Fatalf("marked %d nodes, want marked=%v", got, tc.marked)
			}
		})
	}
}

// A GET's body deadline is callTimeout of the reply's own Content-Length: a peer that
// stalls after the headers is cut at that deadline, not after the 64 MiB allowance; a
// reply announcing more than a shard, or no length, is refused before it is read.
// Invariant: I-171
func TestClient_GetDeadline_FromContentLength(t *testing.T) {
	k := chunk.Key([32]byte{8}, 2, 1, 0)
	maxLen := int64(chunk.MaxPayload + chunk.HeaderSize)
	var mu sync.Mutex
	length, stall := int64(0), false
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n, s := length, stall
		mu.Unlock()
		if n < 0 {
			w.Header().Set("Transfer-Encoding", "chunked")
		} else {
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		}
		w.WriteHeader(http.StatusOK)
		if s {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-stop:
			}
			return
		}
		if n > maxLen { // the client must refuse before reading: send nothing
			return
		}
		buf := make([]byte, 1<<20)
		for sent := int64(0); sent < n; {
			m := min(int64(len(buf)), n-sent)
			if _, err := w.Write(buf[:m]); err != nil {
				return
			}
			sent += m
		}
	}))
	defer srv.Close()
	defer close(stop)
	set := func(n int64, s bool) {
		mu.Lock()
		length, stall = n, s
		mu.Unlock()
	}

	// the stall: headers arrive with Content-Length 1, then nothing; the fake clock's
	// timer is callTimeout(1), and advancing past it ends the call
	c, marked, mmu := clientFor(srv.Listener.Addr().String())
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	c.SetClock(clk)
	set(1, true)
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), 1, k)
		done <- err
	}()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for armed, giveUp := false, time.After(5*time.Second); !armed; {
		select {
		case <-tick.C:
			armed = clk.Pending() == 1
		case <-giveUp:
			t.Fatal("no body deadline armed after the headers")
		}
	}
	clk.Advance(callTimeout(1))
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("stalled GET: %v, want ErrUnavailable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled GET outlived its body deadline")
	}
	mmu.Lock()
	if len(*marked) != 1 {
		t.Fatalf("stalled peer marked %d times, want 1", len(*marked))
	}
	mmu.Unlock()

	// lengths: up to a shard is read whole; more, or none, is refused unread
	c2, _, _ := clientFor(srv.Listener.Addr().String())
	for _, n := range []int64{0, 1, maxLen - 1, maxLen, maxLen + 1, -1} {
		t.Run(fmt.Sprintf("len=%d", n), func(t *testing.T) {
			set(n, false)
			b, err := c2.Get(context.Background(), 1, k)
			switch {
			case n == maxLen+1:
				if !errors.Is(err, ErrTooLarge) {
					t.Fatalf("got %v, want ErrTooLarge", err)
				}
			case n < 0:
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("got %v, want ErrUnavailable", err)
				}
			default:
				if err != nil || int64(len(b)) != n {
					t.Fatalf("len %d err %v", len(b), err)
				}
			}
		})
	}
}
