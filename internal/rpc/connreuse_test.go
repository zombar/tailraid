package rpc

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// The control transport keeps as many idle connections to a host as a burst of control
// calls opens: a second burst of the same size dials none. Proposals forwarded to the
// leader arrive in bursts (every gateway's flush workers, repair workers, GC, stats);
// with 4 idle connections per host, every call past the fourth dialed a fresh TCP
// connection and closed it after.
// Invariant: I-255
func TestClient_ControlBurst_ReusesConnections(t *testing.T) {
	const burst = 16
	var dialed atomic.Int32
	var mu sync.Mutex
	arrived, release := 0, make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		arrived++
		last := arrived%burst == 0
		wait := release
		if last {
			close(release)
			release = make(chan struct{})
		}
		mu.Unlock()
		if !last {
			<-wait // every call of the burst is in flight at once
		}
		_, _ = io.WriteString(w, "{}")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialed.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	c := NewClient([16]byte{1}, func(meta.NodeIdx) (string, bool, bool) { return addr, false, true })

	wave := func() {
		var wg sync.WaitGroup
		for range burst {
			wg.Go(func() {
				resp, err := c.do(t.Context(), http.MethodGet, addr, "/v1/status", nil, nil)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			})
		}
		wg.Wait()
	}
	wave()
	first := dialed.Load()
	if first < burst {
		t.Fatalf("the first burst opened %d connections, want %d (the calls were not concurrent)", first, burst)
	}
	wave()
	if again := dialed.Load() - first; again != 0 {
		t.Fatalf("a second burst of %d control calls dialed %d new connections, want 0", burst, again)
	}
}
