package rpctransports

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const shardSize = 1468006 // a 4 MiB chunk's shard at 3+2

// client is one transport's view of the node RPC: a shard PUT, a shard GET read whole
// (cancelled through ctx) and a small control call, each to host h.
type client interface {
	put(ctx context.Context, h int, body []byte) error
	get(ctx context.Context, h int) error
	apply(ctx context.Context, h int, body []byte) error
}

// rig is a running set of hosts and a client to them.
type rig struct {
	c     client
	conns func() int64 // connections the hosts accepted so far
}

// transport starts hosts for a profile; slow lowers a host's outbound bandwidth by its
// factor (the hedged read's slow holder).
type transport struct {
	name  string
	start func(b *testing.B, p profile, hosts int, slow map[int]float64) rig
}

var transports = []transport{
	{"h1", startHTTP(false)},
	{"h2", startHTTP(true)},
	{"grpc", startGRPC(grpcUnary, false)},
	{"grpc-tuned", startGRPC(grpcUnary, true)},
	{"grpc-stream", startGRPC(grpcStream, false)},
	{"mux", startMux(false)},
	{"mux-4MiB", startMux(true)},
	{"quic", startQUIC()},
}

// hostLinks are one host's two directions.
func hostLinks(p profile, slow float64) (up, down *link) {
	if slow == 0 {
		slow = 1
	}
	return &link{rate: p.rate}, &link{rate: p.rate * slow}
}

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

// run does workers ops at once untimed, twice (the connections a steady state keeps),
// then b.N ops on workers goroutines, and reports the connections the timed ops opened
// per op.
func run(b *testing.B, rg rig, workers int, op func(i int) error) {
	b.Helper()
	for range 2 {
		var wg sync.WaitGroup
		errs := make([]error, workers)
		for w := range workers {
			wg.Go(func() { errs[w] = op(-1) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			b.Fatal(err)
		}
	}
	base := rg.conns()
	b.ResetTimer()
	var next atomic.Int64
	var wg sync.WaitGroup
	var first atomic.Value
	for range workers {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= b.N {
					return
				}
				if err := op(i); err != nil {
					first.CompareAndSwap(nil, err)
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	if err, ok := first.Load().(error); ok && err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(rg.conns()-base)/float64(b.N), "conns/op")
}

func each(b *testing.B, f func(b *testing.B, p profile, tr transport)) {
	if testing.Short() {
		b.Skip("slow: a simulated network path per host")
	}
	for _, p := range profiles {
		for _, tr := range transports {
			b.Run(p.name+"/"+tr.name, func(b *testing.B) { f(b, p, tr) })
		}
	}
}

// The flusher's steady state: 8 chunks at once, each PUTting its 5 shards to 5 hosts
// (docs/04 §9).
func BenchmarkPutFanout(b *testing.B) {
	each(b, func(b *testing.B, p profile, tr transport) {
		rg := tr.start(b, p, 5, nil)
		body := bytes.Repeat([]byte{0xa5}, shardSize)
		var lat latencies
		start := time.Now()
		run(b, rg, 8, func(i int) error {
			t0 := time.Now()
			var wg sync.WaitGroup
			errs := make([]error, 5)
			for s := range 5 {
				wg.Go(func() { errs[s] = rg.c.put(context.Background(), s, body) })
			}
			wg.Wait()
			if i >= 0 {
				lat.add(time.Since(t0))
			}
			return errors.Join(errs...)
		})
		b.ReportMetric(float64(b.N*5*shardSize)/time.Since(start).Seconds()/(1<<20), "MiB/s")
		lat.report(b, "chunk-")
	})
}

// A hedged read: 4 holders asked, the last cancelled once 3 shards arrived. Host 3 has a
// quarter of the bandwidth, so its GET is cut mid-body.
func BenchmarkHedgedGet(b *testing.B) {
	each(b, func(b *testing.B, p profile, tr transport) {
		rg := tr.start(b, p, 4, map[int]float64{3: 0.25})
		var lat latencies
		run(b, rg, 8, func(i int) error {
			t0 := time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			got := make(chan error, 4)
			for s := range 4 {
				go func() { got <- rg.c.get(ctx, s) }()
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
			<-got
			return errors.Join(errs...)
		})
		lat.report(b, "read-")
	})
}

// Readahead and a seek (I-203): a read and three prefetches start together, and the seek
// that follows the read cancels the prefetches still in flight. The op lasts until all
// four returned.
func BenchmarkReadaheadSeek(b *testing.B) {
	each(b, func(b *testing.B, p profile, tr transport) {
		rg := tr.start(b, p, 1, nil)
		var lat latencies
		run(b, rg, 2, func(i int) error {
			t0 := time.Now()
			pctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var wg sync.WaitGroup
			for range 3 {
				wg.Go(func() { _ = rg.c.get(pctx, 0) })
			}
			err := rg.c.get(context.Background(), 0)
			cancel()
			wg.Wait()
			if i >= 0 {
				lat.add(time.Since(t0))
			}
			return err
		})
		lat.report(b, "op-")
	})
}

// Proposals forwarded to the leader: 32 calls of 200 bytes at once (I-255).
func BenchmarkControlBurst(b *testing.B) {
	each(b, func(b *testing.B, p profile, tr transport) {
		rg := tr.start(b, p, 1, nil)
		cmd := bytes.Repeat([]byte{'c'}, 200)
		var call latencies
		run(b, rg, 1, func(i int) error {
			var wg sync.WaitGroup
			errs := make([]error, 32)
			for c := range 32 {
				wg.Go(func() {
					t0 := time.Now()
					errs[c] = rg.c.apply(context.Background(), 0, cmd)
					if i >= 0 {
						call.add(time.Since(t0))
					}
				})
			}
			wg.Wait()
			return errors.Join(errs...)
		})
		call.report(b, "call-")
	})
}

// A repair pass streams shards from a host (4 GETs at once) while control calls to the
// same host run one at a time.
func BenchmarkMixedRepairAndControl(b *testing.B) {
	each(b, func(b *testing.B, p profile, tr transport) {
		rg := tr.start(b, p, 1, nil)
		ctx, stop := context.WithCancel(context.Background())
		var bg sync.WaitGroup
		for range 4 {
			bg.Go(func() {
				for ctx.Err() == nil {
					_ = rg.c.get(ctx, 0)
				}
			})
		}
		cmd := bytes.Repeat([]byte{'c'}, 200)
		var call latencies
		run(b, rg, 1, func(i int) error {
			t0 := time.Now()
			err := rg.c.apply(context.Background(), 0, cmd)
			if i >= 0 {
				call.add(time.Since(t0))
			}
			return err
		})
		stop()
		bg.Wait()
		call.report(b, "call-")
	})
}
