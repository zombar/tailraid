package cluster_test

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/repair"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
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
// A remove and a rename over a name are trashes (docs/02): the checker keeps the bytes
// it believes each trashed inode holds, restores and purges them at random, and after
// every quiescence reads every trashed root it knows on every node (I-152, I-153, I-155).
// Node 0 carries a mirror (docs/16): after every quiescence the checker begins a snapshot
// and keeps the world as of its index; the copy runs some steps later, while the volume
// moves on, GC runs and the mirror goes offline and back; a sync whose host restarted or
// whose mirror went away fails. Every complete snapshot then reads byte-exact against its
// world through every gateway, and no chunk a sync could not read was collected while it
// was held (I-179, I-181, I-185).
// Users (docs/21): the cluster has two users from the start, kim an admin and lee a plain
// user, every gateway operation runs as kim through the vfs view, so every write, remove,
// rename, restore and purge carries its actor through the WAL and the FSM under every
// fault; a step grants or revokes lee's Write on /d and, while lee lacks it, has lee try
// a write there, which is refused and changes nothing (I-349, I-353).
// Invariant: I-4, I-9, I-12, I-13, I-15, I-29, I-152, I-153, I-155, I-179, I-181, I-185, I-349, I-353, I-377
// Lifecycle: L-162, L-163, L-173, L-199, L-200, L-201, L-203, L-204, L-205
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
	kim, lee := clusterUsers(t, c)
	fsOf := func(gw int) *vfs.FS { return c.Nodes[gw].FS.As(kim) }
	leeWrites := false // whether lee holds Write on /d right now
	cm := newClusterMirror(t, c, 0)
	type pendingSync struct {
		sn    meta.Snapshot
		clone *meta.State
		host  *cluster.Node // the host process that holds the clone: a restart loses it
		w     world         // the volume as of the begin's index
		due   int
	}
	var syncing *pendingSync
	snapWorlds := map[string]world{} // complete snapshots → the world as of their index
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
	// trashed: the bytes the model believes each trashed inode holds (a remove or a
	// rename over a name trashes it, docs/02). An entry whose operation failed with an
	// unknown outcome may describe a live inode: a restore or purge of it then fails
	// with ErrNotExist, and the quiescence drops it when the FSM's trash lacks it.
	trashed := map[meta.Ino][]byte{}
	inoOf := func(gw int, name string) (meta.Ino, bool) {
		in, err := c.Nodes[gw].Node.FSM.Resolve(strings.Split(strings.TrimPrefix(name, "/"), "/"))
		return in.Ino, err == nil
	}
	knownTrash := func() []meta.Ino {
		inos := make([]meta.Ino, 0, len(trashed))
		for ino := range trashed {
			inos = append(inos, ino)
		}
		slices.Sort(inos)
		return inos
	}
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

	// finishSync runs the copy of the pending snapshot as its host's worker would, and ends
	// it: complete, or failed when the host restarted (its clone is gone) or the mirror is
	// offline (docs/16 §3).
	finishSync := func(step int) {
		p := syncing
		var man *mirror.Manifest
		switch {
		case !c.Alive(cm.host) || c.Nodes[cm.host].Node != p.host:
			log("%d sync %s: its host restarted, the clone is gone", step, p.sn.Name)
		case cm.offline.Load():
			log("%d sync %s: the mirror is offline", step, p.sn.Name)
		default:
			m, err := cm.sync(p.sn, p.clone)
			if err != nil {
				log("%d sync %s failed: %v", step, p.sn.Name, err)
				break
			}
			man = &m
			for _, h := range m.Missing { // unreadable is allowed (nodes are down); collected is not (I-181)
				if _, ok := c.Nodes[cm.host].Node.FSM.GetChunk(meta.Hash(h)); !ok {
					t.Fatalf("step %d: chunk %x of snapshot %s was collected while its sync held it", step, h[:6], p.sn.Name)
				}
			}
		}
		if err := cm.end(p.sn, man); err != nil && !errors.Is(err, meta.ErrNotExist) {
			log("%d end %s failed: %v (retried)", step, p.sn.Name, err)
			return
		}
		if row, ok := c.Leader().Node.FSM.SnapshotRow(p.sn.ID); ok && row.State == meta.SnapComplete {
			snapWorlds[p.sn.Name] = p.w
			log("%d snapshot %s complete (%d files, %d new bytes)", step, p.sn.Name, row.Files, row.NewBytes)
		} else {
			log("%d snapshot %s ended without completing (%+v)", step, p.sn.Name, row)
		}
		syncing = nil
	}

	doStep := func(step int) {
		gws := liveGateways()
		if syncing != nil && step >= syncing.due && !partitioned {
			finishSync(step)
		}
		if rng.Intn(25) == 0 && !partitioned { // the mirror's disk comes and goes; GC runs, held by a running sync
			if rng.Intn(3) == 0 {
				cm.offline.Store(!cm.offline.Load())
				log("%d mirror offline=%v", step, cm.offline.Load())
				return
			}
			leader := c.Leader()
			c.Clk.Advance(11 * time.Minute)
			c.Probe()
			res, err := repair.NewGC(leader.Node.FSM, leader.Node, leader.Client, leader.Prober, c.Clk, 10*time.Minute, nil).RunOnce(ctx)
			log("%d gc: %+v %v (sync running: %v)", step, res, err, syncing != nil)
			return
		}
		switch r := rng.Intn(100); {
		case r < 52 && len(gws) > 0: // client write at a random offset through the file's gateway
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
			fh, err := fsOf(gw).OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
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
		case r < 58 && len(gws) > 0: // truncate (an overlay operation: no proposal, so never ambiguous)
			name := names[rng.Intn(len(names))]
			if !exists(name) {
				return
			}
			gw, ok := pickGateway(name, gws)
			if !ok {
				return
			}
			size := rng.Intn(len(worlds[0][name]) + cs)
			fh, err := fsOf(gw).OpenFile(name, os.O_WRONLY, 0)
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
		case r < 64 && len(gws) > 0: // remove: the file lands in the trash with its bytes (L-199)
			gw := gws[rng.Intn(len(gws))]
			name := names[rng.Intn(len(names))]
			if !exists(name) || frozen[name] {
				return
			}
			ino, resolved := inoOf(gw, name) // the inode this gateway would trash
			content := worlds[0][name]
			if err := fsOf(gw).Remove(name); err != nil {
				if definite(err) {
					log("%d remove %s via n%d: %v (stale gateway, nothing happened)", step, name, gw, err)
					return
				}
				log("%d remove %s via n%d failed: %v (outcome unknown)", step, name, gw, err)
				ambiguous(func(w world) { delete(w, name) }, name)
				if resolved {
					trashed[ino] = content
				}
				return
			}
			for _, w := range worlds {
				delete(w, name)
			}
			if resolved {
				trashed[ino] = content
			}
			delete(owner, name)
			frozen[name] = true // other gateways may still resolve the old inode (docs/02 §stale window)
			log("%d remove %s via n%d (ino %d to the trash)", step, name, gw, ino)
		case r < 70 && len(gws) > 0: // rename over another name: the replaced file lands in the trash (L-205)
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
			replaced, hadTo := inoOf(gw, to)
			replacedContent := worlds[0][to]
			if err := fsOf(gw).Rename(from, to); err != nil {
				if definite(err) {
					log("%d rename %s -> %s via n%d: %v (stale gateway, nothing happened)", step, from, to, gw, err)
					return
				}
				log("%d rename %s -> %s via n%d failed: %v (outcome unknown)", step, from, to, gw, err)
				ambiguous(mv, from, to)
				if hadTo {
					trashed[replaced] = replacedContent
				}
				return
			}
			for _, w := range worlds {
				mv(w)
			}
			if hadTo {
				trashed[replaced] = replacedContent
			}
			if g, ok := owner[from]; ok { // the dirty data follows the inode
				owner[to] = g
			} else {
				delete(owner, to)
			}
			delete(owner, from)
			frozen[from], frozen[to] = true, true
			log("%d rename %s -> %s via n%d", step, from, to, gw)
		case r < 73: // restore a trashed inode under a free name (L-200)
			inos := knownTrash()
			if len(inos) == 0 || partitioned {
				return
			}
			ino := inos[rng.Intn(len(inos))]
			name := names[rng.Intn(len(names))]
			if frozen[name] || exists(name) {
				return // the name is taken or in its stale window; try another step
			}
			leader := c.Leader()
			parts := strings.Split(strings.TrimPrefix(name, "/"), "/")
			parent, err := leader.Node.FSM.Resolve(parts[:len(parts)-1])
			if err != nil {
				return // the directory is not there yet
			}
			content := trashed[ino]
			res, _, err := leader.Node.Propose(ctx, meta.Command{Op: meta.OpRestore, Actor: kim, Restore: &meta.Restore{Ino: ino, NewParent: parent.Ino, NewName: parts[len(parts)-1], Now: c.Clk.Now().UnixNano()}})
			if err == nil {
				err = res.Err
			}
			if err != nil {
				if definite(err) {
					log("%d restore ino %d as %s: %v (nothing happened)", step, ino, name, err)
					if errors.Is(err, meta.ErrNotExist) {
						delete(trashed, ino) // live after an ambiguous remove, or purged
					}
					return
				}
				log("%d restore ino %d as %s failed: %v (outcome unknown)", step, ino, name, err)
				ambiguous(func(w world) { w[name] = content }, name)
				return
			}
			for _, w := range worlds {
				w[name] = content
			}
			delete(trashed, ino)
			delete(owner, name)
			frozen[name] = true // its dirty bytes, if any, sit in the gateway that wrote them (L-203)
			log("%d restore ino %d as %s", step, ino, name)
		case r < 75: // purge one trashed inode, or everything in the trash (L-201)
			inos := knownTrash()
			if len(inos) == 0 || partitioned {
				return
			}
			cmd := meta.Command{Op: meta.OpPurge, Actor: kim, Purge: &meta.Purge{Now: c.Clk.Now().UnixNano()}}
			ino := inos[rng.Intn(len(inos))]
			if rng.Intn(4) == 0 {
				cmd.Purge.Before = max(c.Clk.Now().UnixNano(), c.Leader().Node.FSM.LastNow()) + 1
				ino = 0
				cmd.Actor, cmd.System = 0, true // the sweep is the node's (docs/21 §4)
			} else {
				cmd.Purge.Ino = ino
			}
			res, _, err := c.Leader().Node.Propose(ctx, cmd)
			if err == nil {
				err = res.Err
			}
			if err != nil {
				log("%d purge ino %d failed: %v (the quiescence resolves it)", step, ino, err)
				if errors.Is(err, meta.ErrNotExist) {
					delete(trashed, ino)
				}
				return
			}
			if ino == 0 {
				clear(trashed)
				log("%d purge all", step)
				return
			}
			delete(trashed, ino)
			log("%d purge ino %d", step, ino)
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
		case r < 96:
			// lee's Write on /d comes and goes; without it a write by lee is refused and
			// changes nothing (docs/21 §3, §4)
			if partitioned {
				return
			}
			var grantIdx uint64
			if d, err := c.Leader().Node.FSM.Resolve([]string{"d"}); err == nil {
				role := meta.RoleNone
				if !leeWrites {
					role = meta.RoleWrite
				}
				res, index, err := c.Leader().Node.Propose(ctx, meta.Command{Op: meta.OpGrant, Actor: kim, GrantOp: &meta.GrantOp{Ino: d.Ino, Subject: meta.Subject{Kind: meta.SubjectUser, Idx: uint16(lee)}, Role: role, Now: c.Clk.Now().UnixNano()}})
				if err == nil && (res.Err == nil || (role == meta.RoleNone && errors.Is(res.Err, meta.ErrNotExist))) {
					leeWrites = !leeWrites
					grantIdx = index
					log("%d lee %s on /d", step, map[bool]string{true: "granted write", false: "revoked"}[leeWrites])
				}
			}
			if leeWrites {
				return
			}
			gws := liveGateways()
			if len(gws) == 0 {
				return
			}
			gw := gws[rng.Intn(len(gws))]
			// a view checks its own replica: the revoke takes effect on this gateway once
			// its FSM holds it (docs/21 §4), and a restarted node may still be catching up
			if grantIdx != 0 {
				if err := c.Nodes[gw].Node.FSM.WaitApplied(ctx, grantIdx); err != nil {
					return // the gateway is not caught up: nothing to assert this step
				}
			}
			fh, err := c.Nodes[gw].FS.As(lee).OpenFile("/d/c", os.O_CREATE|os.O_RDWR, 0o644)
			if err == nil {
				_, err = fh.Write([]byte("lee"))
				_ = fh.Close()
			}
			if err == nil || (!errors.Is(err, syscall.EACCES) && !errors.Is(err, os.ErrPermission) && !errors.Is(err, os.ErrNotExist)) {
				t.Fatalf("step %d: lee's write on /d/c without Write on /d: %v", step, err)
			}
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
			var hosts []int // a disk belongs to an Active host (docs/14 §1): never the draining one
			for _, g := range liveGateways() {
				if g != draining {
					hosts = append(hosts, g)
				}
			}
			if len(disks) < 4 && len(hosts) > 0 {
				i := hosts[rng.Intn(len(hosts))]
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
			checkTrash(t, c, trashed, step)
			owner = map[string]int{}
			frozen = map[string]bool{}
			// snapshots: end a syncing row the run lost track of (an ambiguous begin),
			// read every complete one, and begin the next as of this quiescence
			cm.offline.Store(false)
			cm.attach()
			for _, sn := range c.Leader().Node.FSM.Snapshots() {
				if sn.State == meta.SnapSyncing && (syncing == nil || syncing.sn.ID != sn.ID) {
					_ = cm.end(sn, nil)
				}
			}
			c.Settle()
			checkSnapshots(t, c, cm, snapWorlds, names, step)
			if syncing == nil {
				name := fmt.Sprintf("s%d", step)
				if sn, clone, err := cm.begin(name); err == nil {
					syncing = &pendingSync{sn: sn, clone: clone, host: c.Nodes[cm.host].Node, w: worlds[0].clone(), due: step + 1 + rng.Intn(20)}
					log("%d begin snapshot %s at index %d", step, name, sn.Index)
				} else {
					log("%d begin snapshot %s failed: %v", step, name, err)
				}
			}
		}
	}
}

// checkSnapshots reads every complete snapshot the model knows through every gateway
// and compares it with the world as of its index (I-179, I-185); a name the world lacks
// must not be in it.
func checkSnapshots(t *testing.T, c *testharness.Cluster, cm *clusterMirror, snaps map[string]world, names []string, step int) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(snaps)) {
		w := snaps[name]
		for i := range c.Nodes {
			for _, f := range names {
				got, err := c.ReadFile(i, cm.path(name, f))
				switch {
				case w[f] == nil && err == nil:
					t.Fatalf("step %d: n%d snapshot %s holds %s, removed before its begin", step, i, name, f)
				case w[f] == nil:
				case err != nil:
					t.Fatalf("step %d: n%d snapshot %s %s: %v", step, i, name, f, err)
				case !bytes.Equal(got, w[f]):
					t.Fatalf("step %d: n%d snapshot %s %s differs at %d (len %d want %d)", step, i, name, f, testharness.FirstDiff(got, w[f]), len(got), len(w[f]))
				}
			}
		}
	}
}

// checkTrash reads every trashed root the model knows on every node and compares it with
// the bytes the model holds for it (I-155); a known inode the FSM's trash lacks was purged
// or never trashed (an ambiguous failure) and leaves the model. The FSMs agree with each
// other already (checkInvariants, I-4), so the roots come from n0.
func checkTrash(t *testing.T, c *testharness.Cluster, trashed map[meta.Ino][]byte, step int) {
	t.Helper()
	inTrash := map[meta.Ino]bool{}
	roots := c.Nodes[0].Node.FSM.Trash()
	for i := range roots {
		inTrash[roots[i].Ino] = true
	}
	for ino, want := range trashed {
		if !inTrash[ino] {
			delete(trashed, ino)
			continue
		}
		for i := range c.Nodes {
			if _, ok := c.Nodes[i].Node.FSM.GetInode(ino); ok {
				t.Fatalf("step %d: n%d serves trashed ino %d as live", step, i, ino)
			}
			fh, err := c.Nodes[i].FS.OpenTrash(ino, nil) // the node's own reader: no role to check
			if err != nil {
				t.Fatalf("step %d: n%d open trashed ino %d: %v", step, i, ino, err)
			}
			got := make([]byte, len(want)+1)
			n, _ := fh.ReadAt(got, 0)
			_ = fh.Close()
			if !bytes.Equal(got[:n], want) {
				t.Fatalf("step %d: n%d trashed ino %d differs at %d (len %d want %d)", step, i, ino, testharness.FirstDiff(got[:n], want), n, len(want))
			}
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
		if v := c.Nodes[i].Node.FSM.CheckClasses(); len(v) != 0 {
			t.Fatalf("step %d: n%d placement classes: %v", step, i, v)
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

// clusterUsers makes kim the first admin and lee a plain user on the cluster (docs/21),
// through the leader, and answers their indices.
func clusterUsers(t *testing.T, c *testharness.Cluster) (kim, lee meta.UserIdx) {
	t.Helper()
	l := c.Leader().Node
	now := c.Clk.Now().UnixNano()
	propose := func(cmd meta.Command) meta.Result {
		t.Helper()
		res, index, err := l.Propose(ctx, cmd)
		if err == nil {
			err = res.Err
		}
		if err != nil {
			t.Fatalf("op %d: %v", cmd.Op, err)
		}
		if err := l.FSM.WaitApplied(ctx, index); err != nil {
			t.Fatal(err)
		}
		return res
	}
	propose(meta.Command{Op: meta.OpInvite, InviteOp: &meta.InviteOp{Bootstrap: true, Invite: meta.Invite{Hash: meta.HashCode("aaaaaaaaaa"), Name: "kim", Kind: meta.InviteUser, ExpiresAt: now + int64(time.Hour)}, Now: now}})
	kim = meta.UserIdx(propose(meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserCreate, Name: "kim", Code: "aaaaaaaaaa", Key: [32]byte{1}, Now: now}}).Ino)
	propose(meta.Command{Op: meta.OpInvite, Actor: kim, InviteOp: &meta.InviteOp{Invite: meta.Invite{Hash: meta.HashCode("bbbbbbbbbb"), Name: "lee", Kind: meta.InviteUser, ExpiresAt: now + int64(time.Hour)}, Now: now}})
	lee = meta.UserIdx(propose(meta.Command{Op: meta.OpUser, UserOp: &meta.UserOp{Action: meta.UserCreate, Name: "lee", Code: "bbbbbbbbbb", Key: [32]byte{2}, Now: now}}).Ino)
	c.Settle()
	return kim, lee
}
