package rpc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
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
	// the two stalled PUTs hold the whole background share before the third is sent
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for deadline := time.After(10 * time.Second); len(e.srv.bgPutSem) < 2; {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("the stalled PUTs hold %d background slots, want 2", len(e.srv.bgPutSem))
		}
	}
	p := bytes.Repeat([]byte{7}, 100)
	queued := make(chan error, 1)
	go func() { queued <- c.Put(chunk.Background(ctx), 1, key(p, 1), p) }()
	select {
	case err := <-queued:
		t.Fatalf("the third background PUT ran with the share full: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// the client-path PUT lands while the background one is still queued: the order,
	// not a wall-clock bound, is the property (a slow runner takes a second per call)
	if err := c.Put(ctx, 1, key(p, 0), p); err != nil {
		t.Fatalf("foreground PUT: %v", err)
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

// expectRecord is what expectSpies share: the Expect header of every request and the
// body bytes the transport read.
type expectRecord struct {
	mu   sync.Mutex
	seen []string
	read int64
}

// expectSpy wraps one transport (the client has two) and reports to a record.
type expectSpy struct {
	base http.RoundTripper
	rec  *expectRecord
}

type countReader struct {
	r io.ReadCloser
	n *int64
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

func (c countReader) Close() error { return c.r.Close() }

func (s *expectSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	s.rec.mu.Lock()
	s.rec.seen = append(s.rec.seen, r.Method+" "+r.Header.Get("Expect"))
	s.rec.mu.Unlock()
	if r.Body != nil {
		r.Body = countReader{r.Body, &s.rec.read}
	}
	return s.base.RoundTrip(r)
}

func spyExpect(c *Client) *expectRecord {
	rec := &expectRecord{}
	c.WrapTransport(func(base http.RoundTripper) http.RoundTripper { return &expectSpy{base: base, rec: rec} })
	return rec
}

// A background PUT asks the node to continue before it sends its body; a foreground PUT,
// a GET and a HEAD do not. The transport waits for the node's answer longer than a node
// holds a PUT for a slot, so a queued background PUT is not sent twice over.
// Invariant: I-420
func TestClient_ExpectContinue_BackgroundPutOnly(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	rec := spyExpect(c)
	if tr := c.http.Transport.(*expectSpy).base.(*http.Transport); tr.ExpectContinueTimeout <= SlotWait {
		t.Fatalf("ExpectContinueTimeout %v, want above SlotWait %v", tr.ExpectContinueTimeout, SlotWait)
	}
	p := bytes.Repeat([]byte{9}, 64)
	if err := c.Put(ctx, 1, key(p, 0), p); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(chunk.Background(ctx), 1, key(p, 1), p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(chunk.Background(ctx), 1, key(p, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Has(chunk.Background(ctx), 1, key(p, 0)); err != nil {
		t.Fatal(err)
	}
	want := []string{"PUT ", "PUT 100-continue", "GET ", "HEAD "}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !slices.Equal(rec.seen, want) {
		t.Fatalf("Expect headers %q, want %q", rec.seen, want)
	}
}

// A node without room answers a background PUT's 507 before the body: not one byte of
// the shard leaves the sender. A foreground PUT gets the same answer, its body sent or
// not.
// Invariant: I-420
// Lifecycle: L-44
func TestChunkRPC_FullNodeIs507_NoBodySent(t *testing.T) {
	small, _ := chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "s"), 100)
	e := newEnvWith(t, func(o *Options) { o.Disk = func(string) (ShardStore, meta.NodeIdx, bool) { return small, 1, true } })
	c := e.client()
	rec := spyExpect(c)
	p := bytes.Repeat([]byte{1}, 200)
	if err := c.Put(chunk.Background(ctx), 1, key(p, 0), p); !errors.Is(err, chunk.ErrFull) {
		t.Fatalf("background PUT to a full node: %v, want ErrFull", err)
	}
	if n := atomic.LoadInt64(&rec.read); n != 0 {
		t.Fatalf("the transport read %d body bytes for a refused background PUT, want none", n)
	}
	if err := c.Put(ctx, 1, key(p, 0), p); !errors.Is(err, chunk.ErrFull) {
		t.Fatalf("foreground PUT to a full node: %v, want ErrFull", err)
	}
}
