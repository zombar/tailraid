package cluster_test

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
)

var steps = flag.Int("steps", 0, "model checker steps (0 = 40 under -short, 600 otherwise)")

// world is one candidate client belief: name → content. Files are immutable once stored
// so worlds may share them; every mutation stores a fresh slice.
type world map[string][]byte

func (w world) clone() world {
	c := make(world, len(w))
	maps.Copy(c, w)
	return c
}

// maxWorlds bounds the candidate explosion from ambiguous metadata failures; beyond it the
// checker quiesces early, which resolves every ambiguity.
const maxWorlds = 8

// TestModelCheck_Long drives a 5-node 3+2 cluster with a weighted random schedule of
// client operations and faults, and after every quiescence checks the plan's
// invariants: every acknowledged byte reads identically from every live gateway (I-12),
// every live FSM holds the same state (I-4), every committed chunk has its shards on the
// nodes its placement names (I-9/I-13), and repair converges (I-15).
//
// Two modelling rules follow from the docs. A metadata operation that fails (EROFS) has an
// unknown outcome when the command may have reached a leader that then lost leadership
// (docs/02 §forwarding, I-29): the entry can still commit after the heal. The checker
// therefore keeps every candidate world and accepts the one reality matches. And
// unflushed writes to one file stay on one gateway between quiescences: docs/04 §6 makes
// concurrent writers through different gateways last-committer-wins at chunk
// granularity, which a byte-exact model cannot predict.
// The schedule also changes the volume policy and drains a node mid-run: after each
// quiescence the repairer must have migrated every chunk to the current (k,m) and moved
// every slot off the draining node (docs/06).
// Invariant: I-4, I-9, I-12, I-13, I-15, I-29
// Lifecycle: L-162, L-163, L-173
func TestModelCheck_Long(t *testing.T) {
	n := *steps
	if n == 0 {
		if testing.Short() {
			n = 40
		} else {
			n = 600
		}
	}
	rng := testharness.Seed(t)
	c := testharness.NewCluster(t, 5, 3, 2)
	defer c.Close()
	cs := int(testharness.ChunkSize)
	worlds := []world{{}}
	names := []string{"/a", "/b", "/d/c", "/d/e", "/big"}
	// A node serves NFS only once WaitReady has passed (node.Serve), so a restarted node
	// becomes a gateway only when its FSM has caught up with the log it holds.
	pending := map[int]bool{}
	liveGateways := func() []int {
		var out []int
		for i := range c.Nodes {
			if !c.Alive(i) {
				continue
			}
			if pending[i] {
				if err := c.Nodes[i].Node.WaitReady(ctx, 200*time.Millisecond); err != nil {
					continue
				}
				delete(pending, i)
			}
			out = append(out, i)
		}
		return out
	}
	restart := func(i int) {
		c.Restart(i)
		pending[i] = true
	}
	quorumUp := func() {
		for i := range c.Nodes {
			if !c.Alive(i) {
				restart(i)
			}
		}
		c.Heal()
		c.Settle()
		pending = map[int]bool{} // Settle ran WaitReady on every node
	}
	var partitioned bool
	log := func(format string, args ...any) { t.Logf("step: "+format, args...) }
	policy := [2]uint8{3, 2}
	policies := [][2]uint8{{3, 2}, {2, 2}, {2, 1}, {1, 2}, {3, 1}}
	draining := -1 // node index being drained, or -1
	type addedDisk struct {
		node   int
		idx    meta.NodeIdx
		failed bool
	}
	var disks []*addedDisk // added disks (docs/14): grown and failed at random
	restoreDisks := func() {
		for _, d := range disks {
			if d.failed {
				c.FailDisk(d.node, d.idx, false)
				d.failed = false
			}
		}
	}
	leaderOp := func(what string, op func(l *testharness.ClusterNode) error) bool {
		if partitioned {
			return false
		}
		if err := op(c.Leader()); err != nil {
			log("%s failed: %v", what, err)
			return false
		}
		return true
	}
	// owner: the gateway holding a file's unflushed writes; frozen: names touched by a
	// remove or rename this epoch. Nobody writes them until the next quiescence: a gateway
	// that has not yet applied the command still resolves the old inode (docs/02 §stale
	// window) and after an ambiguous failure the dirty data may or may not have followed
	// the inode.
	owner := map[string]int{}
	frozen := map[string]bool{}
	pickGateway := func(name string, gws []int) (int, bool) {
		if frozen[name] {
			return 0, false
		}
		if gw, ok := owner[name]; ok {
			if c.Alive(gw) {
				return gw, true
			}
			return 0, false // its dirty data is in a dead gateway's WAL; wait for the restart
		}
		gw := gws[rng.Intn(len(gws))]
		owner[name] = gw
		return gw, true
	}
	// ambiguous forks every world with op applied; the names are frozen until resolved.
	ambiguous := func(op func(w world), touched ...string) {
		forked := make([]world, 0, 2*len(worlds))
		for _, w := range worlds {
			alt := w.clone()
			op(alt)
			forked = append(forked, w, alt)
		}
		worlds = forked
		for _, nm := range touched {
			frozen[nm] = true
		}
	}
	exists := func(name string) bool { return worlds[0][name] != nil }
	// definite: the gateway's own FSM answered before proposing anything, so nothing
	// happened. The answer may still disagree with the model: a gateway that has not
	// applied another gateway's command yet (or sits in a partition) resolves stale names.
	definite := func(err error) bool {
		return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrExist) || errors.Is(err, meta.ErrNotExist) || errors.Is(err, meta.ErrExist)
	}

	doStep := func(step int) {
		gws := liveGateways()
		switch r := rng.Intn(100); {
		case r < 55 && len(gws) > 0: // client write at a random offset through the file's gateway
			name := names[rng.Intn(len(names))]
			gw, ok := pickGateway(name, gws)
			if !ok {
				return
			}
			cur := worlds[0][name]
			var off int
			switch rng.Intn(4) {
			case 0:
				off = 0
			case 1:
				off = len(cur)
			case 2:
				off = len(cur) + rng.Intn(cs)
			default:
				if len(cur) > 0 {
					off = rng.Intn(len(cur))
				}
			}
			data := testharness.Pattern(1+rng.Intn(cs), byte(step))
			fh, err := c.Nodes[gw].FS.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				log("%d open %s on n%d: %v (not acknowledged)", step, name, gw, err)
				if definite(err) {
					return // stale gateway: nothing was proposed
				}
				if !exists(name) { // the Create may have committed anyway
					ambiguous(func(w world) { w[name] = []byte{} }, name)
				}
				return
			}
			if _, err := fh.Seek(int64(off), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := fh.Write(data); err != nil {
				log("%d write %s on n%d: %v (not acknowledged)", step, name, gw, err)
				_ = fh.Close()
				return
			}
			_ = fh.Close()
			for _, w := range worlds {
				old := w[name]
				end := off + len(data)
				buf := make([]byte, max(len(old), end))
				copy(buf, old)
				copy(buf[off:], data)
				w[name] = buf
			}
			log("%d write %s@%d+%d via n%d", step, name, off, len(data), gw)
		case r < 62 && len(gws) > 0: // truncate (an overlay operation: no proposal, so never ambiguous)
			name := names[rng.Intn(len(names))]
			if !exists(name) {
				return
			}
			gw, ok := pickGateway(name, gws)
			if !ok {
				return
			}
			size := rng.Intn(len(worlds[0][name]) + cs)
			fh, err := c.Nodes[gw].FS.OpenFile(name, os.O_WRONLY, 0)
			if err != nil {
				return
			}
			if err := fh.Truncate(int64(size)); err != nil {
				log("%d truncate %s via n%d failed: %v", step, name, gw, err)
				_ = fh.Close()
				return
			}
			_ = fh.Close()
			for _, w := range worlds {
				buf := make([]byte, size)
				copy(buf, w[name])
				w[name] = buf
			}
			log("%d truncate %s to %d via n%d", step, name, size, gw)
		case r < 68 && len(gws) > 0: // remove
			gw := gws[rng.Intn(len(gws))]
			name := names[rng.Intn(len(names))]
			if !exists(name) || frozen[name] {
				return
			}
			if err := c.Nodes[gw].FS.Remove(name); err != nil {
				if definite(err) {
					log("%d remove %s via n%d: %v (stale gateway, nothing happened)", step, name, gw, err)
					return
				}
				log("%d remove %s via n%d failed: %v (outcome unknown)", step, name, gw, err)
				ambiguous(func(w world) { delete(w, name) }, name)
				return
			}
			for _, w := range worlds {
				delete(w, name)
			}
			delete(owner, name)
			frozen[name] = true // other gateways may still resolve the old inode (docs/02 §stale window)
			log("%d remove %s via n%d", step, name, gw)
		case r < 74 && len(gws) > 0: // rename over another name
			gw := gws[rng.Intn(len(gws))]
			from, to := names[rng.Intn(len(names))], names[rng.Intn(len(names))]
			if from == to || !exists(from) || frozen[from] || frozen[to] {
				return
			}
			mv := func(w world) {
				if w[from] != nil {
					w[to] = w[from]
				}
				delete(w, from)
			}
			if err := c.Nodes[gw].FS.Rename(from, to); err != nil {
				if definite(err) {
					log("%d rename %s -> %s via n%d: %v (stale gateway, nothing happened)", step, from, to, gw, err)
					return
				}
				log("%d rename %s -> %s via n%d failed: %v (outcome unknown)", step, from, to, gw, err)
				ambiguous(mv, from, to)
				return
			}
			for _, w := range worlds {
				mv(w)
			}
			if g, ok := owner[from]; ok { // the dirty data follows the inode
				owner[to] = g
			} else {
				delete(owner, to)
			}
			delete(owner, from)
			frozen[from], frozen[to] = true, true
			log("%d rename %s -> %s via n%d", step, from, to, gw)
		case r < 82: // kill one node (never below a majority)
			if len(gws) <= 3 {
				return
			}
			victim := gws[rng.Intn(len(gws))]
			c.Kill(victim)
			log("%d kill n%d", step, victim)
		case r < 88: // restart a dead node
			for i := range c.Nodes {
				if !c.Alive(i) {
					restart(i)
					log("%d restart n%d", step, i)
					break
				}
			}
		case r < 92 && !partitioned && len(gws) == 5: // partition 2|3
			c.Partition([]int{0, 1}, []int{2, 3, 4})
			partitioned = true
			log("%d partition {0,1}|{2,3,4}", step)
		case r < 95: // advance the clock past the live window and let hold-downs run
			c.Clk.Advance(cluster.LiveThreshold + time.Second)
			c.Probe()
			log("%d clock +live", step)
		case r < 97: // change the volume policy (new chunks follow it, old ones migrate)
			limit := len(c.Nodes)
			if draining >= 0 {
				limit--
			}
			var options [][2]uint8
			for _, p := range policies {
				if p != policy && int(p[0])+int(p[1]) <= limit {
					options = append(options, p)
				}
			}
			if len(options) == 0 {
				return
			}
			next := options[rng.Intn(len(options))]
			if leaderOp("set_policy", func(l *testharness.ClusterNode) error {
				res, _, err := l.Node.Propose(ctx, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{K: next[0], M: next[1], Now: c.Clk.Now().UnixNano()}})
				if err == nil {
					err = res.Err
				}
				return err
			}) {
				policy = next
				log("%d set_policy %d+%d", step, next[0], next[1])
			}
		case r < 98 && rng.Intn(2) == 0 && !partitioned: // add a disk, or fail or restore one
			gws := liveGateways()
			if len(disks) < 4 && len(gws) > 0 {
				i := gws[rng.Intn(len(gws))]
				disks = append(disks, &addedDisk{node: i, idx: c.AddDisk(i)})
				log("%d add disk to n%d", step, i)
				return
			}
			if len(disks) > 0 {
				d := disks[rng.Intn(len(disks))]
				d.failed = !d.failed
				c.FailDisk(d.node, d.idx, d.failed)
				c.Probe()
				log("%d disk %d of n%d failed=%v", step, d.idx, d.node, d.failed)
			}
		case r < 98: // drain a node, or put the draining one back
			if draining >= 0 {
				victim := draining
				if leaderOp("activate", func(l *testharness.ClusterNode) error {
					return l.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[victim].Idx(), State: meta.Active})
				}) {
					draining = -1
					log("%d activate n%d", step, victim)
				}
				return
			}
			if int(policy[0])+int(policy[1]) >= len(c.Nodes) {
				return // nowhere to move the slots to
			}
			victim := rng.Intn(len(c.Nodes))
			if leaderOp("drain", func(l *testharness.ClusterNode) error {
				return l.Node.MemberOp(ctx, rpc.MemberOp{Op: "set_state", NodeIdx: c.Nodes[victim].Idx(), State: meta.Draining})
			}) {
				draining = victim
				log("%d drain n%d", step, victim)
			}
		default: // repair pass on the leader (if any)
			if partitioned {
				return
			}
			leader := c.Leader()
			st, err := newRepairer(c, leader, repairCfg()).Scan(ctx)
			if err == nil {
				log("%d repair: %+v", step, st)
			}
		}
	}

	for step := 1; step <= n; step++ {
		doStep(step)
		if step%25 == 0 || step == n || len(worlds) > maxWorlds {
			// quiesce: heal, bring everyone back, flush every gateway, repair, then check
			if partitioned {
				c.Heal()
				partitioned = false
			}
			quorumUp()
			restoreDisks()
			for i := range c.Nodes {
				for range 30 {
					if err := c.Nodes[i].Flush.FlushAll(ctx); err == nil {
						break
					}
					<-time.After(100 * time.Millisecond)
				}
			}
			c.Settle()
			c.Clk.Advance(cluster.FillAfter + time.Second)
			c.Probe()
			// repair until a pass finds nothing left to do (fills, then migrations)
			var st repair.Stats
			for pass := 0; ; pass++ {
				var err error
				if st, err = newRepairer(c, c.Leader(), repairCfg()).Scan(ctx); err != nil {
					t.Fatalf("step %d repair: %v", step, err)
				}
				c.Settle()
				if st.Repaired+st.Partial+st.Migrated+st.Deferred == 0 {
					break
				}
				if pass == 5 {
					t.Fatalf("step %d: repair does not converge: %+v", step, st)
				}
			}
			if st.PolicyStale != 0 || st.Unrepairable != 0 {
				t.Fatalf("step %d: after repair %+v (policy %d+%d, draining %d)", step, st, policy[0], policy[1], draining)
			}
			worlds = []world{checkInvariants(t, c, worlds, names, step, policy, draining)}
			owner = map[string]int{}
			frozen = map[string]bool{}
		}
	}
}

// checkInvariants returns the candidate world reality matches, or fails the test.
func checkInvariants(t *testing.T, c *testharness.Cluster, worlds []world, names []string, step int, policy [2]uint8, draining int) world {
	t.Helper()
	// I-12: every acknowledged byte reads identically from every gateway, cold; names the
	// world lacks must not exist
	sort.Strings(names)
	var matched world
	var firstMismatch string
	for wi, w := range worlds {
		mismatch := ""
	check:
		for _, name := range names {
			for i := range c.Nodes {
				got, err := c.ReadFileCold(i, name)
				switch {
				case w[name] == nil && errors.Is(err, meta.ErrNotExist):
					continue
				case w[name] == nil:
					mismatch = fmt.Sprintf("n%d %s exists (len %d), model says removed (err %v)", i, name, len(got), err)
				case err != nil:
					mismatch = fmt.Sprintf("n%d read %s: %v", i, name, err)
				case !bytes.Equal(got, w[name]):
					d := testharness.FirstDiff(got, w[name])
					win := func(b []byte) []byte { return b[d:min(len(b), d+12)] }
					mismatch = fmt.Sprintf("n%d %s differs at %d (len %d want %d) got % x want % x", i, name, d, len(got), len(w[name]), win(got), win(w[name]))
					if in, err := c.Nodes[i].Node.FSM.Resolve(strings.Split(strings.TrimPrefix(name, "/"), "/")); err == nil {
						for ci, h := range in.Chunks { // the chunk table of the offending inode helps triage
							ch, _ := c.Nodes[i].Node.FSM.GetChunk(h)
							t.Logf("  n%d %s ino %d v%d size %d chunk[%d]=%x size=%d refs=%d placement=%v", i, name, in.Ino, in.Version, in.Size, ci, h[:6], ch.Size, ch.Refs, ch.Placement)
						}
					}
				}
				if mismatch != "" {
					break check
				}
			}
		}
		if mismatch == "" {
			matched = w
			if wi != 0 {
				t.Logf("step %d: candidate %d of %d matched", step, wi, len(worlds))
			}
			break
		}
		if wi == 0 {
			firstMismatch = mismatch
		}
	}
	if matched == nil {
		t.Fatalf("step %d: no candidate world matches (%d candidates); against the canonical one: %s", step, len(worlds), firstMismatch)
	}
	// I-4: every FSM holds the same state
	var ref []byte
	for i := range c.Nodes {
		snap, err := c.Nodes[i].Node.FSM.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		st := snap.(interface{ State() *meta.State }).State()
		var buf bytes.Buffer
		if err := meta.EncodeState(&buf, st); err != nil {
			t.Fatal(err)
		}
		if v := meta.CheckInvariants(st); len(v) != 0 {
			t.Fatalf("step %d: n%d FSM invariants: %v", step, i, v)
		}
		enc := buf.Bytes()[13:] // skip magic/version/applied
		if ref == nil {
			ref = enc
		} else if !bytes.Equal(ref, enc) {
			t.Fatalf("step %d: n%d FSM state diverges from n0", step, i)
		}
	}
	// I-9/I-13/I-15: after repair every live chunk is fully placed on distinct nodes, on
	// the current policy, off the draining node, and every named slot holds its shard
	c.Nodes[0].Node.FSM.ForEachChunk(func(ch meta.Chunk) bool {
		if ch.Refs == 0 {
			return true
		}
		wantK, wantM := meta.TargetPolicy(ch, c.Nodes[0].Node.FSM.Volume())
		if ch.K != wantK || ch.M != wantM {
			t.Fatalf("step %d: chunk %x still on %d+%d, policy %d+%d", step, ch.Hash[:6], ch.K, ch.M, wantK, wantM)
		}
		seen := map[meta.NodeIdx]bool{}
		for i, s := range ch.Placement {
			if s == 0 {
				t.Fatalf("step %d: chunk %x slot %d still missing after repair", step, ch.Hash[:6], i)
			}
			if seen[s] {
				t.Fatalf("step %d: chunk %x places two slots on node %d", step, ch.Hash[:6], s)
			}
			store, host := c.Target(s)
			if store == nil {
				t.Fatalf("step %d: chunk %x slot %d on unknown target %d", step, ch.Hash[:6], i, s)
			}
			if draining >= 0 && host == draining {
				t.Fatalf("step %d: chunk %x slot %d still on draining n%d", step, ch.Hash[:6], i, draining)
			}
			seen[s] = true
			if !store.Has(chunk.Key(ch.Hash, ch.K, ch.M, i)) {
				t.Fatalf("step %d: chunk %x slot %d: shard missing on target %d (n%d)", step, ch.Hash[:6], i, s, host)
			}
		}
		return true
	})
	return matched
}
