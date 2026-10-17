package soak

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
)

type phase struct {
	name string
	run  func(ctx context.Context, r *Run) error
}

// phases is the sequence of docs/09 §6, in order.
var phases = []phase{
	{"1 baseline transfer", phaseBaseline},
	{"2 cross-view integrity", phaseIntegrity},
	{"3 random-access churn", phaseChurn},
	{"4 parity", phaseParity},
	{"5 corruption", phaseCorruption},
	{"6 consensus", phaseConsensus},
	{"7 membership", phaseMembership},
	{"8 dedup and gc", phaseDedupGC},
	{"9 restart storm", phaseRestartStorm},
	{"10 partition", phasePartition},
	{"11 automation", phaseAutomation},
	{"12 large file", phaseLargeFile},
	{"13 disks", phaseDisks},
	{"14 diskless host", phaseDiskless},
	{"15 s3 bucket", phaseBucket},
	{"16 web ui", phaseWeb},
	{"17 hot path", phaseHotPath},
	{"18 repair fan-out", phaseFanOut},
	{"19 snapshots", phaseSnapshots},
	{"20 stats", phaseStats},
	{"21 search and media", phaseSearch},
	{"22 copies under faults", phaseCopies},
	{"23 many users at once", phaseMultiUser},
	{"24 secrets", phaseSecrets},
	{"33 stage", phaseStage},
	{"32 restore jobs", phaseRestoreJobs}, // before 28: its starts are the env's own ops, which sign only in a users-first run
	{"28 users and keys", phaseUsers},
	{"29 grants on the gateways", phaseGrants},
	{"30 secrets per user", phaseUserSecrets},
	{"31 audit and sessions", phaseAudit},
}

// model is the client's belief about the corpus: path → content.
type model map[string][]byte

func (r *Run) randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = r.Rng.Read(b)
	return b
}

// corpus builds the baseline file set under dir (sizes straddle the chunk size).
func (r *Run) corpus(dir string) model {
	cs := r.Profile.ChunkSize
	m := model{}
	m[join(dir, "big.bin")] = r.randomBytes(r.Profile.BigFile)
	m[join(dir, "empty")] = []byte{}
	m[join(dir, "one")] = r.randomBytes(1)
	m[join(dir, "b4095")] = r.randomBytes(4095)
	for _, d := range []int{-1, 0, 1} {
		m[join(dir, fmt.Sprintf("chunk%+d", d))] = r.randomBytes(cs + d)
	}
	sparse := make([]byte, 2*cs+123)
	copy(sparse, r.randomBytes(100))
	copy(sparse[len(sparse)-100:], r.randomBytes(100))
	m[join(dir, "sparse.bin")] = sparse
	for i := range r.Profile.SmallFiles {
		m[join(dir, "small", fmt.Sprintf("f%03d.txt", i))] = []byte(fmt.Sprintf("file %d\n", i))
	}
	return m
}

func sortedPaths(m model) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// writeModel stores every file of m through v (sparse files get head and tail only).
func (r *Run) writeModel(v Volume, m model) error {
	dirs := map[string]bool{}
	for _, p := range sortedPaths(m) {
		d := join(p, "..")
		if !dirs[d] {
			if err := v.Mkdir(d); err != nil {
				return fmt.Errorf("mkdir %s: %w", d, err)
			}
			dirs[d] = true
		}
		data := m[p]
		if len(data) > 300 && isZero(data[100:len(data)-100]) { // sparse: write head and tail
			if err := v.WriteAt(p, 0, data[:100]); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
			if err := v.WriteAt(p, int64(len(data)-100), data[len(data)-100:]); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
			continue
		}
		if err := v.WriteAt(p, 0, data); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}
	return nil
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// verifyModel reads every file of m through v; returns mismatches as (path, detail).
func (r *Run) verifyModel(v Volume, m model) (bad int, first string) {
	for _, p := range sortedPaths(m) {
		got, err := v.ReadAll(p)
		switch {
		case err != nil:
			bad++
			if first == "" {
				first = fmt.Sprintf("%s: %v", p, err)
			}
		case !bytes.Equal(got, m[p]):
			bad++
			if first == "" {
				first = fmt.Sprintf("%s: differs at %d (len %d want %d)", p, firstDiff(got, m[p]), len(got), len(m[p]))
			}
		}
	}
	return bad, first
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// corpusModel is shared between phases (written in phase 1).
func (r *Run) corpusModel() model {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shared == nil {
		r.shared = map[string]any{}
	}
	m, _ := r.shared["corpus"].(model)
	return m
}

func (r *Run) setShared(k string, v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shared == nil {
		r.shared = map[string]any{}
	}
	r.shared[k] = v
}

// leaderRef returns the current leader as a NodeRef (falls back to the first node).
func (r *Run) leaderRef(ctx context.Context) NodeRef {
	if rep, _, err := r.anyStatus(ctx); err == nil {
		for i := range rep.Nodes {
			if rep.Nodes[i].Leader {
				if n, ok := r.nodeByIdx(int(rep.Nodes[i].Idx)); ok {
					return n
				}
			}
		}
	}
	return r.nodes[0]
}

// chunksOf asks the leader (whose FSM is never behind a SetPlacement) for a file's chunks.
func (r *Run) chunksOf(ctx context.Context, path string) ([]admin.ChunkReport, error) {
	return r.Env.Chunks(ctx, r.leaderRef(ctx), path)
}

// checkFSMAgreement records the FSM-hash comparison, or a note when no node serves
// /dev/fsm-hash (live clusters without dev.enabled).
func (r *Run) checkFSMAgreement(ctx context.Context, cites, name string) {
	agree, detail, available := r.fsmAgreementSettled(ctx)
	if !available {
		r.event("note", name+": skipped, "+detail)
		return
	}
	r.check(cites, name, agree, detail)
}

// waitHealthy polls status until the cluster is healthy with every node live (except
// removed rows, mirrors, which are never probed as targets and may be offline at will
// (docs/16 §1), and the targets named in excused, such as a disk the phase closed); the
// detail describes the last view.
func (r *Run) waitHealthy(ctx context.Context, timeout time.Duration, excused ...string) (bool, string) {
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		rep, from, err := r.anyStatus(ctx)
		detail := fmt.Sprintf("status unavailable: %v", err)
		if err == nil {
			var down []string
			for i := range rep.Nodes {
				if !rep.Nodes[i].Live && rep.Nodes[i].State != "removed" && rep.Nodes[i].Kind != meta.KindMirror.String() && !slices.Contains(excused, rep.Nodes[i].Name) {
					down = append(down, rep.Nodes[i].Name)
				}
			}
			if rep.Health == "healthy" && len(down) == 0 {
				return true, fmt.Sprintf("healthy, %d nodes live (via %s)", len(rep.Nodes), from)
			}
			detail = fmt.Sprintf("health %s, down %v (via %s)", rep.Health, down, from)
		}
		if time.Now().After(deadline) || r.Env.Wait(ctx, 500*time.Millisecond) != nil {
			return false, detail
		}
	}
}

// repairUntilClean runs repair passes until one finds nothing to do (or 8 passes).
func (r *Run) repairUntilClean(ctx context.Context) (admin.Report, error) {
	for range 64 { // a pass that moves nothing ends it, unless the home gate just opened
		st, err := r.repairNow(ctx)
		if err != nil {
			return admin.Report{}, err
		}
		r.lastRepair = st
		if st.Repaired+st.Partial+st.Migrated+st.Deferred+st.Homed == 0 && !st.HomeOpen {
			break
		}
		if err := r.Env.Wait(ctx, 200*time.Millisecond); err != nil {
			return admin.Report{}, err
		}
	}
	rep, _, err := r.anyStatus(ctx)
	return rep, err
}

// --- 1 baseline ---------------------------------------------------------------------------

func phaseBaseline(ctx context.Context, r *Run) error {
	gw := r.gateways()[0]
	v := r.volume(gw)
	m := r.corpus(join(r.Root, "corpus"))
	before := r.snapshotCounters()
	t := time.Now()
	err := r.writeModel(v, m)
	took := time.Since(t)
	after := r.snapshotCounters()
	r.check("I-7", "every write acknowledged", err == nil, fmt.Sprintf("%d files via %s", len(m), gw.Name))
	if err != nil {
		return err
	}
	mb := float64(after.BytesWritten-before.BytesWritten) / (1 << 20)
	r.event("baseline written", fmt.Sprintf("%.1f MiB in %s (%.1f MiB/s) via %s", mb, took.Round(time.Millisecond), mb/took.Seconds(), gw.Name))
	bad, first := r.verifyModel(v, m)
	r.check("I-34 L-72", "read-your-writes on the writing gateway", bad == 0, first)
	r.setShared("corpus", m)
	return r.Env.Wait(ctx, r.Profile.FlushIdle)
}

// --- 2 integrity ---------------------------------------------------------------------------

func phaseIntegrity(ctx context.Context, r *Run) error {
	m := r.corpusModel()
	for _, gw := range r.gateways() {
		v := r.volume(gw)
		bad, first := r.verifyModel(v, m)
		r.checkAt("I-12 I-36 L-51", "corpus identical via "+gw.Name, bad == 0, fmt.Sprintf("%d of %d files differ: %s", bad, len(m), first), first, "", gw.Name)
	}
	for _, n := range r.nodes {
		res, err := r.Env.Fsck(ctx, n)
		if err != nil {
			r.checkAt("I-44", "fsck on "+n.Name, false, err.Error(), "", "", n.Name)
			continue
		}
		r.checkAt("I-44", "fsck on "+n.Name, res.OK, res.Detail, "", "", n.Name)
	}
	r.checkFSMAgreement(ctx, "I-4", "FSM hashes agree")
	reps, err := r.chunksOf(ctx, join(r.Root, "corpus", "big.bin"))
	if err != nil {
		return err
	}
	full := true
	for i := range reps {
		c := &reps[i]
		if c.LiveSlots != len(c.Slots) || c.State != "committed" {
			full = false
			r.checkAt("I-9 L-39 L-51", "chunk fully placed", false, fmt.Sprintf("%s %d/%d live slots", c.State, c.LiveSlots, len(c.Slots)), "big.bin", c.Hash, "")
		}
	}
	r.check("I-9 L-39 L-51", "big.bin fully placed on live nodes", full, fmt.Sprintf("%d chunks", len(reps)))
	return nil
}

// --- 3 churn -------------------------------------------------------------------------------

func phaseChurn(ctx context.Context, r *Run) error {
	gws := r.gateways()
	writers := gws
	if len(writers) > 2 {
		writers = writers[:2]
	}
	cs := r.Profile.ChunkSize
	all := model{}
	for wi, gw := range writers {
		v := r.volume(gw)
		dir := join(r.Root, "churn", gw.Name)
		if err := v.Mkdir(dir); err != nil {
			return err
		}
		m := model{}
		names := make([]string, 4)
		for i := range names {
			names[i] = join(dir, fmt.Sprintf("f%d", i))
		}
		errs := 0
		for range r.Profile.ChurnOps {
			name := names[r.Rng.Intn(len(names))]
			cur := m[name]
			switch x := r.Rng.Intn(10); {
			case x < 6: // write
				var off int
				switch r.Rng.Intn(4) {
				case 0:
					off = 0
				case 1:
					off = len(cur)
				case 2:
					off = len(cur) + r.Rng.Intn(cs)
				default:
					if len(cur) > 0 {
						off = r.Rng.Intn(len(cur))
					}
				}
				data := r.randomBytes(1 + r.Rng.Intn(cs))
				if err := v.WriteAt(name, int64(off), data); err != nil {
					errs++
					continue
				}
				if end := off + len(data); end > len(cur) {
					cur = append(cur, make([]byte, end-len(cur))...)
				}
				copy(cur[off:], data)
				m[name] = cur
			case x < 8: // truncate
				if cur == nil {
					continue
				}
				size := r.Rng.Intn(len(cur) + cs)
				if err := v.Truncate(name, int64(size)); err != nil {
					errs++
					continue
				}
				if size < len(cur) {
					cur = cur[:size]
				} else {
					cur = append(cur, make([]byte, size-len(cur))...)
				}
				m[name] = cur
			case x < 9: // rename over another name
				to := names[r.Rng.Intn(len(names))]
				if to == name || cur == nil {
					continue
				}
				if err := v.Rename(name, to); err != nil {
					errs++
					continue
				}
				m[to] = cur
				delete(m, name)
			default: // remove
				if cur == nil {
					continue
				}
				if err := v.Remove(name); err != nil {
					errs++
					continue
				}
				delete(m, name)
			}
		}
		r.check("I-45 L-2 L-3 L-5 L-6", "churn ops acknowledged via "+gw.Name, errs == 0, fmt.Sprintf("%d of %d ops failed", errs, r.Profile.ChurnOps))
		maps.Copy(all, m)
		_ = wi
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	reader := gws[len(gws)-1]
	v := r.volume(reader)
	bad, first := r.verifyModel(v, all)
	r.checkAt("I-12 I-36 L-21", "churn result identical via "+reader.Name, bad == 0, fmt.Sprintf("%d files differ: %s", bad, first), first, "", reader.Name)
	for p := range all {
		r.corpusModel()[p] = all[p] // keep verifying them in later phases
	}
	return nil
}

// --- 4 parity ------------------------------------------------------------------------------

func phaseParity(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Kill {
		r.skip("environment cannot kill nodes")
		return nil
	}
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	m := r.corpusModel()
	k, mm := int(rep.Volume.K), int(rep.Volume.M)
	// victims: the last hosts that hold slots, never the gateway we read through; a
	// diskless host holds nothing, so losing it degrades no chunk (docs/14 §9)
	gw := r.gateways()[0]
	var victims []NodeRef
	holders := 0
	for _, n := range slices.Backward(r.nodes) {
		if hostSlots(rep, int(n.Idx)) == 0 {
			continue
		}
		holders++
		if n.Idx != gw.Idx && len(victims) < mm+1 {
			victims = append(victims, n)
		}
	}
	if len(victims) < mm+1 || holders < k+mm+1 {
		r.skip(fmt.Sprintf("need %d hosts with slots besides the gateway to lose m+1, have %d", mm+1, len(victims)))
		return nil
	}
	v := r.volume(gw)
	u := r.undoer(ctx)
	defer u.run()
	// lose m
	for _, n := range victims[:mm] {
		if err := r.Env.Kill(ctx, n); err != nil {
			return err
		}
		u.add("start "+n.Name, func(c context.Context) error { return r.Env.Start(c, n) })
		r.event("kill", n.Name)
	}
	if err := r.Env.Wait(ctx, 2*time.Second); err != nil {
		return err
	}
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-12 L-64 L-131", fmt.Sprintf("reads exact with m=%d nodes dead", mm), bad == 0, first, first, "", gw.Name)
	during := r.randomBytes(r.Profile.ChunkSize + 77)
	p := join(r.Root, "parity", "during-loss.bin")
	werr := v.Mkdir(join(r.Root, "parity"))
	if werr == nil {
		werr = v.WriteAt(p, 0, during)
	}
	r.check("L-41 L-52", "writes accepted with m nodes dead (degraded)", werr == nil, fmt.Sprint(werr))
	if werr == nil {
		m[p] = during
	}
	// liveness takes LiveThreshold to notice the loss; then status must show degraded chunks
	if err := r.Env.Wait(ctx, max(r.Profile.FlushIdle, cluster.LiveThreshold+time.Second)); err != nil {
		return err
	}
	if rep, _, err = r.anyStatus(ctx); err == nil {
		r.check("I-56 L-86", "status reports degraded chunks while a holder is dead", rep.Volume.Degraded > 0, fmt.Sprintf("%d degraded", rep.Volume.Degraded))
	}
	// the dead node returns: after fill_after a repair pass fills what was written meanwhile
	for _, n := range victims[:mm] {
		if err := r.Env.Start(ctx, n); err != nil {
			return err
		}
		u.done("start " + n.Name)
		r.event("start", n.Name)
	}
	healthy, hdetail := r.waitHealthy(ctx, r.Profile.RestartSettle+30*time.Second)
	r.check("L-88 L-132", "cluster healthy after the node returned", healthy, hdetail)
	if err := r.Env.Wait(ctx, r.Profile.FillAfter+time.Second); err != nil {
		return err
	}
	if rep, err = r.repairUntilClean(ctx); err != nil {
		return err
	}
	r.check("I-15 L-87 L-89", "degraded chunks back to 0 after repair", rep.Volume.Degraded == 0, fmt.Sprintf("%d degraded, %d unreadable", rep.Volume.Degraded, rep.Volume.Unreadable))
	// lose m+1: every read is exact or an error, never garbage
	for _, n := range victims {
		if err := r.Env.Kill(ctx, n); err != nil {
			return err
		}
		u.add("start "+n.Name, func(c context.Context) error { return r.Env.Start(c, n) })
		r.event("kill", n.Name)
	}
	if err := r.Env.Wait(ctx, 2*time.Second); err != nil {
		return err
	}
	garbage, errs := 0, 0
	for _, path := range sortedPaths(m) {
		got, err := v.ReadAll(path)
		switch {
		case err != nil:
			errs++
		case !bytes.Equal(got, m[path]):
			garbage++
			r.checkAt("I-40 L-65", "read returned wrong bytes with m+1 nodes dead", false, path, path, "", gw.Name)
		}
	}
	r.check("I-40 L-65", fmt.Sprintf("no garbage with m+1=%d nodes dead", mm+1), garbage == 0, fmt.Sprintf("%d reads failed cleanly, %d exact", errs, len(m)-errs-garbage))
	for _, n := range victims {
		if err := r.Env.Start(ctx, n); err != nil {
			return err
		}
		u.done("start " + n.Name)
		r.event("start", n.Name)
	}
	healthy, hdetail = r.waitHealthy(ctx, r.Profile.RestartSettle+30*time.Second)
	r.check("L-132", "cluster healthy after all nodes returned", healthy, hdetail)
	if rep, err = r.repairUntilClean(ctx); err != nil {
		return err
	}
	r.check("I-15 L-88", "no degraded chunks after recovery", rep.Volume.Degraded == 0 && rep.Volume.Unreadable == 0, fmt.Sprintf("%d degraded", rep.Volume.Degraded))
	bad, first = r.verifyModel(v, m)
	r.checkAt("I-12", "corpus exact after recovery", bad == 0, first, first, "", gw.Name)
	return nil
}

// --- 5 corruption --------------------------------------------------------------------------

func shardKey(c admin.ChunkReport, slot int) (string, error) {
	hb, err := hex.DecodeString(c.Hash)
	if err != nil || len(hb) != 32 {
		return "", fmt.Errorf("bad hash %q", c.Hash)
	}
	var h [32]byte
	copy(h[:], hb)
	return string(chunk.Key(h, c.K, c.M, slot)), nil
}

// presentSlots lists the slots whose live node answered HEAD with the shard present.
func presentSlots(c admin.ChunkReport) []int {
	var out []int
	for i, s := range c.Slots {
		if s.Node != 0 && s.Live && s.Present != nil && *s.Present {
			out = append(out, i)
		}
	}
	return out
}

// liveSlots counts the slots placed on a live node.
func liveSlots(c admin.ChunkReport) int {
	n := 0
	for _, s := range c.Slots {
		if s.Node != 0 && s.Live {
			n++
		}
	}
	return n
}

// slotTable prints a chunk's slots for a check's detail: "0:n3 live present, 1:n1 live absent".
func slotTable(c admin.ChunkReport) string {
	if len(c.Hash) < 12 {
		return fmt.Sprintf("chunk %q: malformed hash", c.Hash)
	}
	parts := make([]string, 0, len(c.Slots))
	for i, s := range c.Slots {
		state := "missing"
		if s.Node != 0 {
			state = fmt.Sprintf("node %d", s.Node)
			switch {
			case !s.Live:
				state += " dead"
			case s.Present == nil:
				state += " unchecked"
			case *s.Present:
				state += " present"
			default:
				state += " ABSENT"
			}
		}
		parts = append(parts, fmt.Sprintf("%d:%s", i, state))
	}
	return fmt.Sprintf("%s %d+%d [%s]", c.Hash[:12], c.K, c.M, strings.Join(parts, ", "))
}

func (r *Run) nodeByIdx(idx int) (NodeRef, bool) {
	host := r.hostIdx(idx) // a slot on a disk is served by the disk's host (docs/14)
	for _, n := range r.nodes {
		if int(n.Idx) == host {
			return n, true
		}
	}
	return NodeRef{}, false
}

// storedDelta is how many bytes the volume grew by between two status samples; negative
// when GC or a migration shrank it meanwhile (an unsigned subtraction would wrap).
func storedDelta(before, after uint64) int64 { return int64(after) - int64(before) }

// corruptionVerdict judges the m+1 corruption read. With fewer than m+1 shards corrupted
// the check is void and fails loudly instead of passing by accident. A reader without
// the chunk in its cache must get an error; a reader that may have it cached may also
// serve the exact bytes. Wrong bytes are never acceptable.
func corruptionVerdict(corrupted, m int, coldReader bool, err error, exact bool) (bool, string) {
	switch {
	case corrupted <= m:
		return false, fmt.Sprintf("only %d of the required m+1=%d shards could be corrupted", corrupted, m+1)
	case err != nil:
		return true, fmt.Sprintf("read failed cleanly: %v", err)
	case !exact:
		return false, "read returned wrong bytes"
	case coldReader:
		return false, "a gateway without the chunk cached served it with m+1 shards corrupt"
	}
	return true, "served exact from the gateway's cache"
}

func phaseCorruption(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Corrupt {
		r.skip("environment cannot corrupt shards")
		return nil
	}
	gw := r.gateways()[0]
	v := r.volume(gw)
	m := r.corpusModel()
	// one shard (≤ m) of a committed chunk: reads stay exact, scrub repairs it
	target := join(r.Root, "corpus", "big.bin")
	var c admin.ChunkReport
	var holder NodeRef
	var key string
	var cerr error
	slot := -1
	for range 3 { // a repair may move the slot between lookup and corrupt
		reps, err := r.chunksOf(ctx, target)
		if err != nil || len(reps) == 0 {
			return fmt.Errorf("chunks of %s: %w", target, err)
		}
		c = reps[0]
		// the describe checks each live slot with HEAD: a slot named by the placement but
		// absent from its live node is a finding of its own, not a reason to abort
		present := presentSlots(c)
		r.checkAt("I-13 L-111", "every live slot of the committed chunk holds its shard", len(present) == liveSlots(c), slotTable(c), target, c.Hash, "")
		if len(present) == 0 {
			return fmt.Errorf("no present slot to corrupt: %s", slotTable(c))
		}
		slot = present[0]
		if key, err = shardKey(c, slot); err != nil {
			return err
		}
		var ok bool
		if holder, ok = r.nodeByIdx(c.Slots[slot].Node); !ok {
			return fmt.Errorf("slot %d of %s on unknown node %d", slot, c.Hash, c.Slots[slot].Node)
		}
		if cerr = r.Env.Corrupt(ctx, holder, key); cerr == nil {
			break
		}
		if err := r.Env.Wait(ctx, time.Second); err != nil {
			return err
		}
	}
	if cerr != nil {
		return fmt.Errorf("corrupt %s slot %d on %s: %w (%s)", c.Hash[:12], slot, holder.Name, cerr, slotTable(c))
	}
	r.event("corrupt", fmt.Sprintf("%s slot %d (target %d) on %s", c.Hash[:12], slot, c.Slots[slot].Node, holder.Name))
	got, err := v.ReadAll(target)
	r.checkAt("I-40 L-68 L-105", "read exact with m shards corrupt", err == nil && bytes.Equal(got, m[target]), fmt.Sprint(err), target, c.Hash, holder.Name)
	st, err := r.scrub(ctx)
	if err != nil {
		return err
	}
	r.check("I-15 L-110", "scrub finds and repairs the corrupt shard", st.ScrubBad >= 1 && st.Repaired >= 1, fmt.Sprintf("%+v", st))
	if reps, err := r.chunksOf(ctx, target); err == nil && len(reps) > 0 && slot < len(reps[0].Slots) {
		s := reps[0].Slots[slot]
		r.checkAt("L-110 L-130", "corrupt shard replaced", s.Node != 0 && s.Live && s.Present != nil && *s.Present, fmt.Sprintf("slot %d now on node %d", slot, s.Node), target, c.Hash, "")
	}
	// m+1 shards of a dedicated chunk: the read fails cleanly or is served exact from a
	// cache, never wrong bytes
	p := join(r.Root, "corrupt.bin")
	data := r.randomBytes(r.Profile.ChunkSize)
	if err := v.WriteAt(p, 0, data); err != nil {
		return err
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	reps, err := r.chunksOf(ctx, p)
	if err != nil || len(reps) != 1 {
		return fmt.Errorf("chunks of %s: %w", p, err)
	}
	c = reps[0]
	corrupted := 0
	for _, slot := range presentSlots(c) { // m+1 of the slots that hold their shard
		if corrupted > int(c.M) {
			break
		}
		key, err := shardKey(c, slot)
		if err != nil {
			return err
		}
		holder, ok := r.nodeByIdx(c.Slots[slot].Node)
		if !ok {
			continue
		}
		if err := r.Env.Corrupt(ctx, holder, key); err == nil {
			corrupted++
		}
	}
	r.event("corrupt", fmt.Sprintf("%s: %d shards", c.Hash[:12], corrupted))
	// read through a gateway that does not hold the chunk in its cache
	reader := gw
	for _, g := range r.gateways() {
		if g.Idx != gw.Idx {
			reader = g
			break
		}
	}
	rv := r.volume(reader)
	got, err = rv.ReadAll(p)
	ok, text := corruptionVerdict(corrupted, int(c.M), reader.Idx != gw.Idx, err, bytes.Equal(got, data))
	r.checkAt("I-40 L-106", fmt.Sprintf("m+1=%d corrupt shards: error, never garbage", int(c.M)+1), ok, text, p, c.Hash, reader.Name)
	// the file is lost; an operator deletes it and purges it from the trash, which is
	// what takes its unrepairable chunk out of every later count (docs/06 §1)
	if err := v.Remove(p); err != nil {
		return err
	}
	purged, err := r.Env.PurgeTrash(ctx)
	if err != nil {
		return err
	}
	r.event("purge", fmt.Sprintf("%d inodes, the lost file among them", purged))
	return nil
}

// --- 6 consensus ---------------------------------------------------------------------------

func phaseConsensus(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Kill {
		r.skip("environment cannot kill nodes")
		return nil
	}
	m := r.corpusModel()
	u := r.undoer(ctx)
	defer u.run()
	for round := range r.Profile.LeaderKills {
		rep, _, err := r.anyStatus(ctx)
		if err != nil {
			return err
		}
		var leader NodeRef
		for i := range rep.Nodes {
			if rep.Nodes[i].Leader {
				leader, _ = r.nodeByIdx(int(rep.Nodes[i].Idx))
			}
		}
		if leader.Name == "" {
			r.check("I-6", "leader known before the kill", false, "no leader in status")
			return nil
		}
		var gw NodeRef
		for _, g := range r.gateways() { // write through a follower: killing our own gateway cuts the mount
			if g.Idx != leader.Idx {
				gw = g
				break
			}
		}
		if gw.Name == "" {
			if round == 0 {
				r.skip(fmt.Sprintf("the only gateway (%s) leads; a live run cannot kill its own node", leader.Name))
				return nil
			}
			r.event("note", fmt.Sprintf("round %d skipped: the only gateway (%s) now leads", round, leader.Name))
			break
		}
		v := r.volume(gw)
		dir := join(r.Root, "consensus")
		if err := v.Mkdir(dir); err != nil {
			return err
		}
		acked := model{}
		// writes before, during and after the leader loss: every acknowledged one must persist
		for i := range 6 {
			if i == 2 {
				if err := r.Env.Kill(ctx, leader); err != nil {
					return err
				}
				u.add("start "+leader.Name, func(c context.Context) error { return r.Env.Start(c, leader) })
				r.event("kill leader", leader.Name)
			}
			p := join(dir, fmt.Sprintf("r%d-%d.bin", round, i))
			data := r.randomBytes(1 + r.Rng.Intn(r.Profile.ChunkSize))
			if err := v.WriteAt(p, 0, data); err == nil {
				acked[p] = data
			} else if i >= 2 {
				r.event("write refused during election", fmt.Sprintf("%s: %v", p, err))
			}
		}
		if err := r.Env.Start(ctx, leader); err != nil {
			return err
		}
		u.done("start " + leader.Name)
		r.event("start", leader.Name)
		healthy, hdetail := r.waitHealthy(ctx, r.Profile.RestartSettle+30*time.Second)
		r.check("L-61", "cluster healthy after the leader returned", healthy, hdetail)
		if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
			return err
		}
		reader := r.gateways()[len(r.gateways())-1]
		rv := r.volume(reader)
		bad, first := r.verifyModel(rv, acked)
		r.checkAt("I-7 I-12 L-57 L-58 L-61", fmt.Sprintf("round %d: acknowledged writes persist across the leader loss", round), bad == 0, fmt.Sprintf("%d acked, %s", len(acked), first), first, "", reader.Name)
		maps.Copy(m, acked)
	}
	r.checkFSMAgreement(ctx, "I-4 I-29", "FSM hashes agree after leader losses")
	return nil
}

// --- 7 membership --------------------------------------------------------------------------

func phaseMembership(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Mutate {
		r.skip("policy changes and drains need --destructive on a live cluster")
		return nil
	}
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	m := r.corpusModel()
	gw := r.gateways()[0]
	v := r.volume(gw)
	k0, m0 := rep.Volume.K, rep.Volume.M
	active := 0 // placement targets (disks), not rows: a device holds no slot (docs/14 §9)
	for i := range rep.Nodes {
		if rep.Nodes[i].Kind == "disk" && rep.Nodes[i].State == "active" && rep.Nodes[i].Live {
			active++
		}
	}
	// a different policy with the same k+m, then back
	var k1, m1 uint8
	switch {
	case k0 > 1:
		k1, m1 = k0-1, m0+1
	case int(k0)+int(m0) < active:
		k1, m1 = k0+1, m0
	default:
		r.skip("no alternative policy fits the live node count")
		return nil
	}
	u := r.undoer(ctx)
	defer u.run()
	// the way back is the mode the cluster was in: an auto volume goes back to auto,
	// not to a pin on the policy auto had chosen (I-112)
	wasAuto := rep.Volume.Auto
	restore := func(c context.Context) error {
		if wasAuto {
			return r.Env.SetPolicyAuto(c)
		}
		return r.Env.SetPolicy(c, k0, m0)
	}
	for _, pol := range [][2]uint8{{k1, m1}, {k0, m0}} {
		var err error
		if pol == [2]uint8{k0, m0} {
			err = restore(ctx)
		} else {
			err = r.Env.SetPolicy(ctx, pol[0], pol[1])
		}
		if err != nil {
			r.check("L-100", fmt.Sprintf("set-redundancy %d+%d accepted", pol[0], pol[1]), false, err.Error())
			return nil
		}
		if pol == [2]uint8{k1, m1} {
			u.add("set-redundancy back", restore)
		} else {
			u.done("set-redundancy back")
		}
		r.event("set-redundancy", fmt.Sprintf("%d+%d", pol[0], pol[1]))
		// writes under the new policy while the migration runs
		p := join(r.Root, fmt.Sprintf("policy-%d+%d.bin", pol[0], pol[1]))
		data := r.randomBytes(r.Profile.ChunkSize + 5)
		if err := v.WriteAt(p, 0, data); err == nil {
			m[p] = data
		}
		if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
			return err
		}
		if rep, err = r.repairUntilClean(ctx); err != nil {
			return err
		}
		r.check("I-15 I-32 L-100 L-101", fmt.Sprintf("migration to %d+%d complete", pol[0], pol[1]), rep.Volume.StalePolicy == 0 && rep.Volume.Degraded == 0, fmt.Sprintf("%d stale, %d degraded (last pass %+v)", rep.Volume.StalePolicy, rep.Volume.Degraded, r.lastRepair))
		bad, first := r.verifyModel(v, m)
		r.checkAt("I-12 I-15 L-173", fmt.Sprintf("corpus exact under %d+%d", pol[0], pol[1]), bad == 0, first, first, "", gw.Name)
	}
	// drain the last node (needs a spare), then put it back
	if active <= int(k0)+int(m0) || len(r.nodes) < 2 {
		r.event("note", "drain skipped: no spare node beyond k+m, or no node besides the gateway")
		return nil
	}
	// the last host that holds slots, never the gateway: draining a diskless host would
	// move nothing (docs/14 §9)
	var victim NodeRef
	for _, n := range slices.Backward(r.nodes) {
		if n.Idx != gw.Idx && hostSlots(rep, int(n.Idx)) > 0 {
			victim = n
			break
		}
	}
	if victim.Idx == 0 {
		r.event("note", "drain skipped: no host besides the gateway holds slots")
		return nil
	}
	if err := r.Env.SetState(ctx, victim, meta.Draining); err != nil {
		r.check("L-93", "drain accepted", false, err.Error())
		return nil
	}
	u.add("activate "+victim.Name, func(c context.Context) error { return r.Env.SetState(c, victim, meta.Active) })
	r.event("drain", victim.Name)
	if rep, err = r.repairUntilClean(ctx); err != nil {
		return err
	}
	slots := hostSlots(rep, int(victim.Idx)) // its own store and its disks (docs/14 §1)
	r.checkAt("I-42 L-94", "drained node holds no slots", slots == 0, fmt.Sprintf("%d slots left", slots), "", "", victim.Name)
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-32", "corpus exact after the drain", bad == 0, first, first, "", gw.Name)
	if err := r.Env.SetState(ctx, victim, meta.Active); err != nil {
		return err
	}
	u.done("activate " + victim.Name)
	r.event("activate", victim.Name)
	return nil
}

// --- 8 dedup and gc ------------------------------------------------------------------------

func phaseDedupGC(ctx context.Context, r *Run) error {
	gw := r.gateways()[0]
	v := r.volume(gw)
	m := r.corpusModel()
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	before := rep.Volume.StoredBytes
	// a second copy of the corpus must cost almost nothing
	copyM := model{}
	for p, d := range m {
		if len(p) > len(r.Root)+len("/corpus/") && p[:len(r.Root)+len("/corpus/")] == r.Root+"/corpus/" {
			copyM[join(r.Root, "copy", p[len(r.Root)+len("/corpus/"):])] = d
		}
	}
	if err := r.writeModel(v, copyM); err != nil {
		return err
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	if rep, _, err = r.anyStatus(ctx); err != nil {
		return err
	}
	grew := storedDelta(before, rep.Volume.StoredBytes)
	r.check("I-2 L-19 L-33", "second copy of the corpus adds < 1% stored bytes", before > 0 && grew*100 < int64(before), fmt.Sprintf("%+d bytes on %d (dedup %.2fx)", grew, before, rep.Volume.DedupRatio))
	maps.Copy(m, copyM)
	// a unique file is removed; after the grace GC reclaims its shards
	p := join(r.Root, "gc.bin")
	data := r.randomBytes(r.Profile.BigFile)
	if err := v.WriteAt(p, 0, data); err != nil {
		return err
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	if rep, _, err = r.anyStatus(ctx); err != nil {
		return err
	}
	stored1 := rep.Volume.StoredBytes
	if err := v.Remove(p); err != nil {
		return err
	}
	r.event("remove", p)
	// a delete is a trash (docs/06 §1): nothing is reclaimed until the entry is purged
	if rep, _, err = r.anyStatus(ctx); err != nil {
		return err
	}
	r.check("I-157 L-199", "the removed file waits in the trash, its bytes still stored", rep.Volume.TrashInodes > 0 && rep.Volume.StoredBytes == stored1, fmt.Sprintf("trash %d inodes, stored %d -> %d", rep.Volume.TrashInodes, stored1, rep.Volume.StoredBytes))
	purged, err := r.Env.PurgeTrash(ctx)
	if err != nil {
		return err
	}
	r.event("purge", fmt.Sprintf("%d inodes", purged))
	r.check("I-153 L-201", "purge empties the trash", purged > 0, fmt.Sprintf("%d purged", purged))
	if err := r.Env.Wait(ctx, r.Profile.Grace+time.Second); err != nil {
		return err
	}
	res, err := r.gcNow(ctx)
	if err != nil {
		return err
	}
	r.check("I-13 L-201 L-75 L-77", "gc collects the purged file's chunks", res.Victims > 0 && res.Deleted > 0, fmt.Sprintf("%+v (refused deletes are orphans for the sweep)", res))
	if rep, _, err = r.anyStatus(ctx); err != nil {
		return err
	}
	r.check("L-77 L-134", "stored bytes shrink after gc", rep.Volume.StoredBytes < stored1, fmt.Sprintf("%d -> %d", stored1, rep.Volume.StoredBytes))
	fsck, err := r.Env.Fsck(ctx, r.nodes[0])
	r.check("I-27 I-44", "fsck clean after gc", err == nil && fsck.OK, fsck.Detail+fmt.Sprint(err))
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-13 I-41", "corpus exact after gc", bad == 0, first, first, "", gw.Name)
	return nil
}

// --- 9 restart storm -----------------------------------------------------------------------

func phaseRestartStorm(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Kill {
		r.skip("environment cannot restart nodes")
		return nil
	}
	m := r.corpusModel()
	dir := join(r.Root, "storm")
	acked := model{}
	for _, gw := range r.gateways() {
		v := r.volume(gw)
		if err := v.Mkdir(dir); err != nil {
			return err
		}
		for i := range 3 {
			p := join(dir, fmt.Sprintf("%s-%d.bin", gw.Name, i))
			data := r.randomBytes(1 + r.Rng.Intn(r.Profile.ChunkSize))
			if err := v.WriteAt(p, 0, data); err == nil {
				acked[p] = data
			}
		}
		// a read-modify-write across a chunk boundary of a two-chunk file: what the WAL
		// replays after the restart is a partial overwrite, not a fresh file
		cs := r.Profile.ChunkSize
		p := join(dir, gw.Name+"-rmw.bin")
		base := r.randomBytes(2*cs + 5)
		if err := v.WriteAt(p, 0, base); err == nil {
			patch := r.randomBytes(cs / 2)
			off := int64(cs - 7)
			if err := v.WriteAt(p, off, patch); err == nil {
				acked[p] = overwrite(base, off, patch)
			}
		}
	}
	// restart everything while those writes may still sit in the WALs
	if err := r.Env.Restart(ctx, r.nodes); err != nil {
		return err
	}
	r.event("restart all", fmt.Sprintf("%d nodes", len(r.nodes)))
	healthy, hdetail := r.waitHealthy(ctx, r.Profile.RestartSettle+60*time.Second)
	r.check("L-120", "cluster healthy after the restart storm", healthy, hdetail)
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	gw := r.gateways()[len(r.gateways())-1]
	v := r.volume(gw)
	if len(acked) == 0 {
		r.checkSkipped("I-7 I-37 L-114 L-121", "writes acknowledged before the restart persist", "no write was acknowledged before the restart: nothing to verify")
	} else {
		bad, first := r.verifyModel(v, acked)
		r.checkAt("I-7 I-37 L-114 L-121", "writes acknowledged before the restart persist", bad == 0, first, first, "", gw.Name)
	}
	maps.Copy(m, acked)
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-12 L-120", "whole corpus exact after the restart storm", bad == 0, first, first, "", gw.Name)
	r.checkFSMAgreement(ctx, "I-4 I-5", "FSM hashes agree after the restart storm")
	return nil
}

// --- 10 partition --------------------------------------------------------------------------

// phasePartition cuts a minority of nodes off, then the gateway itself. The majority keeps
// committing (L-148); a cut-off gateway refuses metadata operations with EROFS while its
// WRITEs stay buffered and are committed once the partition heals (L-59); nothing it
// refused shows up afterwards and nothing acknowledged is applied twice (I-29).
func phasePartition(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Partition {
		r.skip("environment cannot partition nodes")
		return nil
	}
	rep, _, err := r.anyStatus(ctx)
	if err != nil {
		return err
	}
	k := int(rep.Volume.K)
	voters := 0
	for i := range rep.Nodes {
		if rep.Nodes[i].Voter {
			voters++
		}
	}
	gws := r.gateways()
	gw := gws[0]
	// the minority: the last nodes, never the first gateway, keeping a voter quorum and at
	// least k nodes on the majority side
	budget := min((voters-1)/2, len(r.nodes)-k)
	var minority, majority []NodeRef
	for _, n := range slices.Backward(r.nodes) {
		if n.Idx != gw.Idx && len(minority) < budget {
			minority = append(minority, n)
		} else {
			majority = append(majority, n)
		}
	}
	if len(minority) == 0 {
		r.skip(fmt.Sprintf("need a minority to cut off: %d voters, %d nodes, k=%d", voters, len(r.nodes), k))
		return nil
	}
	m := r.corpusModel()
	v := r.volume(gw)
	dir := join(r.Root, "partition")
	if err := v.Mkdir(dir); err != nil {
		return err
	}
	cs := r.Profile.ChunkSize
	// files written before the cut, so a cut-off gateway can write into an existing inode
	pre := map[string][]byte{}
	for _, name := range []string{"minor.bin", "self.bin"} {
		p := join(dir, name)
		data := r.randomBytes(cs + 5)
		if err := v.WriteAt(p, 0, data); err != nil {
			return err
		}
		pre[p], m[p] = data, data
	}
	if err := r.Env.Wait(ctx, r.Profile.FlushIdle); err != nil {
		return err
	}
	names := func(side []NodeRef) string {
		out := make([]string, 0, len(side))
		for _, n := range side {
			out = append(out, n.Name)
		}
		return strings.Join(out, ",")
	}

	u := r.undoer(ctx)
	defer u.run()
	// step 1: a minority is cut off
	if err := r.Env.Partition(ctx, minority, majority); err != nil {
		return err
	}
	u.add("heal", r.Env.Heal)
	r.event("partition", names(minority)+" | "+names(majority))
	led, ldetail := r.awaitLeader(ctx, majority, r.Profile.RestartSettle+30*time.Second)
	r.check("I-6", "the majority side elects a leader", led, ldetail)
	major := join(dir, "major.bin")
	data := r.randomBytes(cs + 9)
	werr := v.WriteAt(major, 0, data)
	r.check("I-6 L-148", fmt.Sprintf("writes commit on the majority side with %s cut off", names(minority)), werr == nil, fmt.Sprint(werr))
	if werr == nil {
		m[major] = data
	}
	var minorGW *NodeRef
	for i := range gws {
		for _, n := range minority {
			if gws[i].Idx == n.Idx {
				minorGW = &gws[i]
			}
		}
	}
	minor := join(dir, "minor.bin")
	refused := join(dir, "minor-dir")
	if minorGW != nil {
		mv := r.volume(*minorGW)
		merr := mv.Mkdir(refused)
		r.checkAt("I-6 I-39 L-59", "metadata operation on the cut-off side is refused", merr != nil, fmt.Sprint(merr), refused, "", minorGW.Name)
		tail := r.randomBytes(cs / 2)
		off := int64(cs - 3)
		if werr := mv.WriteAt(minor, off, tail); werr == nil { // L-59: still buffered
			m[minor] = overwrite(pre[minor], off, tail)
			r.event("note", fmt.Sprintf("WRITE buffered on cut-off gateway %s", minorGW.Name))
		} else {
			r.event("note", fmt.Sprintf("WRITE refused on cut-off gateway %s: %v", minorGW.Name, werr))
		}
	} else {
		r.event("note", "no gateway on the cut-off side; its EROFS and buffered-write checks need MultiGateway")
	}
	if err := r.Env.Heal(ctx); err != nil {
		return err
	}
	u.done("heal")
	r.event("heal", "")
	healthy, hdetail := r.waitHealthy(ctx, r.Profile.RestartSettle+30*time.Second)
	r.check("L-132", "cluster healthy after the heal", healthy, hdetail)
	ok, detail := r.awaitCommitted(ctx, gw, minor, m[minor])
	r.checkAt("I-7 L-59", "bytes written on the cut-off side are committed after the heal", ok, detail, minor, "", gw.Name)
	r.noteRefused(v, refused)
	bad, first := r.verifyModel(v, m)
	r.checkAt("I-12 I-29", "corpus exact after the minority rejoins", bad == 0, first, first, "", gw.Name)
	r.checkFSMAgreement(ctx, "I-4 I-29", "FSM hashes agree after the minority rejoins")

	// step 2: the gateway itself is cut off
	var others []NodeRef
	for _, n := range r.nodes {
		if n.Idx != gw.Idx {
			others = append(others, n)
		}
	}
	if err := r.Env.Partition(ctx, []NodeRef{gw}, others); err != nil {
		return err
	}
	u.add("heal", r.Env.Heal)
	r.event("partition", gw.Name+" | "+names(others))
	refused = join(dir, "self-dir")
	merr := v.Mkdir(refused)
	r.checkAt("I-6 I-39 L-59", "metadata operation on the cut-off gateway is refused", merr != nil, fmt.Sprint(merr), refused, "", gw.Name)
	self := join(dir, "self.bin")
	tail := r.randomBytes(cs / 2)
	off := int64(cs - 7)
	werr = v.WriteAt(self, off, tail)
	r.checkAt("L-59", "WRITE on the cut-off gateway is buffered, not refused", werr == nil, fmt.Sprint(werr), self, "", gw.Name)
	if werr == nil {
		m[self] = overwrite(pre[self], off, tail)
	}
	if err := r.Env.Heal(ctx); err != nil {
		return err
	}
	u.done("heal")
	r.event("heal", "")
	healthy, hdetail = r.waitHealthy(ctx, r.Profile.RestartSettle+30*time.Second)
	r.check("L-132", "cluster healthy after the gateway rejoins", healthy, hdetail)
	ok, detail = r.awaitCommitted(ctx, gw, self, m[self])
	r.checkAt("I-7 L-59", "bytes buffered on the cut-off gateway are committed after the heal", ok, detail, self, "", gw.Name)
	r.noteRefused(v, refused)
	for _, g := range gws {
		rv := r.volume(g)
		bad, first := r.verifyModel(rv, m)
		r.checkAt("I-12 I-29 L-148", "corpus exact after the gateway rejoins", bad == 0, first, first, "", g.Name)
	}
	r.checkFSMAgreement(ctx, "I-4 I-29", "FSM hashes agree after the gateway rejoins")
	return nil
}

// overwrite is the file content after writing data at off into base.
func overwrite(base []byte, off int64, data []byte) []byte {
	out := make([]byte, max(len(base), int(off)+len(data)))
	copy(out, base)
	copy(out[off:], data)
	return out
}

// noteRefused records whether a metadata operation refused during a partition landed
// anyway. Raft allows either outcome (the leader may have appended it before losing its
// lease and commit it on rejoining); what matters is that every node agrees, which the
// FSM-hash check covers.
func (r *Run) noteRefused(v Volume, path string) {
	if _, err := v.Size(path); err == nil {
		r.event("note", "refused metadata operation landed after the heal (ambiguous outcome, allowed): "+path)
	} else {
		r.event("note", "refused metadata operation is absent after the heal: "+path)
	}
}

// awaitLeader polls status through the given nodes until one reports a healthy leader.
func (r *Run) awaitLeader(ctx context.Context, via []NodeRef, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	for {
		detail := "no status"
		for _, n := range via {
			rep, err := r.Env.Status(ctx, n)
			if err != nil {
				detail = fmt.Sprintf("%s: %v", n.Name, err)
				continue
			}
			if rep.Health == "healthy" && rep.Leader != "" {
				return true, fmt.Sprintf("leader %s (via %s)", rep.Leader, n.Name)
			}
			detail = fmt.Sprintf("%s: health %s, leader %q", n.Name, rep.Health, rep.Leader)
		}
		if time.Now().After(deadline) || r.Env.Wait(ctx, 500*time.Millisecond) != nil {
			return false, detail
		}
	}
}

// awaitCommitted polls until the FSM of a node other than gw (or gw itself when it is the
// only node) lists path with committed chunks summing to the expected length, and the
// bytes read through gw match. The chunk sum is what proves the flush landed: gw alone
// would serve its own dirty bytes.
func (r *Run) awaitCommitted(ctx context.Context, gw NodeRef, path string, want []byte) (bool, string) {
	witness := gw
	for _, n := range r.nodes {
		if n.Idx != gw.Idx {
			witness = n
			break
		}
	}
	deadline := time.Now().Add(r.Profile.FlushIdle + 30*time.Second)
	for {
		detail := ""
		reps, err := r.Env.Chunks(ctx, witness, path)
		switch {
		case err != nil:
			detail = fmt.Sprintf("%s: %v", witness.Name, err)
		default:
			committed, total := true, 0
			for i := range reps {
				if !reps[i].Found || reps[i].State == "unknown" {
					committed = false
				}
				total += int(reps[i].Size)
			}
			v := r.volume(gw)
			got, rerr := v.ReadAll(path)
			switch {
			case !committed || total != len(want):
				detail = fmt.Sprintf("%s lists %d chunks, %d bytes committed, want %d", witness.Name, len(reps), total, len(want))
			case rerr != nil:
				detail = fmt.Sprintf("read via %s: %v", gw.Name, rerr)
			case !bytes.Equal(got, want):
				detail = fmt.Sprintf("read via %s: %d bytes, want %d", gw.Name, len(got), len(want))
			default:
				return true, fmt.Sprintf("%d chunks committed on %s", len(reps), witness.Name)
			}
		}
		if time.Now().After(deadline) || r.Env.Wait(ctx, 500*time.Millisecond) != nil {
			return false, detail
		}
	}
}
