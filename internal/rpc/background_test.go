package rpc

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// stalledPut opens a raw PUT that announces 100 bytes and sends 10, holding its slot
// until the connection closes or the body deadline passes; background marks it.
func stalledPut(t *testing.T, e *env, name string, background bool) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	prio := ""
	if background {
		prio = HeaderPriority + ": " + PriorityBackground + "\r\n"
	}
	fmt.Fprintf(conn, "PUT /v1/disks/d0/shards/%s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\n%sContent-Length: 100\r\n\r\n0123456789",
		key([]byte(name), 0), e.addr, HeaderCluster, meta.HexID(e.cid), prio)
	return conn
}

// Background shard requests hold at most half of a node's PUT slots: with four slots,
// two stalled background PUTs fill the background share, a third background PUT queues
// behind them, and a client-path PUT still lands at once; the mark never changes a reply.
//
// Invariant: I-175
func TestServer_BackgroundShare(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.PutSlots, o.GetSlots, o.BodyTimeout = 4, 4, 10*time.Second })
	c := e.client()
	a := stalledPut(t, e, "bg-a", true)
	b := stalledPut(t, e, "bg-b", true)
	p := bytes.Repeat([]byte{7}, 100)
	// the two stalled PUTs hold the background share once a third background PUT no
	// longer lands within the server's queueing time
	queued := make(chan error, 1)
	go func() { queued <- c.Put(chunk.Background(ctx), 1, key(p, 1), p) }()
	select {
	case err := <-queued:
		t.Fatalf("the third background PUT ran with the share full: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	start := time.Now()
	if err := c.Put(ctx, 1, key(p, 0), p); err != nil {
		t.Fatalf("foreground PUT: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the foreground PUT waited %s behind background work", took)
	}
	select {
	case err := <-queued:
		t.Fatalf("the queued background PUT ran while the share was full: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	a.Close()
	b.Close()
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("background PUT after the share freed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued background PUT never ran")
	}
	if !e.store.Has(key(p, 0)) || !e.store.Has(key(p, 1)) {
		t.Fatal("a PUT did not land")
	}
	got, err := c.Get(chunk.Background(ctx), 1, key(p, 1))
	if err != nil || !bytes.Equal(got, p) {
		t.Fatalf("background GET: %v", err)
	}
}

// The background share is half the slots, never below one.
//
// Invariant: I-175
func TestBackgroundSlots_Table(t *testing.T) {
	for _, c := range []struct{ slots, want int }{{0, 1}, {1, 1}, {2, 1}, {3, 1}, {4, 2}, {5, 2}, {16, 8}, {31, 15}, {32, 16}, {33, 16}} {
		if got := backgroundSlots(c.slots); got != c.want {
			t.Fatalf("backgroundSlots(%d) = %d, want %d", c.slots, got, c.want)
		}
	}
}

type headerSpy struct {
	base http.RoundTripper
	mu   sync.Mutex
	seen []string
}

func (s *headerSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.seen = append(s.seen, r.Header.Get(HeaderPriority))
	s.mu.Unlock()
	return s.base.RoundTrip(r)
}

// A client sends the priority header only under the Background mark, on every shard
// method.
//
// Invariant: I-175
func TestClient_BackgroundHeader(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	spy := &headerSpy{}
	c.WrapTransport(func(base http.RoundTripper) http.RoundTripper { spy.base = base; return spy })
	p := bytes.Repeat([]byte{9}, 64)
	if err := c.Put(ctx, 1, key(p, 0), p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, 1, key(p, 0)); err != nil {
		t.Fatal(err)
	}
	bg := chunk.Background(ctx)
	if err := c.Put(bg, 1, key(p, 1), p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(bg, 1, key(p, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Has(bg, 1, key(p, 1)); err != nil {
		t.Fatal(err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	want := []string{"", "", PriorityBackground, PriorityBackground, PriorityBackground}
	if fmt.Sprint(spy.seen) != fmt.Sprint(want) {
		t.Fatalf("priority headers %q, want %q", spy.seen, want)
	}
}
