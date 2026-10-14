package rpc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
)

// A shard GET the caller gives up on after its headers arrived keeps its connection when
// at most DrainMax bytes are left as it notices (between reads of getStep): the rest is read and dropped, and the next request
// reuses the connection. With more left it closes it, as before. Before, every cancelled
// GET closed its connection (Go retires one whose response it stopped reading), so a
// hedged read or a readahead cut short by a seek cost a handshake and a cold congestion
// window on the next request to that host (docs/reports/rpc-transports-2026-10.md).
// Invariant: I-262
func TestClient_CancelledGet_DrainsAShortRest(t *testing.T) {
	const head = 1 << 10 // sent before the caller gives up; it may still be unread then
	for _, tc := range []struct {
		name   string
		rest   int // bytes the server still has to send when the caller gives up
		reused bool
	}{
		{"nothing left", 0, true},
		{"one byte left", 1, true},
		{"DrainMax left, the head included", DrainMax - head, true},
		// the caller's cancel is noticed between reads, so the read under way may take up to
		// getStep of the rest first: more than DrainMax is still left only past this
		{"DrainMax+getStep+1 left", DrainMax + getStep + 1, false},
		{"a whole shard left", 1 << 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var conns atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/again") {
					w.Header().Set("Content-Length", "1")
					_, _ = w.Write([]byte{1})
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(head+tc.rest))
				_, _ = w.Write(make([]byte, head))
				w.(http.Flusher).Flush()
				<-release // the caller gives up here
				_, _ = w.Write(make([]byte, tc.rest))
			}))
			srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				if s == http.StateNew {
					conns.Add(1)
				}
			}
			srv.Start()
			defer srv.Close()
			// runs before srv.Close, which waits for the handler: a failing test never hangs
			releaseRest := sync.OnceFunc(func() { close(release) })
			defer releaseRest()
			addr := strings.TrimPrefix(srv.URL, "http://")
			c := NewClient([16]byte{1}, func(meta.NodeIdx) (string, bool, bool) { return addr, false, true })
			c.SetDisks(disk0, nil)
			// one connection to the host: the second request waits for the first's to be
			// returned to the pool (reused) or closed (a new dial), instead of racing the
			// transport's return of a drained connection with a dial of its own
			c.http.Transport.(*http.Transport).MaxConnsPerHost = 1
			headers := make(chan struct{}, 1)
			c.http.Transport = signalRT{c.http.Transport, headers}

			ctx, cancel := context.WithCancel(t.Context())
			got := make(chan error, 1)
			go func() { _, err := c.Get(ctx, 1, chunk.ShardKey("k")); got <- err }()
			// the headers are in (a GET that already completed had them too): give up, then
			// let the rest come
			select {
			case <-headers:
			case <-time.After(10 * time.Second):
				t.Fatal("no headers within 10 s")
			}
			cancel()
			releaseRest()
			// a short rest may arrive within the read under way: the GET then completes,
			// which keeps the connection as well
			if err := <-got; !errors.Is(err, context.Canceled) && (!tc.reused || err != nil) {
				t.Fatalf("a GET the caller gave up on: %v, want context.Canceled", err)
			}
			// the next request on that host: one new connection unless the first was kept
			resp, err := c.do(t.Context(), http.MethodGet, addr, shardPrefix+"x/again", nil, nil) // the shard transport, as the GET
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if reused := conns.Load() == 1; reused != tc.reused {
				t.Fatalf("%d connections for two requests: reused %v, want %v", conns.Load(), reused, tc.reused)
			}
		})
	}
}

// signalRT tells the test when a response's headers arrived.
type signalRT struct {
	http.RoundTripper
	headers chan struct{}
}

func (s signalRT) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.RoundTripper.RoundTrip(r)
	if err == nil {
		select {
		case s.headers <- struct{}{}:
		default:
		}
	}
	return resp, err
}
