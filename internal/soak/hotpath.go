package soak

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/vfs"
)

// --- 17 hot path --------------------------------------------------------------------------
//
// The read path's readahead (docs/04 §5, I-166): a file of hotpathChunks chunks is written
// through one gateway and, once committed, read whole and cold through another (or the
// same one: the flush never fills the read cache, I-78, but a client's own cache may serve
// a re-read of what it just wrote, which is why the readahead checks are skipped on a
// one-gateway rig whose counters did not move). The content is exact, the cold-read
// throughput is reported, readahead served every chunk but the first window, fetched no
// more than the file's chunks, and never ran more than ReadAhead prefetches at once.

const (
	hotpathChunks = 64
	hotpathPiece  = 1 << 20
)

func phaseHotPath(ctx context.Context, r *Run) error {
	gws := r.gateways()
	if len(gws) == 0 {
		r.skip("no gateway reachable")
		return nil
	}
	writer, reader := gws[0], gws[len(gws)-1]
	size := hotpathChunks * r.Profile.ChunkSize
	p := join(r.Root, "hotpath.bin")
	wv := r.volume(writer)
	h := sha256.New()
	for off := 0; off < size; off += hotpathPiece {
		piece := r.randomBytes(min(hotpathPiece, size-off))
		if err := wv.WriteAt(p, int64(off), piece); err != nil {
			r.checkAt("I-7", "hot-path file written", false, fmt.Sprintf("write at %d: %v", off, err), p, "", writer.Name)
			return nil
		}
		h.Write(piece)
		if err := r.Env.Wait(ctx, 0); err != nil {
			return err
		}
	}
	want := [32]byte(h.Sum(nil))
	// the read must see committed chunks: wait for the writer's overlay to drain, as the
	// large-file phase does
	var rep admin.Report
	var err error
	for range 20 {
		if werr := r.Env.Wait(ctx, r.Profile.FlushIdle); werr != nil {
			return werr
		}
		if rep, err = r.Env.Status(ctx, writer); err == nil && rep.Gateway != nil && rep.Gateway.DirtyInodes == 0 {
			break
		}
	}
	if err != nil || rep.Gateway == nil || rep.Gateway.DirtyInodes != 0 {
		r.checkAt("I-7", "hot-path file committed before the read", false, fmt.Sprintf("gateway %+v, %v", rep.Gateway, err), p, "", writer.Name)
		return nil
	}

	before, err := r.Env.Status(ctx, reader)
	if err != nil {
		return err
	}
	// sample the reading gateway while the read runs: prefetches in flight must never
	// exceed ReadAhead (I-166)
	var (
		mu      sync.Mutex
		samples int
		peak    int
	)
	note := func(g *admin.GatewayReport) {
		if g == nil {
			return
		}
		mu.Lock()
		samples++
		peak = max(peak, g.Prefetching)
		mu.Unlock()
	}
	note(before.Gateway)
	sctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-tick.C:
				if rep, err := r.Env.Status(sctx, reader); err == nil {
					note(rep.Gateway)
				}
			}
		}
	})
	rv := r.volume(reader)
	t := time.Now()
	sum, n, rerr := rv.Hash(p)
	took := time.Since(t)
	stop()
	wg.Wait()
	after, err := r.Env.Status(ctx, reader)
	if err != nil {
		return err
	}
	note(after.Gateway)
	mb := float64(size) / (1 << 20)
	r.event("cold read", fmt.Sprintf("%.0f MiB in %s (%.1f MiB/s) via %s", mb, took.Round(time.Millisecond), mb/took.Seconds(), reader.Name))
	r.checkAt("I-12 I-40", "content hash exact on the cold read", rerr == nil && n == int64(size) && sum == want, fmt.Sprintf("%d bytes via %s, %v", n, reader.Name, rerr), p, "", reader.Name)

	switch {
	case before.Gateway == nil || after.Gateway == nil:
		r.checkSkipped("I-166", "readahead served the sequential read", "the gateway reports no readahead counters (status without the gateway section)")
	default:
		fetched := after.Gateway.ReadaheadFetched - before.Gateway.ReadaheadFetched
		joined := after.Gateway.ReadaheadJoined - before.Gateway.ReadaheadJoined
		text := fmt.Sprintf("%d of %d chunks served by readahead, %d prefetched, via %s", joined, hotpathChunks, fetched, reader.Name)
		if len(gws) == 1 && fetched == 0 && joined == 0 {
			r.checkSkipped("I-166", "readahead served the sequential read", "one gateway: the client's own cache served the read it had just written; "+text)
		} else {
			r.checkAt("I-166", "readahead served the sequential read", joined >= hotpathChunks-vfs.ReadAhead-1, text, p, "", reader.Name)
			r.checkAt("I-166", "readahead fetched no more than the file's chunks", fetched <= hotpathChunks, text, p, "", reader.Name)
		}
		r.checkAt("I-166", "prefetches in flight within ReadAhead", peak <= vfs.ReadAhead, fmt.Sprintf("peak %d of %d allowed over %d samples", peak, vfs.ReadAhead, samples), p, "", reader.Name)
	}
	if err := wv.Remove(p); err != nil {
		r.checkAt("I-81", "hot-path file removed", false, err.Error(), p, "", writer.Name)
	}
	return nil
}
