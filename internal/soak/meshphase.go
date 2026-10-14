package soak

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/mesh"
	"github.com/zombar/tailraid/internal/meta"
)

// --- 25 mesh join, 26 mesh paths -------------------------------------------------------
//
// docs/20 on a running cluster (docs/09 §6). Phase 25: a late device joins by a code
// printed on a non-bootstrap node while the bootstrap is down (the pending row, the join,
// a first handshake with every member, the log caught up); a rotated token refuses the
// old code; a reset and a re-join keep the device's index, address and disks; its removal
// drops it from every member's device. Phase 26: every pair direct; a UDP-only cut and
// its recovery; a restart that keeps the pinned endpoints. Both run only where the nodes
// have a device of their own (Caps.Mesh); a live cluster observes its paths and no more.

// MeshEnv is what the mesh phases ask of an environment beyond Env: the join code of a
// node, a late device joining by code, a throwaway join attempt, a reset and re-join, and
// a UDP-only cut between two nodes (Heal lifts it).
type MeshEnv interface {
	// JoinCode is `tailraid token` on n: the current token with n's key and endpoints.
	JoinCode(ctx context.Context, n NodeRef) (string, error)
	// RotateToken is `tailraid token rotate` through n; it returns the new code.
	RotateToken(ctx context.Context, n NodeRef) (string, error)
	// LateJoin starts the environment's late device, which joins by code, and returns it
	// once ready; RemoveLate stops it and forgets its state.
	LateJoin(ctx context.Context, code string) (NodeRef, error)
	RemoveLate(ctx context.Context) error
	// TryJoin is one join attempt by a throwaway device with the code: the command's
	// output and its failure, if any.
	TryJoin(ctx context.Context, code string) (string, error)
	// RejoinAfterReset wipes n's local Raft state and identity (`tailraid reset`) and
	// joins it again by code, as a device re-installed would; it returns once n is ready.
	RejoinAfterReset(ctx context.Context, n NodeRef, code string) error
	// CutUDP drops the UDP between a and b both ways and leaves TCP alone.
	CutUDP(ctx context.Context, a, b NodeRef) error
}

// The mesh phases' events in results.jsonl: the join round trip (attrs: the late node,
// the members that showed its pending row, and the milliseconds to the pending row, the
// join, the first handshake with every member and the log caught up), and the paths
// table (attrs: "a>b" → "class endpoint handshake").
const (
	meshJoinEvent  = "mesh join"
	meshPathsEvent = "mesh paths"
)

// direct is a path class the storage code treats as a direct connection.
func direct(class string) bool {
	return class == mesh.PathLAN.String() || class == mesh.PathWAN.String()
}

// devices are the status report's active device rows, by name.
func devices(rep admin.Report) map[string]admin.NodeReport {
	out := map[string]admin.NodeReport{}
	for i := range rep.Nodes {
		if n := &rep.Nodes[i]; n.Kind == "device" && n.State != "removed" {
			out[n.Name] = *n
		}
	}
	return out
}

// peersOf is a node's mesh peers by member name, from its own status.
func peersOf(rep admin.Report) map[string]admin.MeshPeer {
	out := map[string]admin.MeshPeer{}
	if rep.Mesh == nil {
		return out
	}
	for _, p := range rep.Mesh.Peers {
		if p.Name != "" {
			out[p.Name] = p
		}
	}
	return out
}

// handshakeAge parses the status report's seen age (since the last packet or handshake
// from the peer, the path monitor's sign of life; a handshake alone renews only every
// two minutes); ok is false for a peer never heard from.
func handshakeAge(p admin.MeshPeer) (time.Duration, bool) {
	age := p.SeenAge
	if age == "" {
		age = p.HandshakeAge
	}
	if age == "" {
		return 0, false
	}
	d, err := time.ParseDuration(age)
	return d, err == nil
}

// meshSample is the sampler's view of the mesh (docs/20 §10): every node's path class to
// every other device and its counters, from each node's own status.
func (r *Run) meshSample(ctx context.Context) (paths map[string]string, counters map[string][2]uint64) {
	nodes, err := r.Env.Nodes(ctx)
	if err != nil {
		return nil, nil
	}
	paths, counters = map[string]string{}, map[string][2]uint64{}
	for _, n := range nodes {
		rep, err := r.Env.Status(ctx, n)
		if err != nil || rep.Mesh == nil {
			continue
		}
		counters[n.Name] = [2]uint64{rep.Mesh.RxBytes, rep.Mesh.TxBytes}
		for name, p := range peersOf(rep) {
			paths[n.Name+">"+name] = p.Class
		}
	}
	return paths, counters
}

// pollUntil polls cond every 250 ms until it answers true or the deadline passes; it
// returns how long it took and whether cond was met.
func (r *Run) pollUntil(ctx context.Context, within time.Duration, cond func() bool) (time.Duration, bool) {
	start := time.Now()
	for {
		if cond() {
			return time.Since(start), true
		}
		if time.Since(start) > within {
			return time.Since(start), false
		}
		if err := r.Env.Wait(ctx, 250*time.Millisecond); err != nil {
			return time.Since(start), false
		}
	}
}

// --- 25 -------------------------------------------------------------------------------

func phaseMeshJoin(ctx context.Context, r *Run) error {
	me, ok := r.Env.(MeshEnv)
	if !ok || !r.Env.Caps().Mesh {
		r.skip("the environment has no mesh of its own to join (a live cluster is only observed)")
		return nil
	}
	if !r.Env.Caps().Kill || !r.Env.Caps().Mutate {
		r.skip("needs Kill and Mutate")
		return nil
	}
	nodes, err := r.Env.Nodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) < 3 {
		r.skip("needs three nodes: the bootstrap to kill, one to print the code and one to rotate the token")
		return nil
	}
	bootstrap, printer, rotator := nodes[0], nodes[1], nodes[2]
	u := r.undoer(ctx)
	defer u.run()
	u.add("remove the late device", me.RemoveLate)

	// (a) the bootstrap down, a code printed on another member
	u.add("start "+bootstrap.Name, func(c context.Context) error { return r.Env.Start(c, bootstrap) })
	if err := r.Env.Kill(ctx, bootstrap); err != nil {
		return err
	}
	code, err := me.JoinCode(ctx, printer)
	r.check("I-321", "a join code printed on "+printer.Name+" with the bootstrap down", err == nil && code != "", strings.TrimPrefix(errText(err), "; "))
	if err != nil {
		return nil
	}
	leaderRep, err := r.Env.Status(ctx, printer)
	if err != nil {
		return err
	}
	var leaderApplied uint64
	for i := range leaderRep.Nodes {
		if leaderRep.Nodes[i].Leader {
			leaderApplied = leaderRep.Nodes[i].Applied
		}
	}

	// (b) the late device joins: the pending row on the members, the join, the first
	// handshakes, the log caught up
	members := nodes[1:]
	start := time.Now()
	type seen struct {
		at    time.Duration
		nodes []string
	}
	pendingCh := make(chan seen, 1)
	pctx, pstop := context.WithCancel(ctx)
	go func() { // watch the members' status for the pending row while the join runs
		var out seen
		for pctx.Err() == nil {
			for _, m := range members {
				rep, err := r.Env.Status(pctx, m)
				if err != nil || len(rep.Pending) == 0 {
					continue
				}
				if out.at == 0 {
					out.at = time.Since(start)
				}
				if !slices.Contains(out.nodes, m.Name) {
					out.nodes = append(out.nodes, m.Name)
				}
			}
			select {
			case <-pctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
		pendingCh <- out
	}()
	late, err := me.LateJoin(ctx, code)
	joinTook := time.Since(start)
	pstop()
	pending := <-pendingCh
	if err != nil {
		r.check("I-321", "the late device joined by the code", false, err.Error())
		return nil
	}
	r.check("I-321", "the late device joined by the code with the bootstrap down", true, fmt.Sprintf("%s joined as node %d in %s", late.Name, late.Idx, joinTook.Round(time.Millisecond)))
	sort.Strings(pending.nodes)
	r.check("I-319", "the pending row seen on a member before the join", len(pending.nodes) > 0,
		fmt.Sprintf("seen on %d of %d members (%s) after %s", len(pending.nodes), len(members), strings.Join(pending.nodes, ", "), pending.at.Round(time.Millisecond)))
	var lateRep admin.Report
	hsTook, hsOK := r.pollUntil(ctx, 10*time.Second, func() bool {
		rep, err := r.Env.Status(ctx, late)
		if err != nil {
			return false
		}
		lateRep = rep
		peers := peersOf(rep)
		for _, m := range members {
			p, ok := peers[m.Name]
			if _, shook := handshakeAge(p); !ok || !shook || !direct(p.Class) {
				return false
			}
		}
		return true
	})
	r.check("I-321, I-323", "the late device shook hands with every member within 10 s", hsOK, describePeers(lateRep, members)+" after "+hsTook.Round(time.Millisecond).String())
	logTook, logOK := r.pollUntil(ctx, 60*time.Second, func() bool {
		rep, err := r.Env.Status(ctx, printer)
		if err != nil {
			return false
		}
		d := devices(rep)[late.Name]
		return d.Live && d.Applied >= leaderApplied
	})
	r.check("I-321", "the late device's log caught up", logOK, fmt.Sprintf("applied ≥ %d after %s", leaderApplied, logTook.Round(time.Millisecond)))
	r.emit(Record{Kind: "event", Name: meshJoinEvent, Text: late.Name, Attrs: map[string]string{
		"late": late.Name, "pending_ms": strconv.FormatInt(pending.at.Milliseconds(), 10), "pending_on": strings.Join(pending.nodes, " "),
		"join_ms": strconv.FormatInt(joinTook.Milliseconds(), 10), "handshake_ms": strconv.FormatInt((joinTook + hsTook).Milliseconds(), 10),
		"log_ms": strconv.FormatInt((joinTook + hsTook + logTook).Milliseconds(), 10),
	}})
	if err := r.Env.Start(ctx, bootstrap); err != nil {
		return err
	}
	u.done("start " + bootstrap.Name)

	// (c) the token rotated on a third member: the old code gets silence. The rotation
	// is a leader op, retried while the leader the bootstrap's return unsettled is found.
	var newCode string
	_, _ = r.pollUntil(ctx, 30*time.Second, func() bool {
		newCode, err = me.RotateToken(ctx, rotator)
		return err == nil
	})
	r.check("I-319", "token rotated through "+rotator.Name, err == nil && newCode != "" && newCode != code, strings.TrimPrefix(errText(err), "; "))
	if err == nil {
		out, jerr := me.TryJoin(ctx, code)
		refused := jerr != nil && strings.Contains(out, "introduce")
		r.check("I-319", "a join with the old code is refused with the token message", refused, strings.TrimSpace(lastLine(out)))
		none := true
		for _, m := range nodes {
			if rep, err := r.Env.Status(ctx, m); err == nil && len(rep.Pending) > 0 {
				none = false
			}
		}
		why := ""
		if !none {
			why = "a member lists a pending row"
		}
		r.check("I-319", "no pending row after the refused introduce", none, why)
	} else {
		newCode = code
	}

	// (d) a reset and a re-join keep the device's index, address and disks
	before, err := r.Env.Status(ctx, printer)
	if err != nil {
		return err
	}
	bdev := devices(before)[late.Name]
	bdisks := disksOf(before, late.Idx)
	err = me.RejoinAfterReset(ctx, late, newCode)
	after, serr := r.Env.Status(ctx, printer)
	adev := devices(after)[late.Name]
	same := err == nil && serr == nil && adev.Idx == bdev.Idx && adev.Addr == bdev.Addr && adev.ID != bdev.ID && slices.Equal(disksOf(after, late.Idx), bdisks)
	r.check("I-321, I-94", "a reset device re-joins under its key with the same index, address and disks",
		same, fmt.Sprintf("before idx %d addr %s id %s disks %v; after idx %d addr %s id %s disks %v%s", bdev.Idx, bdev.Addr, short(bdev.ID), bdisks, adev.Idx, adev.Addr, short(adev.ID), disksOf(after, late.Idx), errText(errors.Join(err, serr))))

	// (e) drained, then removed: every member's device drops it, its own view goes dark
	if err := r.Env.SetState(ctx, late, meta.Draining); err != nil {
		r.check("I-318", "drain of the late device", false, err.Error())
		return nil
	}
	rep, err := r.repairUntilClean(ctx)
	if err != nil {
		return err
	}
	// a host leaves after its disks (docs/14 §2): each drained disk row goes first
	for i := range rep.Nodes {
		if d := &rep.Nodes[i]; d.Kind == "disk" && d.Host == late.Idx && d.State != "removed" {
			if err := r.Env.Remove(ctx, NodeRef{Name: d.Name, Idx: d.Idx, Host: late.Name}); err != nil {
				r.check("I-318", "node remove of the late device's disk "+d.Name, false, err.Error())
				return nil
			}
		}
	}
	if err := r.Env.Remove(ctx, late); err != nil {
		r.check("I-318", "node remove of the late device", false, err.Error())
		return nil
	}
	lateKey := ""
	if lateRep.Mesh != nil {
		lateKey = lateRep.Mesh.Key
	}
	dropTook, dropped := r.pollUntil(ctx, 30*time.Second, func() bool {
		for _, m := range nodes {
			rep, err := r.Env.Status(ctx, m)
			if err != nil {
				return false
			}
			if rep.Mesh == nil {
				continue
			}
			for _, p := range rep.Mesh.Peers {
				if p.Key == lateKey || p.Name == late.Name {
					return false
				}
			}
		}
		return true
	})
	r.check("I-318, I-317", "every member's device dropped the removed peer", dropped, "after "+dropTook.Round(time.Millisecond).String())
	darkTook, dark := r.pollUntil(ctx, mesh.PathDead+60*time.Second, func() bool {
		rep, err := r.Env.Status(ctx, late)
		if err != nil {
			return false
		}
		peers, devs := peersOf(rep), devices(rep)
		for _, m := range nodes {
			if p, ok := peers[m.Name]; ok && p.Class != mesh.PathNone.String() {
				return false
			}
			if d, ok := devs[m.Name]; ok && d.Live {
				return false
			}
		}
		return true
	})
	r.check("I-318", "the removed device sees every member none and dead (its packets die at the tunnel, no 403)", dark, "after "+darkTook.Round(time.Second).String())
	return nil
}

// disksOf lists the disk rows of a host, as "idx:id", sorted.
func disksOf(rep admin.Report, host meta.NodeIdx) []string {
	var out []string
	for i := range rep.Nodes {
		if n := &rep.Nodes[i]; n.Kind == "disk" && n.Host == host && n.State != "removed" {
			out = append(out, fmt.Sprintf("%d:%s", n.Idx, short(n.ID)))
		}
	}
	sort.Strings(out)
	return out
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// describePeers is one node's view of the members' paths, for a check's text.
func describePeers(rep admin.Report, members []NodeRef) string {
	peers := peersOf(rep)
	parts := make([]string, 0, len(members))
	for _, m := range members {
		p, ok := peers[m.Name]
		switch {
		case !ok:
			parts = append(parts, m.Name+" absent")
		case p.HandshakeAge == "" && p.SeenAge == "":
			parts = append(parts, m.Name+" "+p.Class+" (never heard from)")
		default:
			parts = append(parts, fmt.Sprintf("%s %s %s seen %s ago (handshake %s)", m.Name, p.Class, p.Endpoint, p.SeenAge, p.HandshakeAge))
		}
	}
	return strings.Join(parts, ", ")
}

// --- 26 -------------------------------------------------------------------------------

func phaseMeshPaths(ctx context.Context, r *Run) error {
	if !r.Env.Caps().Mesh {
		r.skip("the nodes have no mesh device of their own")
		return nil
	}
	nodes, err := r.Env.Nodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) < 2 {
		r.skip("one node: no pair")
		return nil
	}

	// (a) every pair direct, pinned, with a recent handshake
	attrs := map[string]string{}
	for _, n := range nodes {
		rep, err := r.Env.Status(ctx, n)
		if err != nil {
			r.check("I-323, I-325", "paths from "+n.Name, false, err.Error())
			continue
		}
		peers := peersOf(rep)
		ok := true
		for _, m := range nodes {
			if m.Name == n.Name {
				continue
			}
			p, found := peers[m.Name]
			age, shook := handshakeAge(p)
			attrs[n.Name+">"+m.Name] = fmt.Sprintf("%s %s %s", p.Class, p.Endpoint, p.SeenAge)
			if !found || !direct(p.Class) || p.Endpoint == "" || !shook || age > 2*time.Minute {
				ok = false
			}
		}
		others := slices.DeleteFunc(slices.Clone(nodes), func(m NodeRef) bool { return m.Name == n.Name })
		r.check("I-323, I-325", "every pair direct and pinned from "+n.Name, ok, describePeers(rep, others))
	}
	r.emit(Record{Kind: "event", Name: meshPathsEvent, Text: "every pair", Attrs: attrs})

	me, ok := r.Env.(MeshEnv)
	caps := r.Env.Caps()
	if !ok || !caps.Partition || !caps.Mutate || !caps.Kill || len(nodes) < 4 {
		r.checkSkipped("I-323", "a UDP cut and its recovery", "needs a MeshEnv with Partition, Mutate, Kill and four nodes")
		return nil
	}
	a, b, c, d := nodes[0], nodes[1], nodes[2], nodes[3]
	u := r.undoer(ctx)
	defer u.run()

	// (b) the cut: the pair goes dark both ways, each marks the other dead, the rest serve
	u.add("heal", r.Env.Heal)
	if err := me.CutUDP(ctx, a, b); err != nil {
		return err
	}
	r.event("partition", fmt.Sprintf("UDP cut %s↔%s", a.Name, b.Name))
	view := func(from, to NodeRef) (class string, live bool, ok bool) {
		rep, err := r.Env.Status(ctx, from)
		if err != nil {
			return "", false, false
		}
		p := peersOf(rep)[to.Name]
		return p.Class, devices(rep)[to.Name].Live, true
	}
	darkTook, dark := r.pollUntil(ctx, mesh.PathDead+60*time.Second, func() bool {
		ab, _, ok1 := view(a, b)
		ba, _, ok2 := view(b, a)
		return ok1 && ok2 && ab == mesh.PathNone.String() && ba == mesh.PathNone.String()
	})
	r.check("I-323", "the cut pair shows none both ways within the dead-path window", dark, "after "+darkTook.Round(time.Second).String())
	deadTook, dead := r.pollUntil(ctx, 90*time.Second, func() bool {
		_, abLive, ok1 := view(a, b)
		_, baLive, ok2 := view(b, a)
		return ok1 && ok2 && !abLive && !baLive
	})
	r.check("I-323", "each side of the cut marks the other dead", dead, "after "+deadTook.Round(time.Second).String())
	if vc, ok := r.Env.Volume(c); ok {
		path := join(r.Root, "mesh", "through-the-cut.bin")
		data := r.randomBytes(r.Profile.ChunkSize + 77)
		var werr error
		// the cut may have cost the voters an election: a gateway refuses metadata
		// changes while stale (I-109) and serves again once a leader is back
		_, _ = r.pollUntil(ctx, 60*time.Second, func() bool {
			werr = vc.Mkdir(join(r.Root, "mesh"))
			if werr == nil {
				werr = vc.WriteAt(path, 0, data)
			}
			return werr == nil
		})
		var got []byte
		if werr == nil {
			if vd, ok := r.Env.Volume(d); ok {
				_, _ = r.pollUntil(ctx, 30*time.Second, func() bool {
					got, _ = vd.ReadAll(path)
					return len(got) == len(data)
				})
			}
		}
		r.check("I-323", "a write through "+c.Name+" reads back on "+d.Name+" during the cut", werr == nil && slices.Equal(got, data), strings.TrimPrefix(errText(werr), "; "))
	}

	// (c) the cut lifted: direct again, no restart
	if err := r.Env.Heal(ctx); err != nil {
		return err
	}
	u.done("heal")
	r.event("heal", "UDP cut lifted")
	backTook, back := r.pollUntil(ctx, 60*time.Second, func() bool {
		ab, _, ok1 := view(a, b)
		ba, _, ok2 := view(b, a)
		return ok1 && ok2 && direct(ab) && direct(ba)
	})
	r.check("I-323", "the pair is direct again within 30 s of the cut lifted", back && backTook <= 35*time.Second, "after "+backTook.Round(time.Second).String())

	// (d) a restart keeps the pinned endpoints and re-handshakes at once
	endpoints := map[string]string{}
	for _, n := range nodes {
		if n.Name == b.Name {
			continue
		}
		if rep, err := r.Env.Status(ctx, n); err == nil {
			endpoints[n.Name] = peersOf(rep)[b.Name].Endpoint
		}
	}
	if err := r.Env.Restart(ctx, []NodeRef{b}); err != nil {
		return err
	}
	r.event("restart", b.Name)
	var why []string
	hsTook, hsOK := r.pollUntil(ctx, 10*time.Second, func() bool {
		why = why[:0]
		good := true
		for _, n := range nodes {
			if n.Name == b.Name {
				continue
			}
			rep, err := r.Env.Status(ctx, n)
			if err != nil {
				return false
			}
			p := peersOf(rep)[b.Name]
			age, shook := handshakeAge(p)
			if !shook || age > 10*time.Second || !direct(p.Class) || p.Endpoint != endpoints[n.Name] {
				good = false
				why = append(why, fmt.Sprintf("%s sees %s %s %s (was %s) %s", n.Name, b.Name, p.Class, p.Endpoint, endpoints[n.Name], p.HandshakeAge))
			}
		}
		return good
	})
	r.check("I-323", "after a restart the others keep "+b.Name+"'s endpoint and re-handshake within 10 s", hsOK, strings.TrimSpace(strings.Join(why, "; ")+" after "+hsTook.Round(time.Millisecond).String()))
	return nil
}
