package rpc

// HTTP/1.1 against cleartext HTTP/2 for the node RPC's traffic (docs/reports/http2-2026-10.md).
// Benchmarks only: nothing here changes the transport the node uses. Each host is a real
// TCP listener on loopback behind a link that delays every write by a one-way latency and
// paces it through a bandwidth shared by every connection to that host, per direction, so
// one TCP connection and many share the same pipe as they would on a tailnet path.
//
//	go test ./internal/rpc -run '^$' -bench RPCTransport -benchtime 60x

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// netProfile is one path: one-way delay and each direction's bandwidth per host.
type netProfile struct {
	name  string
	delay time.Duration
	rate  float64 // bytes/s per direction per host
}

var netProfiles = []netProfile{
	{"lan", 250 * time.Microsecond, 100 << 20},     // 0.5 ms RTT, ~800 Mbit/s
	{"tailnet", 7500 * time.Microsecond, 25 << 20}, // 15 ms RTT, ~200 Mbit/s (Wi-Fi, a relayed path)
}

// link serializes the bytes of every connection to one host in one direction.
type link struct {
	mu   sync.Mutex
	rate float64
	next time.Time
}

// slot is when n bytes written now have left the link.
func (l *link) slot(n int) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	return l.next
}

type piece struct {
	b   []byte
	due time.Time
}

// slowConn delivers each write after the link has carried it plus the one-way delay.
type slowConn struct {
	net.Conn
	delay  time.Duration
	link   *link
	q      chan piece
	closed chan struct{}
	once   sync.Once
}

func newSlowConn(c net.Conn, delay time.Duration, l *link) *slowConn {
	s := &slowConn{Conn: c, delay: delay, link: l, q: make(chan piece, 1024), closed: make(chan struct{})}
	go s.pump()
	return s
}

func (s *slowConn) Write(p []byte) (int, error) {
	pc := piece{b: append([]byte(nil), p...), due: s.link.slot(len(p)).Add(s.delay)}
	select {
	case s.q <- pc:
		return len(p), nil
	case <-s.closed:
		return 0, net.ErrClosed
	}
}

func (s *slowConn) pump() {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		select {
		case pc := <-s.q:
			if d := time.Until(pc.due); d > 0 {
				t.Reset(d)
				select {
				case <-t.C:
				case <-s.closed:
					return
				}
			}
			if _, err := s.Conn.Write(pc.b); err != nil {
				return
			}
		case <-s.closed:
			return
		}
	}
}

func (s *slowConn) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.Conn.Close()
}

type slowListener struct {
	net.Listener
	delay time.Duration
	link  *link
}

func (l slowListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newSlowConn(c, l.delay, l.link), nil
}

// benchHost is one node: its server, its links and the connections it accepted.
type benchHost struct {
	addr     string
	srv      *http.Server
	up, down *link
	conns    atomic.Int64
	done     chan struct{}
}

// transport is one protocol setting of the client and the servers.
type transport struct {
	name   string
	h2     bool
	window int // the HTTP/2 receive windows (per stream; per connection 8×); 0 = Go's defaults
}

var transports = []transport{
	{name: "h1"},
	{name: "h2", h2: true},
	{name: "h2-4MiB", h2: true, window: 4 << 20},
}

func (tp transport) http2Config() *http.HTTP2Config {
	if tp.window == 0 {
		return nil
	}
	return &http.HTTP2Config{MaxReceiveBufferPerStream: tp.window, MaxReceiveBufferPerConnection: 8 * tp.window}
}

const shardSize = 1468006 // a 4 MiB chunk's shard at 3+2

func shardHandler() http.Handler {
	payload := bytes.Repeat([]byte{0x5a}, 32<<10)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /shard", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /shard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(shardSize))
		for left := shardSize; left > 0; {
			n := min(left, len(payload))
			if _, err := w.Write(payload[:n]); err != nil {
				return
			}
			left -= n
		}
	})
	reply := bytes.Repeat([]byte{'r'}, 100)
	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write(reply)
	})
	return mux
}

type benchRig struct {
	hosts  []*benchHost
	byAddr map[string]*benchHost
	client *http.Client
	tr     *http.Transport
}

func newBenchRig(b *testing.B, n int, np netProfile, tp transport, slow map[int]float64) *benchRig {
	b.Helper()
	rg := &benchRig{byAddr: map[string]*benchHost{}}
	var sp http.Protocols
	sp.SetHTTP1(true)
	sp.SetUnencryptedHTTP2(true)
	for i := range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatal(err)
		}
		rate := np.rate
		if f, ok := slow[i]; ok {
			rate *= f
		}
		h := &benchHost{addr: ln.Addr().String(), up: &link{rate: np.rate}, down: &link{rate: rate}, done: make(chan struct{})}
		h.srv = &http.Server{
			Handler: shardHandler(), Protocols: &sp, HTTP2: tp.http2Config(), ReadHeaderTimeout: 10 * time.Second,
			ConnState: func(_ net.Conn, s http.ConnState) {
				if s == http.StateNew {
					h.conns.Add(1)
				}
			},
		}
		go func() { _ = h.srv.Serve(slowListener{Listener: ln, delay: np.delay, link: h.down}); close(h.done) }()
		rg.hosts = append(rg.hosts, h)
		rg.byAddr[h.addr] = h
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	rg.tr = &http.Transport{
		MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return newSlowConn(c, np.delay, rg.byAddr[addr].up), nil
		},
	}
	if tp.h2 {
		var cp http.Protocols
		cp.SetUnencryptedHTTP2(true) // prior knowledge: no upgrade round trip
		rg.tr.Protocols = &cp
		rg.tr.HTTP2 = tp.http2Config()
	}
	rg.client = &http.Client{Transport: rg.tr}
	b.Cleanup(func() {
		rg.tr.CloseIdleConnections()
		for _, h := range rg.hosts {
			_ = h.srv.Close()
			<-h.done
		}
	})
	return rg
}

func (rg *benchRig) newConns() int64 {
	var n int64
	for _, h := range rg.hosts {
		n += h.conns.Load()
	}
	return n
}

func (rg *benchRig) put(ctx context.Context, h *benchHost, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+h.addr+"/shard", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := rg.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// get reads a shard; stopAfter > 0 cancels it once that many bytes arrived (a seek).
func (rg *benchRig) get(ctx context.Context, h *benchHost, stopAfter int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+h.addr+"/shard", http.NoBody)
	if err != nil {
		return err
	}
	resp, err := rg.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if stopAfter > 0 {
		_, err = io.CopyN(io.Discard, resp.Body, int64(stopAfter))
		cancel()
		return err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (rg *benchRig) apply(ctx context.Context, h *benchHost, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+h.addr+"/apply", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := rg.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// latencies collects one duration per operation and reports p50, p99 and the operations'
// new TCP connections.
type latencies struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d)
	l.mu.Unlock()
}

func (l *latencies) report(b *testing.B, prefix string) {
	b.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return
	}
	s := slices.Clone(l.d)
	slices.Sort(s)
	q := func(p float64) float64 {
		return float64(s[min(len(s)-1, int(p*float64(len(s))))]) / float64(time.Millisecond)
	}
	b.ReportMetric(q(0.50), prefix+"p50-ms")
	b.ReportMetric(q(0.99), prefix+"p99-ms")
}

// runWorkers runs op once untimed to open the connections a steady state keeps, then b.N
// times on workers goroutines, and returns the TCP connections the timed runs opened.
func runWorkers(b *testing.B, rg *benchRig, workers int, op func(i int) error) int64 {
	b.Helper()
	if err := op(-1); err != nil {
		b.Fatal(err)
	}
	base := rg.newConns()
	b.ResetTimer()
	var next atomic.Int64
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for range workers {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= b.N {
					return
				}
				if err := op(i); err != nil {
					firstErr.CompareAndSwap(nil, err)
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	if err, ok := firstErr.Load().(error); ok && err != nil {
		b.Fatal(err)
	}
	return rg.newConns() - base
}

func eachSetting(b *testing.B, run func(b *testing.B, np netProfile, tp transport)) {
	if testing.Short() {
		b.Skip("slow: a real network path per host")
	}
	for _, np := range netProfiles {
		for _, tp := range transports {
			b.Run(np.name+"/"+tp.name, func(b *testing.B) { run(b, np, tp) })
		}
	}
}

// Shard PUTs of one chunk to its k+m = 5 hosts, ChunkWorkers = 8 chunks at once (docs/04
// §9): the flusher's steady state. "one host" sends all five to one node, which is where
// one HTTP/2 connection's receive window is shared by every stream.
func BenchmarkRPCTransport_PutFanout(b *testing.B) {
	for _, spread := range []string{"five-hosts", "one-host"} {
		b.Run(spread, func(b *testing.B) {
			eachSetting(b, func(b *testing.B, np netProfile, tp transport) {
				rg := newBenchRig(b, 5, np, tp, nil)
				body := bytes.Repeat([]byte{0xa5}, shardSize)
				var lat latencies
				ctx := context.Background()
				start := time.Now()
				conns := runWorkers(b, rg, 8, func(i int) error {
					t0 := time.Now()
					var wg sync.WaitGroup
					errs := make([]error, 5)
					for s := range 5 {
						h := rg.hosts[s]
						if spread == "one-host" {
							h = rg.hosts[0]
						}
						wg.Go(func() { errs[s] = rg.put(ctx, h, body) })
					}
					wg.Wait()
					if i >= 0 {
						lat.add(time.Since(t0))
					}
					return errors.Join(errs...)
				})
				el := time.Since(start)
				b.ReportMetric(float64(b.N*5*shardSize)/el.Seconds()/(1<<20), "MiB/s")
				b.ReportMetric(float64(conns)/float64(b.N), "conns/op")
				lat.report(b, "chunk-")
			})
		})
	}
}

// A read asks k+1 = 4 holders for a chunk's shards and cancels the last once k = 3
// arrived: the hedged read. Host 3 is the slow one, so its GET is cut mid-body; on
// HTTP/1.1 that closes its connection and the next read dials again.
func BenchmarkRPCTransport_HedgedGet(b *testing.B) {
	eachSetting(b, func(b *testing.B, np netProfile, tp transport) {
		rg := newBenchRig(b, 4, np, tp, map[int]float64{3: 0.25})
		var lat latencies
		ctx := context.Background()
		start := time.Now()
		conns := runWorkers(b, rg, 8, func(i int) error {
			t0 := time.Now()
			octx, cancel := context.WithCancel(ctx)
			defer cancel()
			got := make(chan error, 4)
			for s := range 4 {
				go func() { got <- rg.get(octx, rg.hosts[s], 0) }()
			}
			var errs []error
			for range 3 {
				if err := <-got; err != nil {
					errs = append(errs, err)
				}
			}
			if i >= 0 {
				lat.add(time.Since(t0))
			}
			cancel()
			<-got // the loser, cancelled
			return errors.Join(errs...)
		})
		el := time.Since(start)
		b.ReportMetric(float64(b.N*3*shardSize)/el.Seconds()/(1<<20), "MiB/s")
		b.ReportMetric(float64(conns)/float64(b.N), "conns/op")
		lat.report(b, "read-")
	})
}

// Readahead fetches the next chunks of a file and a seek cancels them part-way (docs/04
// §5, I-203): each op reads one shard whole and cuts three prefetches after 64 KiB.
func BenchmarkRPCTransport_ReadaheadSeek(b *testing.B) {
	eachSetting(b, func(b *testing.B, np netProfile, tp transport) {
		rg := newBenchRig(b, 1, np, tp, nil)
		h := rg.hosts[0]
		var lat latencies
		ctx := context.Background()
		conns := runWorkers(b, rg, 2, func(i int) error {
			t0 := time.Now()
			var wg sync.WaitGroup
			errs := make([]error, 4)
			wg.Go(func() { errs[0] = rg.get(ctx, h, 0) })
			for p := 1; p < 4; p++ {
				wg.Go(func() { errs[p] = rg.get(ctx, h, 64<<10) })
			}
			wg.Wait()
			if i >= 0 {
				lat.add(time.Since(t0))
			}
			return errors.Join(errs...)
		})
		b.ReportMetric(float64(conns)/float64(b.N), "conns/op")
		lat.report(b, "op-")
	})
}

// Proposals forwarded to the leader come in bursts: 32 calls of a 200-byte command and a
// 100-byte reply at once (I-255).
func BenchmarkRPCTransport_ControlBurst(b *testing.B) {
	eachSetting(b, func(b *testing.B, np netProfile, tp transport) {
		rg := newBenchRig(b, 1, np, tp, nil)
		h := rg.hosts[0]
		cmd := bytes.Repeat([]byte{'c'}, 200)
		var burst, call latencies
		ctx := context.Background()
		conns := runWorkers(b, rg, 1, func(i int) error {
			t0 := time.Now()
			var wg sync.WaitGroup
			errs := make([]error, 32)
			for c := range 32 {
				wg.Go(func() {
					c0 := time.Now()
					errs[c] = rg.apply(ctx, h, cmd)
					if i >= 0 {
						call.add(time.Since(c0))
					}
				})
			}
			wg.Wait()
			if i >= 0 {
				burst.add(time.Since(t0))
			}
			return errors.Join(errs...)
		})
		b.ReportMetric(float64(conns)/float64(b.N), "conns/op")
		burst.report(b, "burst-")
		call.report(b, "call-")
	})
}

// A repair pass streams shards from a node (four GETs at once, background) while a
// gateway's control calls to the same node run: one TCP connection carries both on
// HTTP/2, separate connections on HTTP/1.1; the link is shared either way.
func BenchmarkRPCTransport_MixedRepairAndControl(b *testing.B) {
	eachSetting(b, func(b *testing.B, np netProfile, tp transport) {
		rg := newBenchRig(b, 1, np, tp, nil)
		h := rg.hosts[0]
		ctx, stop := context.WithCancel(context.Background())
		var bg sync.WaitGroup
		for range 4 {
			bg.Go(func() {
				for ctx.Err() == nil {
					_ = rg.get(ctx, h, 0)
				}
			})
		}
		cmd := bytes.Repeat([]byte{'c'}, 200)
		var call latencies
		conns := runWorkers(b, rg, 1, func(i int) error {
			c0 := time.Now()
			err := rg.apply(context.Background(), h, cmd)
			if i >= 0 {
				call.add(time.Since(c0))
			}
			return err
		})
		b.ReportMetric(float64(conns)/float64(b.N), "conns/op")
		stop()
		bg.Wait()
		call.report(b, "call-")
	})
}
