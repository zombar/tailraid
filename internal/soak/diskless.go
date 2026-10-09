package soak

import (
	"context"
	"fmt"
)

// --- 14 diskless host --------------------------------------------------------------------

// phaseDiskless exercises a host with no disks (docs/14 §9): a member and gateway that
// holds no slot and has no budget, which status flags diskless; a corpus written through
// it lands on the other hosts' disks, fully placed, and reads exact both there and from
// another gateway (I-118). Skipped on a rig where every host has a disk.
func phaseDiskless(ctx context.Context, r *Run) error {
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	var host NodeRef
	for _, n := range r.nodes {
		if nr, ok := reportNode(rep, n.Idx); ok && nr.Diskless && nr.State == "active" {
			host = n
			break
		}
	}
	if host.Idx == 0 {
		r.skip("no host is diskless")
		return nil
	}
	nr, _ := reportNode(rep, host.Idx)
	r.checkAt("I-118", "status reports the host diskless with no budget", nr.Diskless && nr.Capacity == 0 && nr.Live,
		fmt.Sprintf("diskless %v, capacity %d, live %v", nr.Diskless, nr.Capacity, nr.Live), "", "", host.Name)
	r.checkAt("I-118", "the diskless host holds no slot", hostSlots(rep, int(host.Idx)) == 0, fmt.Sprintf("%d slots", hostSlots(rep, int(host.Idx))), "", "", host.Name)
	v := r.volume(host)
	if v == nil {
		r.event("note", "the diskless host is not a gateway in this environment: nothing written through it")
		return nil
	}
	// a small corpus through the diskless gateway: sizes straddle the chunk size
	cs := r.Profile.ChunkSize
	dir := join(r.Root, "diskless")
	m := model{
		join(dir, "two.bin"):   r.randomBytes(2*cs + 17),
		join(dir, "chunk-1"):   r.randomBytes(cs - 1),
		join(dir, "small.txt"): []byte("written through a host with no disks\n"),
	}
	if err := r.writeModel(v, m); err != nil {
		r.checkAt("I-118", "writes through the diskless host accepted", false, err.Error(), "", "", host.Name)
		return nil
	}
	r.event("write", fmt.Sprintf("%d files through %s", len(m), host.Name))
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-118 I-12", "the corpus reads exact through the diskless host", bad == 0, first, first, "", host.Name)
	for _, gw := range r.gateways() {
		if gw.Idx == host.Idx {
			continue
		}
		bad, first = r.verifyModel(r.volume(gw), m)
		r.checkAt("I-118 I-12", "the corpus written through the diskless host reads exact from "+gw.Name, bad == 0, first, first, "", gw.Name)
		break
	}
	// where it landed: every slot filled, none on the diskless host
	chunks, missing, onHost, firstBad := 0, 0, 0, ""
	for _, p := range sortedPaths(m) {
		reps, err := r.chunksOf(ctx, p)
		if err != nil {
			r.checkAt("I-118", "chunk placement readable", false, err.Error(), p, "", "")
			return nil
		}
		for ci := range reps {
			c := &reps[ci]
			if !c.Found {
				continue
			}
			chunks++
			for _, s := range c.Slots {
				switch {
				case s.Node == 0:
					missing++
				case r.hostIdx(s.Node) == int(host.Idx) || s.Node == int(host.Idx):
					onHost++
					if firstBad == "" {
						firstBad = fmt.Sprintf("%s chunk %s: %s", p, c.Hash[:12], slotTable(*c))
					}
				}
			}
		}
	}
	r.check("I-118 I-9", "every chunk written through the diskless host is fully placed", chunks > 0 && missing == 0, fmt.Sprintf("%d chunks, %d missing slots", chunks, missing))
	r.checkAt("I-118", "no slot names the diskless host or a disk of it", onHost == 0, fmt.Sprintf("%d slots on it; %s", onHost, firstBad), "", "", host.Name)
	if rep, _, err = r.anyStatus(ctx); err != nil {
		return err
	}
	r.checkAt("I-118", "the diskless host still holds no slot after the writes", hostSlots(rep, int(host.Idx)) == 0, fmt.Sprintf("%d slots", hostSlots(rep, int(host.Idx))), "", "", host.Name)
	return nil
}
