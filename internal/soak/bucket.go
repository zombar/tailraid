package soak

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
)

// --- 15 s3 bucket ------------------------------------------------------------------------

// bucketName is the S3 node phase 15 registers, drains and removes.
const bucketName = "soak-bucket"

// bucketSlot is one slot of a phase-15 chunk that sits on the bucket.
type bucketSlot struct {
	path string
	c    *admin.ChunkReport
	idx  int // the slot index
}

// object is the slot's object name under the bucket's prefix (docs/10 §4.2).
func (s bucketSlot) object() (string, error) {
	raw, err := hex.DecodeString(s.c.Hash)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("chunk hash %q", s.c.Hash)
	}
	return rpc.ShardObject(chunk.Key([32]byte(raw), s.c.K, s.c.M, s.idx)), nil
}

// phaseBucket exercises an S3 bucket as a storage node (docs/10 §4) on a rig that offers
// one: registered once and live on every gateway, a corpus placed on it in the disk
// layout, a read with a data holder down served from it, an object deleted behind the
// cluster's back put back by the scrub, GC and the drain leaving its objects alone, and
// the bucket drained and removed again at the end.
func phaseBucket(ctx context.Context, r *Run) error {
	caps := r.Env.Caps()
	if !caps.S3 {
		r.skip("no bucket in this environment")
		return nil
	}
	u := r.undoer(ctx)
	defer u.run()
	var (
		b      *s3.Bucket
		bucket NodeRef
		rep    admin.Report
		err    error
	)
	if caps.AddS3 {
		if !caps.Mutate {
			r.skip("adding an S3 node changes the cluster: needs --destructive")
			return nil
		}
		if b, err = r.Env.Bucket(ctx); err != nil {
			r.check("I-52", "the rig's bucket reachable from the driver", false, err.Error())
			return nil
		}
		if err := r.Env.AddS3(ctx, bucketName); err != nil {
			r.check("I-137", "add-s3 accepted", false, err.Error())
			return nil
		}
		r.event("add-s3", bucketName)
		if rep, _, err = r.anyStatus(ctx); err != nil {
			return err
		}
		nr, ok := reportNodeByName(rep, bucketName)
		if !ok || nr.Kind != "s3" {
			r.check("I-137", "the bucket is in the node table", false, fmt.Sprintf("row %+v", nr))
			return nil
		}
		bucket = NodeRef{Name: bucketName, Idx: nr.Idx}
		u.add("drain and remove "+bucketName, func(c context.Context) error { _, err := r.removeBucket(c, bucket); return err })
		r.bucketLive(ctx, bucket)
		// the leader's refusal names the row (cluster.addS3); any other error, a timeout
		// above all, is a failure of its own, never a pass
		err = r.Env.AddS3(ctx, bucketName+"-again")
		refused := err != nil && strings.Contains(err.Error(), "already S3 node")
		r.check("I-137", "a second S3 node over the same bucket is refused", refused, fmt.Sprint(err))
	} else {
		// a live cluster: its own S3 node, if this device holds the key (docs/09 §6)
		if rep, _, err = r.anyStatus(ctx); err != nil {
			return err
		}
		nr, ok := reportS3Node(rep)
		if !ok {
			r.skip("the cluster has no S3 node")
			return nil
		}
		if b, err = r.Env.Bucket(ctx); err != nil {
			r.skip(err.Error())
			return nil
		}
		bucket = NodeRef{Name: nr.Name, Idx: nr.Idx}
		r.checkSkipped("I-137", "a second S3 node over the same bucket is refused", "the cluster's own bucket is not re-registered")
		r.bucketLive(ctx, bucket)
	}

	// a corpus written while the bucket is a target
	gws := r.gateways()
	gw := gws[0]
	v := r.volume(gw)
	cs := r.Profile.ChunkSize
	dir := join(r.Root, "bucket")
	m := model{}
	for i := range 8 {
		m[join(dir, fmt.Sprintf("f%d.bin", i))] = r.randomBytes(2*cs + 31*i)
	}
	if err := r.writeModel(v, m); err != nil {
		r.checkAt("I-7", "writes with a bucket among the targets accepted", false, err.Error(), "", "", gw.Name)
		return nil
	}
	r.event("write", fmt.Sprintf("%d files through %s", len(m), gw.Name))
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	for _, g := range gws {
		bad, first := r.verifyModel(r.volume(g), m)
		r.checkAt("I-12", "the corpus reads exact through "+g.Name, bad == 0, first, first, "", g.Name)
	}
	slots, err := r.bucketSlots(ctx, m, bucket)
	if err != nil {
		r.check("I-52", "chunk placement readable", false, err.Error())
		return nil
	}
	r.check("I-52", "chunks are placed on the bucket", len(slots) > 0, fmt.Sprintf("%d slots on %s", len(slots), bucketName))
	if len(slots) == 0 {
		return nil
	}
	missing, firstBad := 0, ""
	for _, s := range slots {
		name, err := s.object()
		if err == nil {
			var raw []byte
			if raw, err = b.Get(ctx, name, chunk.HeaderSize+chunk.MaxPayload); err == nil {
				_, err = chunk.Unframe(raw)
			}
		}
		if err != nil {
			missing++
			if firstBad == "" {
				firstBad = fmt.Sprintf("%s chunk %s slot %d: %v", s.path, s.c.Hash[:12], s.idx, err)
			}
		}
	}
	r.check("I-52", "every slot on the bucket is its shard, in the disk layout and framing", missing == 0, fmt.Sprintf("%d of %d bad; %s", missing, len(slots), firstBad))

	if caps.Kill && len(gws) > 1 {
		if err := r.bucketDegradedRead(ctx, slots, m, u); err != nil {
			return err
		}
		v = r.volume(gw) // the victim may have been this gateway: a restarted node has a new volume
	} else {
		r.checkSkipped("L-104", "a read with a data holder down is served from the bucket", "needs kills and a second gateway")
	}

	// an object deleted behind the cluster's back: the scrub's HEAD finds it, repair puts
	// it back (docs/06 §5). On a live cluster that is a delete in the operator's bucket,
	// of this run's own file: only with --destructive.
	victim := slots[0]
	if caps.Corrupt {
		name, _ := victim.object()
		if err := b.Delete(ctx, name); err != nil {
			r.check("L-139", "an object deleted behind the cluster's back", false, err.Error())
			return nil
		}
		r.event("delete object", b.Key(name))
		st, err := r.scrub(ctx)
		if err != nil {
			r.check("L-112 L-139", "scrub with an object missing from the bucket", false, err.Error())
			return nil
		}
		back, err := b.Has(ctx, name)
		r.check("L-112 L-139", "the scrub finds the missing object and repair puts it back", st.ScrubBad > 0 && back && err == nil,
			fmt.Sprintf("scrub %d bad, %d repaired; object back %v %v", st.ScrubBad, st.Repaired, back, err))
	} else {
		r.checkSkipped("L-112 L-139", "the scrub finds the missing object and repair puts it back", "deleting from the cluster's bucket needs --destructive")
	}

	// GC collects a removed file's chunks without a DELETE to the bucket (I-138)
	gone := victim.path
	var goneObjects []string
	for _, s := range slots {
		if s.path == gone {
			if n, err := s.object(); err == nil {
				goneObjects = append(goneObjects, n)
			}
		}
	}
	if err := v.Remove(gone); err != nil {
		return err
	}
	delete(m, gone)
	r.event("remove", gone)
	if err := r.Env.Wait(ctx, r.Profile.Grace+time.Second); err != nil {
		return err
	}
	res, err := r.gcNow(ctx)
	if err != nil {
		return err
	}
	kept := r.objectsPresent(ctx, b, goneObjects)
	r.check("I-138", "gc collects a file on the bucket without deleting its objects", res.Victims > 0 && kept == len(goneObjects),
		fmt.Sprintf("gc %d victims; %d of %d objects still in the bucket (the leader's daily sweep removes them)", res.Victims, kept, len(goneObjects)))

	if !caps.AddS3 {
		r.checkSkipped("I-42", "the bucket drains and is removed", "the cluster's own bucket stays")
		bad, first := r.verifyModel(v, m)
		r.checkAt("I-12", "the corpus reads exact with the bucket among the targets", bad == 0, first, first, "", gw.Name)
		return nil
	}
	// drain and remove: the repairer moves every slot off the bucket and still sends it no
	// DELETE; the removed row stays in the table for its history
	var stale []string
	for _, s := range slots {
		if s.path != gone {
			if n, err := s.object(); err == nil {
				stale = append(stale, n)
			}
		}
	}
	rep, err = r.removeBucket(ctx, bucket)
	u.done("drain and remove " + bucketName)
	if err != nil {
		r.checkAt("I-42", "the bucket drains and is removed", false, err.Error(), "", "", bucketName)
		return nil
	}
	state := ""
	if nr, ok := reportNode(rep, bucket.Idx); ok {
		state = nr.State
	}
	r.checkAt("I-42", "the drained bucket is out of the node table", state == "removed", "state "+state, "", "", bucketName)
	kept = r.objectsPresent(ctx, b, stale)
	r.check("I-138", "the drain moved the slots without deleting the bucket's objects", kept == len(stale), fmt.Sprintf("%d of %d objects left for the sweep", kept, len(stale)))
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-12", "the corpus reads exact after the bucket left", bad == 0, first, first, "", gw.Name)
	return nil
}

// reportS3Node is the first Active S3 row of rep.
func reportS3Node(rep admin.Report) (admin.NodeReport, bool) {
	for i := range rep.Nodes {
		if n := &rep.Nodes[i]; n.Kind == "s3" && n.State == "active" {
			return *n, true
		}
	}
	return admin.NodeReport{}, false
}

// bucketLive waits up to a few S3 probe intervals for every gateway to see the bucket
// live (docs/10 §4.3).
func (r *Run) bucketLive(ctx context.Context, bucket NodeRef) {
	var notLive []string
	for range 5 {
		notLive = notLive[:0]
		for _, n := range r.gateways() {
			rep, err := r.Env.Status(ctx, n)
			if nr, ok := reportNode(rep, bucket.Idx); err != nil || !ok || !nr.Live {
				notLive = append(notLive, n.Name)
			}
		}
		if len(notLive) == 0 {
			break
		}
		if err := r.Env.Wait(ctx, cluster.S3ProbeInterval); err != nil {
			break
		}
	}
	r.checkAt("L-104", "the bucket is live on every gateway", len(notLive) == 0, fmt.Sprintf("not live on %v", notLive), "", "", bucket.Name)
}

// bucketSlots lists the slots of m's chunks that sit on the bucket.
func (r *Run) bucketSlots(ctx context.Context, m model, bucket NodeRef) ([]bucketSlot, error) {
	var out []bucketSlot
	for _, p := range sortedPaths(m) {
		reps, err := r.chunksOf(ctx, p)
		if err != nil {
			return nil, err
		}
		for ci := range reps {
			c := &reps[ci]
			if !c.Found {
				continue
			}
			for _, s := range c.Slots {
				if s.Node == int(bucket.Idx) {
					out = append(out, bucketSlot{path: p, c: c, idx: s.Idx})
				}
			}
		}
	}
	return out, nil
}

// bucketDegradedRead kills the host of another slot of a chunk on the bucket, so that
// chunk has fewer than k slots on devices, and reads the file through a gateway that
// still runs: the read can only be exact by fetching from the bucket (L-104).
func (r *Run) bucketDegradedRead(ctx context.Context, slots []bucketSlot, m model, u *undo) error {
	for _, s := range slots {
		for _, other := range s.c.Slots {
			if other.Idx == s.idx || other.Node == 0 {
				continue
			}
			victim, ok := r.nodeByIdx(other.Node)
			if !ok {
				continue
			}
			var gw NodeRef
			for _, g := range r.gateways() {
				if g.Idx != victim.Idx {
					gw = g
					break
				}
			}
			if gw.Idx == 0 || len(s.c.Slots)-2 >= int(s.c.K) {
				continue // with the victim down it still has k device slots: no need for the bucket
			}
			if err := r.Env.Kill(ctx, victim); err != nil {
				return err
			}
			u.add("start "+victim.Name, func(c context.Context) error { return r.Env.Start(c, victim) })
			r.event("kill", victim.Name)
			// at once, inside the window before the prober marks the holder dead: the
			// reader falls through to the bucket within the request (I-149)
			got, err := r.volume(gw).ReadAll(s.path)
			exact := err == nil && bytes.Equal(got, m[s.path])
			r.checkAt("I-149 L-104", "a read right after the holder died falls through to the bucket", exact,
				fmt.Sprintf("%s with %s just killed: %v", s.path, victim.Name, err), s.path, s.c.Hash[:12], gw.Name)
			if err := r.Env.Wait(ctx, cluster.LiveThreshold+cluster.ProbeInterval); err != nil {
				return err
			}
			got, err = r.volume(gw).ReadAll(s.path)
			exact = err == nil && bytes.Equal(got, m[s.path])
			r.checkAt("L-104 I-12", "a read with a data holder down is served from the bucket", exact,
				fmt.Sprintf("%s with %s down: %v", s.path, victim.Name, err), s.path, s.c.Hash[:12], gw.Name)
			if err := r.Env.Start(ctx, victim); err != nil {
				return err
			}
			u.done("start " + victim.Name)
			r.event("start", victim.Name)
			healthy, detail := r.waitHealthy(ctx, 2*time.Minute) // the victim may have led: the rest of the phase needs a leader
			r.check("I-6", "the cluster is healthy again after the holder returned", healthy, detail)
			return nil
		}
	}
	r.checkSkipped("L-104", "a read with a data holder down is served from the bucket", "no chunk on the bucket with a killable holder")
	return nil
}

// objectsPresent counts which of names are still in the bucket.
func (r *Run) objectsPresent(ctx context.Context, b *s3.Bucket, names []string) int {
	n := 0
	for _, name := range names {
		if has, err := b.Has(ctx, name); err == nil && has {
			n++
		}
	}
	return n
}

// removeBucket drains the bucket, repairs until its slots have moved, and removes it.
func (r *Run) removeBucket(ctx context.Context, bucket NodeRef) (admin.Report, error) {
	if err := r.Env.SetState(ctx, bucket, meta.Draining); err != nil {
		return admin.Report{}, fmt.Errorf("drain %s: %w", bucket.Name, err)
	}
	r.event("drain", bucket.Name)
	rep, err := r.repairUntilClean(ctx)
	if err != nil {
		return rep, err
	}
	if left := hostSlotsOf(rep, bucket.Name); left > 0 {
		return rep, fmt.Errorf("%d slots still on %s after the drain", left, bucket.Name)
	}
	if err := r.Env.Remove(ctx, bucket); err != nil {
		return rep, fmt.Errorf("remove %s: %w", bucket.Name, err)
	}
	r.event("remove", bucket.Name)
	rep, _, err = r.anyStatus(ctx)
	return rep, err
}
