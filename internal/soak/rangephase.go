package soak

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/zombar/tailraid/internal/admin"
)

// Phase 36, range reads (docs/04 §5, I-424): a four-chunk file written through one
// gateway, its last chunk partial; through another gateway, cold, a few KiB of its tail
// (a range in the partial chunk) and then of its second chunk are each served by a range
// fetch (the gateway's reads_ranged up by one each, no readahead started, the bytes
// exact); the second chunk's next READ reads it whole and a third moves nothing more.
// On a rig with one gateway the client's own cache serves the reads, and the checks are
// recorded skipped.
func phaseRangeReads(ctx context.Context, r *Run) error {
	gws := r.gateways()
	if len(gws) == 0 {
		r.skip("no gateway reachable")
		return nil
	}
	writer, reader := gws[0], gws[len(gws)-1]
	cs := r.Profile.ChunkSize
	size := 4*cs - 777
	p := join(r.Root, "range.bin")
	wv := r.volume(writer)
	data := make([]byte, 0, size)
	for off := 0; off < size; off += cs {
		piece := r.randomBytes(min(cs, size-off))
		if err := wv.WriteAt(p, int64(off), piece); err != nil {
			r.checkAt("I-7", "file written through the gateway", false, fmt.Sprintf("write at %d: %v", off, err), p, "", writer.Name)
			return nil
		}
		data = append(data, piece...)
	}
	committed := false
	for range 20 {
		if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
			return err
		}
		if rep, err := r.Env.Status(ctx, writer); err == nil && rep.Gateway != nil && rep.Gateway.DirtyInodes == 0 {
			committed = true
			break
		}
	}
	if !committed {
		r.checkAt("I-7", "file committed before the reads", false, "the writer's gateway stayed dirty", p, "", writer.Name)
		return nil
	}
	rv := r.volume(reader)
	gw := func() (*admin.GatewayReport, error) {
		rep, err := r.Env.Status(ctx, reader)
		if err != nil {
			return nil, err
		}
		return rep.Gateway, nil
	}
	before, err := gw()
	if err != nil {
		return err
	}
	if before == nil {
		r.checkSkipped("I-424", "a cold range is served by a range fetch", "the gateway reports no reader counters (status without the gateway section)")
		return nil
	}
	const n = 4096 // one client READ's worth
	read := func(name string, off int) (*admin.GatewayReport, bool, error) {
		t := time.Now()
		got, rerr := rv.ReadAt(p, int64(off), n)
		took := time.Since(t)
		after, err := gw()
		if err != nil {
			return nil, false, err
		}
		exact := rerr == nil && bytes.Equal(got, data[off:off+n])
		r.event(name, fmt.Sprintf("%d bytes at %d in %s via %s", n, off, took.Round(time.Millisecond), reader.Name))
		r.checkAt("I-12 I-40", name+" exact", exact, fmt.Sprintf("%d bytes, %v", len(got), rerr), p, "", reader.Name)
		return after, exact, nil
	}
	// the tail first: once a chunk is read whole, readahead prefetches the ones after it
	tail, ok, err := read("cold range in the partial last chunk", size-n)
	if err != nil || !ok {
		return err
	}
	if writer.Name == reader.Name {
		r.checkSkipped("I-424", "a cold range is served by a range fetch", "one gateway: the client's own cache served the read it had just written")
		return nil
	}
	r.checkAt("I-424", "a range in a partial chunk is served by a range fetch", tail.ReadsRanged == before.ReadsRanged+1,
		fmt.Sprintf("reads_ranged +%d via %s", tail.ReadsRanged-before.ReadsRanged, reader.Name), p, "", reader.Name)
	first, ok, err := read("cold range in the second chunk", cs+n)
	if err != nil || !ok {
		return err
	}
	ranged := first.ReadsRanged - tail.ReadsRanged
	fetched := first.ReadaheadFetched - before.ReadaheadFetched
	r.checkAt("I-424", "a cold range is served by a range fetch", ranged == 1 && fetched == 0,
		fmt.Sprintf("reads_ranged +%d, readahead_fetched +%d over two cold ranges via %s", ranged, fetched, reader.Name), p, "", reader.Name)
	second, ok, err := read("the chunk's second range", cs+2*n)
	if err != nil || !ok {
		return err
	}
	r.checkAt("I-424", "the chunk's next range reads it whole", second.ReadsRanged == first.ReadsRanged,
		fmt.Sprintf("reads_ranged +%d on the second touch via %s", second.ReadsRanged-first.ReadsRanged, reader.Name), p, "", reader.Name)
	third, ok, err := read("the chunk's third range", cs+3*n)
	if err != nil || !ok {
		return err
	}
	r.checkAt("I-424", "a chunk read whole serves its later ranges", third.ReadsRanged == second.ReadsRanged,
		fmt.Sprintf("reads_ranged +%d on the third touch via %s", third.ReadsRanged-second.ReadsRanged, reader.Name), p, "", reader.Name)
	return nil
}
