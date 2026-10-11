package soak

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/cluster"
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
	want, ok, err := r.writeCommitted(ctx, writer, p, size)
	if err != nil {
		return err
	}
	if !ok {
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
			r.checkAt("I-166", "readahead served the sequential read", joined >= uint64(hotpathChunks-vfs.ReadaheadDepth(uint32(r.Profile.ChunkSize))-1), text, p, "", reader.Name)
			r.checkAt("I-166", "readahead fetched no more than the file's chunks", fetched <= hotpathChunks, text, p, "", reader.Name)
			early := after.Gateway.ReadsEarly - before.Gateway.ReadsEarly
			r.checkAt("I-202", "a cold ranged read was served from verified data shards", early > 0, fmt.Sprintf("%d ranges served early via %s", early, reader.Name), p, "", reader.Name)
		}
		r.checkAt("I-166", "prefetches in flight within ReadAhead", peak <= vfs.ReadAhead, fmt.Sprintf("peak %d of %d allowed over %d samples", peak, vfs.ReadAhead, samples), p, "", reader.Name)
	}
	if err := wv.Remove(p); err != nil {
		r.checkAt("I-81", "hot-path file removed", false, err.Error(), p, "", writer.Name)
	}
	return r.hotpathStraggler(ctx, writer)
}

// writeCommitted writes size random bytes to p through gw in hotpathPiece pieces and
// waits for the gateway's overlay to drain, as the large-file phase does, so a read
// elsewhere sees committed chunks. ok is false when a check failed (recorded); err is
// the environment's.
func (r *Run) writeCommitted(ctx context.Context, gw NodeRef, p string, size int) (want [32]byte, ok bool, err error) {
	v := r.volume(gw)
	h := sha256.New()
	for off := 0; off < size; off += hotpathPiece {
		piece := r.randomBytes(min(hotpathPiece, size-off))
		if werr := v.WriteAt(p, int64(off), piece); werr != nil {
			r.checkAt("I-7", "file written through the gateway", false, fmt.Sprintf("write at %d: %v", off, werr), p, "", gw.Name)
			return want, false, nil
		}
		h.Write(piece)
		if err := r.Env.Wait(ctx, 0); err != nil {
			return want, false, err
		}
	}
	want = [32]byte(h.Sum(nil))
	var rep admin.Report
	for range 20 {
		if werr := r.Env.Wait(ctx, r.Profile.FlushIdle); werr != nil {
			return want, false, werr
		}
		if rep, err = r.Env.Status(ctx, gw); err == nil && rep.Gateway != nil && rep.Gateway.DirtyInodes == 0 {
			break
		}
	}
	if err != nil || rep.Gateway == nil || rep.Gateway.DirtyInodes != 0 {
		r.checkAt("I-7", "file committed before the read", false, fmt.Sprintf("gateway %+v, %v", rep.Gateway, err), p, "", gw.Name)
		return want, false, nil
	}
	return want, true, nil
}

// hotpathStraggler cuts the writing gateway off from one holder in the middle of a
// write (docs/05 §6): the gateway marks the holder suspect within the straggler grace,
// before the prober's liveness threshold, the chunks that follow place around it, and
// once healed the degraded chunks are filled. Skipped where the environment cannot
// partition (a live cluster without --destructive and --restarts).
func (r *Run) hotpathStraggler(ctx context.Context, gw NodeRef) error {
	const name = "a holder cut mid-write is suspect within the grace"
	if !r.Env.Caps().Partition {
		r.checkSkipped("I-169 I-170", name, "the environment cannot partition (a live cluster needs --destructive --restarts)")
		return nil
	}
	rep, err := r.Env.Status(ctx, gw)
	if err != nil {
		return err
	}
	leader := r.leaderRef(ctx)
	var victim NodeRef
	for i := range rep.Nodes {
		n := &rep.Nodes[i]
		if n.Kind != "device" || !n.Live || n.Diskless || n.Idx == gw.Idx || n.Idx == leader.Idx || n.State != "active" {
			continue
		}
		if ref, ok := r.nodeByIdx(int(n.Idx)); ok {
			victim = ref
			break
		}
	}
	if victim.Name == "" {
		r.checkSkipped("I-169 I-170", name, "no live holder other than the gateway and the leader to cut off")
		return nil
	}
	size := 2 * hotpathChunks * r.Profile.ChunkSize
	p := join(r.Root, "hotpath-cut.bin")
	v := r.volume(gw)
	healed := false
	heal := func() error {
		if healed {
			return nil
		}
		healed = true
		return r.Env.Heal(ctx)
	}
	defer func() { _ = heal() }()
	cut := -1
	var cutAt time.Time
	suspectAt, deadAt := time.Duration(-1), time.Duration(-1)
	for off := 0; off < size; off += hotpathPiece {
		piece := r.randomBytes(min(hotpathPiece, size-off))
		if err := v.WriteAt(p, int64(off), piece); err != nil {
			r.checkAt("I-7", "writes acknowledged across the cut", false, fmt.Sprintf("write at %d: %v", off, err), p, "", gw.Name)
			return nil
		}
		if err := r.Env.Wait(ctx, 0); err != nil {
			return err
		}
		if cut < 0 && off+hotpathPiece >= size/4 { // a quarter in: cut the gateway off from the victim
			cut = (off + hotpathPiece) / r.Profile.ChunkSize
			cutAt = time.Now()
			if err := r.Env.Partition(ctx, []NodeRef{gw}, []NodeRef{victim}); err != nil {
				return err
			}
			r.event("holder cut off", fmt.Sprintf("%s cut off from %s at chunk %d", gw.Name, victim.Name, cut))
		}
		if cut >= 0 && (suspectAt < 0 || deadAt < 0) {
			if st, err := r.Env.Status(ctx, gw); err == nil {
				for i := range st.Nodes {
					if st.Nodes[i].Idx != victim.Idx {
						continue
					}
					if st.Nodes[i].Suspect && suspectAt < 0 {
						suspectAt = time.Since(cutAt)
					}
					if st.Nodes[i].DeadFor != "" && deadAt < 0 {
						deadAt = time.Since(cutAt)
					}
				}
			}
		}
	}
	// drain, so every chunk of the file is committed
	for range 20 {
		if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
			return err
		}
		if st, err := r.Env.Status(ctx, gw); err == nil && st.Gateway != nil && st.Gateway.DirtyInodes == 0 {
			break
		}
	}
	if suspectAt < 0 { // the mark may have landed after the last piece
		if st, err := r.Env.Status(ctx, gw); err == nil {
			for i := range st.Nodes {
				if st.Nodes[i].Idx == victim.Idx && st.Nodes[i].Suspect {
					suspectAt = time.Since(cutAt)
				}
			}
		}
	}
	r.checkAt("I-170", name, suspectAt >= 0 && (deadAt < 0 || suspectAt <= deadAt),
		fmt.Sprintf("%s marked suspect after %s, dead after %s (-1: never during the write)", victim.Name, suspectAt, deadAt), p, "", gw.Name)
	reps, err := r.chunksOf(ctx, p)
	if err != nil {
		r.check("I-169", "chunk reports of the cut-off write", false, err.Error())
		return heal()
	}
	// the chunks committed well after the cut place around the victim: the ones in
	// flight at the cut (a piece's worth plus ChunkWorkers, 8) may still carry it or a 0
	// slot
	inFlight := hotpathPiece/r.Profile.ChunkSize + 8
	onVictim, late := 0, 0
	for i := range reps {
		if i <= cut+inFlight {
			continue
		}
		late++
		for _, s := range reps[i].Slots {
			if r.hostIdx(s.Node) == int(victim.Idx) {
				onVictim++
			}
		}
	}
	r.checkAt("I-169 I-170", "chunks after the cut place around the suspect holder", late > 0 && onVictim == 0,
		fmt.Sprintf("%d of %d chunks committed after the cut hold a slot on %s", onVictim, late, victim.Name), p, "", gw.Name)
	if err := heal(); err != nil {
		return err
	}
	if err := r.Env.Wait(ctx, cluster.ProbeInterval+time.Second); err != nil {
		return err
	}
	if st, err := r.Env.Status(ctx, gw); err == nil {
		for i := range st.Nodes {
			if st.Nodes[i].Idx == victim.Idx {
				r.checkAt("I-170", "the mark clears on the next good probe", !st.Nodes[i].Suspect && st.Nodes[i].Live, fmt.Sprintf("%s suspect %v live %v after the heal", victim.Name, st.Nodes[i].Suspect, st.Nodes[i].Live), "", "", gw.Name)
			}
		}
	}
	after, err := r.repairUntilClean(ctx)
	r.check("I-32", "degraded chunks of the cut-off write filled after the heal", err == nil && after.Volume.RepairPending == 0 && after.Volume.Degraded == 0,
		fmt.Sprintf("repair pending %d degraded %d, %v", after.Volume.RepairPending, after.Volume.Degraded, err))
	want, n, herr := v.Hash(p)
	_ = want
	r.checkAt("I-12", "the cut-off write reads back whole", herr == nil && n == int64(size), fmt.Sprintf("%d bytes, %v", n, herr), p, "", gw.Name)
	if err := v.Remove(p); err != nil {
		r.checkAt("I-81", "cut-off file removed", false, err.Error(), p, "", gw.Name)
	}
	return nil
}
