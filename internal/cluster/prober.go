package cluster

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// Liveness constants (docs/05 §timings).
const (
	ProbeInterval  = 5 * time.Second
	LiveThreshold  = 15 * time.Second
	StaleThreshold = 30 * time.Second
	FillAfter      = 10 * time.Minute
	EvictAfter     = 7 * 24 * time.Hour
	// a bucket is probed less often and given longer: every probe is a billed request
	// (docs/10 §4.3)
	S3ProbeInterval = 30 * time.Second
	S3LiveThreshold = 90 * time.Second
	s3ProbeTimeout  = 5 * time.Second
)

// HealthState is the gateway's view of the metadata service (docs/02 §Health).
type HealthState int

const (
	Healthy HealthState = iota + 1
	Stale
	NoLeader
)

func (h HealthState) String() string {
	switch h {
	case Healthy:
		return "healthy"
	case Stale:
		return "stale"
	case NoLeader:
		return "no-leader"
	}
	return "unknown"
}

// HealthReport summarises Raft reachability for status output and the gateway.
type HealthReport struct {
	State       HealthState
	Leader      string // raft ID
	LastContact time.Duration
	Voters      int
	LiveVoters  int
}

// EventKind enumerates prober events.
type EventKind int

const (
	MemberUp   EventKind = iota + 1 // probe succeeded after being dead
	MemberDown                      // dead for longer than FillAfter (repair may re-place)
	MemberLost                      // dead for longer than EvictAfter
)

// Event is delivered on Prober.Events.
type Event struct {
	Kind  EventKind
	Idx   meta.NodeIdx
	Since time.Time
}

// Probe is the last observation of one node.
type Probe struct {
	LastOK    time.Time
	RTT       time.Duration
	Free      int64
	Applied   uint64
	Version   string
	Codec     uint8     // meta.CodecVersion the node reported (0 from a binary that predates the field)
	Dev       bool      // /dev/* enabled on that node (docs/08)
	Web       uint16    // the node's browser UI port, 0 when off (docs/13 §1)
	FS        string    // filesystem holding the target's store (docs/14 §8)
	DeadSince time.Time // zero while live
	Down      bool      // MemberDown already emitted
	Lost      bool      // MemberLost already emitted
}

// MirrorProbe is what the mirror's host last said of it (docs/16 §1). Online holds only
// while the host is live and its last reply listed the mirror online; Used, Free and
// Total stay from the last reply that listed it, and LastSeen is when it was last online.
type MirrorProbe struct {
	Health   rpc.MirrorHealth
	Online   bool
	LastSeen time.Time
}

// HealthClient probes a node (rpc.Client).
type HealthClient interface {
	Health(ctx context.Context, addr string) (rpc.Health, error)
}

// S3Prober checks an S3 node's bucket from this device (rpc.Client). A HealthClient
// without it never sees a bucket live.
type S3Prober interface {
	ProbeS3(ctx context.Context, idx meta.NodeIdx) error
}

// Prober keeps an all-to-all liveness view and implements meta.Peers (docs/05 §4).
type Prober struct {
	node    *Node
	client  HealthClient
	clk     clock.Clock
	timeout time.Duration
	// SelfHealth supplies this node's own health row (free bytes, version); optional.
	SelfHealth func() rpc.Health

	mu      sync.Mutex
	probes  map[meta.NodeIdx]*Probe
	s3Tried map[meta.NodeIdx]time.Time    // last S3 probe sent, good or not
	suspect map[meta.NodeIdx]time.Time    // hosts marked suspect, by host idx, until their next good probe (I-170)
	mirrors map[meta.NodeIdx]*MirrorProbe // by mirror row: the host's last /health word on it (docs/16 §1)
	// codecView overrides what MinCodec takes a peer to read (dev.enabled only, SetCodecView)
	codecView map[meta.NodeIdx]uint8
	events    chan Event

	fillAfter, evictAfter time.Duration
}

var _ meta.Peers = (*Prober)(nil)

// NewProber wires a prober for node. client may be nil in single-node setups.
func NewProber(node *Node, client HealthClient, clk clock.Clock) *Prober {
	return &Prober{
		node: node, client: client, clk: clk, timeout: 3 * time.Second, probes: map[meta.NodeIdx]*Probe{}, s3Tried: map[meta.NodeIdx]time.Time{}, mirrors: map[meta.NodeIdx]*MirrorProbe{}, events: make(chan Event, 64),
		fillAfter: FillAfter, evictAfter: EvictAfter,
	}
}

// FillAfter is the MemberDown hold-down in force (repair.fill_after).
func (p *Prober) FillAfter() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fillAfter
}

// SetHoldDowns overrides the MemberDown and MemberLost hold-downs (the configured
// repair.fill_after and evict_after), so the prober's events agree with the repairer.
func (p *Prober) SetHoldDowns(fillAfter, evictAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fillAfter, p.evictAfter = fillAfter, evictAfter
}

// Events delivers MemberUp/Down/Lost; the buffer is bounded and events are dropped,
// never blocking, when nobody reads.
func (p *Prober) Events() <-chan Event { return p.events }

func (p *Prober) emit(e Event) {
	select {
	case p.events <- e:
	default:
	}
}

// Run probes every ProbeInterval until ctx is done.
func (p *Prober) Run(ctx context.Context) {
	p.Tick(ctx)
	t := p.clk.NewTimer(ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
		}
		p.Tick(ctx)
		t.Reset(ProbeInterval)
	}
}

// Tick probes every device member once (exported for tests driving the fake clock). A
// host's reply also settles its disks: live when the host listed them OK (I-90).
func (p *Prober) Tick(ctx context.Context) {
	nodes := p.node.FSM.Nodes()
	now := p.clk.Now()
	disks := map[meta.NodeIdx][]meta.Node{}
	mirrors := map[meta.NodeIdx][]meta.Node{}
	for i := range nodes {
		nd := &nodes[i]
		switch {
		case nd.State == meta.Removed:
		case nd.Kind == meta.KindDisk:
			disks[nd.Host] = append(disks[nd.Host], *nd)
		case nd.Kind == meta.KindMirror:
			mirrors[nd.Host] = append(mirrors[nd.Host], *nd)
		}
	}
	var wg sync.WaitGroup
	for i := range nodes {
		nd := nodes[i]
		if nd.State == meta.Removed { // nothing to probe, nothing to keep
			p.mu.Lock()
			delete(p.probes, nd.Idx)
			delete(p.s3Tried, nd.Idx)
			delete(p.mirrors, nd.Idx)
			p.mu.Unlock()
			continue
		}
		if nd.Kind == meta.KindS3 {
			p.probeS3(ctx, &wg, nd.Idx, now)
			continue
		}
		if nd.Kind != meta.KindDevice {
			continue
		}
		if nd.ID == p.node.ID {
			h := rpc.Health{Applied: p.node.AppliedIndex()}
			if p.SelfHealth != nil {
				h = p.SelfHealth()
			}
			p.record(nd.Idx, now, 0, true, h, nil)
			p.recordDisks(disks[nd.Idx], now, 0, true, h)
			p.recordMirrors(mirrors[nd.Idx], now, true, h)
			continue
		}
		if p.client == nil {
			continue
		}
		addr, _, ok := p.node.RPCAddrOf(nd.Idx)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(idx meta.NodeIdx, addr string, ds, ms []meta.Node) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, p.timeout)
			defer cancel()
			start := p.clk.Now()
			h, err := p.client.Health(pctx, addr)
			at, rtt := p.clk.Now(), p.clk.Now().Sub(start)
			p.record(idx, at, rtt, err == nil, h, nil)
			p.recordDisks(ds, at, rtt, err == nil, h)
			p.recordMirrors(ms, at, err == nil, h)
		}(nd.Idx, addr, disks[nd.Idx], mirrors[nd.Idx])
	}
	wg.Wait()
	p.holdDowns(now)
}

// probeS3 probes a bucket once per S3ProbeInterval, in the tick's wait group.
func (p *Prober) probeS3(ctx context.Context, wg *sync.WaitGroup, idx meta.NodeIdx, now time.Time) {
	s3p, ok := p.client.(S3Prober)
	if !ok {
		return
	}
	p.mu.Lock()
	tried, seen := p.s3Tried[idx]
	due := !seen || now.Sub(tried) >= S3ProbeInterval
	if due {
		p.s3Tried[idx] = now
	}
	p.mu.Unlock()
	if !due {
		return
	}
	wg.Go(func() {
		pctx, cancel := context.WithTimeout(ctx, s3ProbeTimeout)
		defer cancel()
		start := p.clk.Now()
		err := s3p.ProbeS3(pctx, idx)
		at := p.clk.Now()
		p.record(idx, at, at.Sub(start), err == nil, rpc.Health{}, nil)
	})
}

// threshold is how long after its last good probe a node stays live.
func (p *Prober) threshold(idx meta.NodeIdx) time.Duration {
	if nd, found := p.node.nodeByIdx(idx); found && nd.Kind == meta.KindS3 {
		return S3LiveThreshold
	}
	return LiveThreshold
}

// record settles one probe row; disk is the disk's own health for a disk row (its free
// bytes and filesystem), nil for a device, which stores nothing (docs/14 §9).
func (p *Prober) record(idx meta.NodeIdx, now time.Time, rtt time.Duration, ok bool, h rpc.Health, disk *rpc.DiskHealth) {
	live := p.threshold(idx)
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.probes[idx]
	if pr == nil {
		pr = &Probe{}
		p.probes[idx] = pr
	}
	if ok {
		delete(p.suspect, idx) // a good probe clears the mark (I-170)
		wasDead := !pr.DeadSince.IsZero()
		pr.LastOK, pr.RTT, pr.Applied, pr.Version, pr.Dev, pr.Codec, pr.Web = now, rtt, h.Applied, h.Version, h.Dev, h.Codec, h.Web
		pr.Free, pr.FS = 0, ""
		if disk != nil {
			pr.Free, pr.FS = disk.Free, disk.FS
		}
		pr.DeadSince, pr.Down, pr.Lost = time.Time{}, false, false
		if wasDead {
			p.emit(Event{Kind: MemberUp, Idx: idx, Since: now})
		}
		return
	}
	if pr.DeadSince.IsZero() && now.Sub(pr.LastOK) >= live {
		pr.DeadSince = now
	}
}

// recordDisks settles a host's disks from its health reply: an unreachable host's disks
// age like the host; a disk the host lists OK is live; a disk it lists failing, or does
// not list at all, is dead from this probe on (docs/14 §4, I-90).
func (p *Prober) recordDisks(ds []meta.Node, now time.Time, rtt time.Duration, hostOK bool, h rpc.Health) {
	for i := range ds {
		d := &ds[i]
		if !hostOK {
			p.record(d.Idx, now, rtt, false, rpc.Health{}, nil)
			continue
		}
		var dh *rpc.DiskHealth
		for i := range h.Disks {
			if h.Disks[i].ID == d.ID {
				dh = &h.Disks[i]
			}
		}
		if dh != nil && dh.OK {
			p.record(d.Idx, now, rtt, true, h, dh)
			continue
		}
		p.mu.Lock()
		pr := p.probes[d.Idx]
		if pr == nil {
			pr = &Probe{}
			p.probes[d.Idx] = pr
		}
		pr.LastOK = time.Time{}
		if pr.DeadSince.IsZero() {
			pr.DeadSince = now
		}
		p.mu.Unlock()
	}
}

// recordMirrors settles a host's mirrors from its health reply (docs/16 §1): a mirror the
// reply lists online is online, one it lists offline or leaves out is offline; an
// unreachable host's mirrors are offline and keep what the last reply said of them.
func (p *Prober) recordMirrors(ms []meta.Node, now time.Time, hostOK bool, h rpc.Health) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range ms {
		m := &ms[i]
		mp := p.mirrors[m.Idx]
		if mp == nil {
			mp = &MirrorProbe{Health: rpc.MirrorHealth{ID: m.ID}}
			p.mirrors[m.Idx] = mp
		}
		if !hostOK {
			mp.Online, mp.Health.Online, mp.Health.Syncing = false, false, nil
			mp.Health.Reason = "the host is unreachable"
			continue
		}
		var mh *rpc.MirrorHealth
		for j := range h.Mirrors {
			if h.Mirrors[j].ID == m.ID {
				mh = &h.Mirrors[j]
			}
		}
		if mh == nil {
			mp.Online, mp.Health.Online, mp.Health.Syncing = false, false, nil
			mp.Health.Reason = "not attached"
			continue
		}
		mp.Health, mp.Online = *mh, mh.Online
		if mh.Online {
			mp.LastSeen = now
		}
	}
}

// Mirror is the last word on mirror row idx from its host, false before any.
func (p *Prober) Mirror(idx meta.NodeIdx) (MirrorProbe, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mp, ok := p.mirrors[idx]
	if !ok {
		return MirrorProbe{}, false
	}
	return *mp, true
}

// MirrorOnline is whether mirror row idx is online (docs/16 §1, I-188): its host is live
// and its host's last health reply listed it online.
func (p *Prober) MirrorOnline(idx meta.NodeIdx) bool {
	mp, ok := p.Mirror(idx)
	if !ok || !mp.Online {
		return false
	}
	nd, found := p.node.nodeByIdx(idx)
	return found && p.IsLive(nd.Host)
}

// holdDowns emits MemberDown/MemberLost once a node has been dead long enough.
func (p *Prober) holdDowns(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for idx, pr := range p.probes {
		if pr.DeadSince.IsZero() {
			continue
		}
		if !pr.Down && now.Sub(pr.DeadSince) >= p.fillAfter {
			pr.Down = true
			p.emit(Event{Kind: MemberDown, Idx: idx, Since: pr.DeadSince})
		}
		if !pr.Lost && now.Sub(pr.DeadSince) >= p.evictAfter {
			pr.Lost = true
			p.emit(Event{Kind: MemberLost, Idx: idx, Since: pr.DeadSince})
		}
	}
}

// hostOf is the device that serves idx: itself for a device or a bucket, its host for a
// disk.
func (p *Prober) hostOf(idx meta.NodeIdx) meta.NodeIdx {
	if nd, found := p.node.nodeByIdx(idx); found && nd.Kind == meta.KindDisk {
		return nd.Host
	}
	return idx
}

// Suspect marks idx's host suspect (docs/05 §6, I-170): a shard RPC to it failed at the
// transport level or a PUT to it outlasted the straggler grace. IsLive answers false for
// the host and its disks until the first probe of it that succeeds after the mark. This
// host is never suspect of itself.
func (p *Prober) Suspect(idx meta.NodeIdx) {
	host := p.hostOf(idx)
	if self, ok := p.node.SelfIdx(); ok && self == host {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.suspect == nil {
		p.suspect = map[meta.NodeIdx]time.Time{}
	}
	if _, already := p.suspect[host]; !already {
		p.suspect[host] = p.clk.Now()
	}
}

// IsSuspect reports whether idx's host is marked suspect.
func (p *Prober) IsSuspect(idx meta.NodeIdx) bool {
	host := p.hostOf(idx)
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.suspect[host]
	return ok
}

// Snapshot returns a copy of the probe table.
func (p *Prober) Snapshot() map[meta.NodeIdx]Probe {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[meta.NodeIdx]Probe, len(p.probes))
	for idx, pr := range p.probes {
		out[idx] = *pr
	}
	return out
}

// IsLive reports whether a probe succeeded within LiveThreshold (self is always live).
// A node that joined less than LiveThreshold ago and has not been probed yet counts as
// live: the leader just talked to it, and treating it as dead would place the first
// writes after a join degraded.
func (p *Prober) IsLive(idx meta.NodeIdx) bool {
	if self, ok := p.node.SelfIdx(); ok && self == idx {
		return true
	}
	if p.IsSuspect(idx) {
		return false // until its host's next good probe (docs/05 §6, I-170)
	}
	// this host's own disks: live iff the host's own health lists them OK, without an age
	// test, since nothing probes self over the network (docs/14 §4)
	if nd, found := p.node.nodeByIdx(idx); found && nd.Kind == meta.KindDisk {
		if self, ok := p.node.SelfIdx(); ok && nd.Host == self && p.SelfHealth != nil {
			for _, dh := range p.SelfHealth().Disks {
				if dh.ID == nd.ID {
					return dh.OK
				}
			}
			return false
		}
	}
	p.mu.Lock()
	pr, ok := p.probes[idx]
	var lastOK, deadSince time.Time
	if ok {
		lastOK, deadSince = pr.LastOK, pr.DeadSince // copied under the lock: record rewrites the probe in place
	}
	p.mu.Unlock()
	if ok && !lastOK.IsZero() {
		return p.clk.Now().Sub(lastOK) < p.threshold(idx)
	}
	if !deadSince.IsZero() {
		return false // reported failing (a disk its host lists not OK): no join grace
	}
	// a bucket is live only once this node has reached it: a device without its
	// credentials never sees it live (docs/10 §4.1)
	if nd, found := p.node.nodeByIdx(idx); found && nd.JoinedAt > 0 && nd.Kind != meta.KindS3 {
		return p.clk.Now().Sub(time.Unix(0, nd.JoinedAt)) < LiveThreshold
	}
	return false
}

// SetCodecView makes MinCodec take peer idx as reading codec v whatever it reports (0:
// its report again): a rolling upgrade in which this node sees an old member that another
// node does not. Only /dev/codec calls it (dev.enabled, soak phase 22, docs/09 §6).
func (p *Prober) SetCodecView(idx meta.NodeIdx, v uint8) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v == 0 {
		delete(p.codecView, idx)
		return
	}
	if p.codecView == nil {
		p.codecView = map[meta.NodeIdx]uint8{}
	}
	p.codecView[idx] = v
}

// MinCodec is the oldest codec version a live active device reports, this node's own
// included: what the leader may write without leaving a live member unable to read it
// (docs/12 §6, I-156). A member that predates the field reports 0 and counts as oldest.
func (p *Prober) MinCodec() uint8 {
	minCodec := uint8(meta.CodecVersion)
	self, _ := p.node.SelfIdx()
	p.node.FSM.ForEachNode(func(nd meta.Node) bool {
		if nd.Kind != meta.KindDevice || nd.State != meta.Active || nd.Idx == self || !p.IsLive(nd.Idx) {
			return true
		}
		p.mu.Lock()
		pr, ok := p.probes[nd.Idx]
		codec := uint8(0)
		if ok {
			codec = pr.Codec
		}
		if v, set := p.codecView[nd.Idx]; set && ok {
			codec = v
		}
		p.mu.Unlock()
		if ok && codec < minCodec {
			minCodec = codec
		}
		return true
	})
	return minCodec
}

// DeadFor returns how long a node has been dead (0 when live or unknown).
func (p *Prober) DeadFor(idx meta.NodeIdx) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.probes[idx]
	if !ok || pr.DeadSince.IsZero() {
		return 0
	}
	return p.clk.Now().Sub(pr.DeadSince)
}

// --- meta.Peers ---

func (p *Prober) Self() meta.NodeIdx {
	idx, _ := p.node.SelfIdx()
	return idx
}

func (p *Prober) Live() []meta.NodeIdx {
	var out []meta.NodeIdx
	p.node.FSM.ForEachNode(func(nd meta.Node) bool {
		if nd.State != meta.Removed && p.IsLive(nd.Idx) {
			out = append(out, nd.Idx)
		}
		return true
	})
	slices.Sort(out)
	return out
}

// Rank orders self first, then by RTT ascending, unknown devices after those, S3 nodes last.
func (p *Prober) Rank(nodes []meta.NodeIdx) []meta.NodeIdx {
	self := p.Self()
	snap := p.Snapshot()
	out := append([]meta.NodeIdx(nil), nodes...)
	rtt := func(idx meta.NodeIdx) time.Duration {
		if idx == self {
			return -1
		}
		if nd, found := p.node.nodeByIdx(idx); found {
			switch {
			case nd.Kind == meta.KindDisk && nd.Host == self:
				return -1 // this host's own disk: no network between us (docs/14 §9)
			case nd.Kind == meta.KindS3:
				return 2 * time.Hour // after every device, even unprobed ones (docs/10 §4.3)
			}
		}
		if pr, ok := snap[idx]; ok && !pr.LastOK.IsZero() {
			return pr.RTT
		}
		return time.Hour
	}
	sort.SliceStable(out, func(i, j int) bool { return rtt(out[i]) < rtt(out[j]) })
	return out
}

// Health reports the Raft reachability state (docs/02 §Health).
func (n *Node) Health(clk clock.Clock, prober *Prober) HealthReport {
	rep := HealthReport{}
	_, leaderID := n.Raft.LeaderWithID()
	rep.Leader = string(leaderID)
	if cfg := n.Raft.GetConfiguration(); cfg.Error() == nil {
		for _, s := range cfg.Configuration().Servers {
			if s.Suffrage == raft.Voter {
				rep.Voters++
				if prober != nil {
					if nd, ok := n.nodeByID(string(s.ID)); ok && prober.IsLive(nd.Idx) {
						rep.LiveVoters++
					}
				} else if string(s.ID) == n.ID {
					rep.LiveVoters++
				}
			}
		}
	}
	switch {
	case n.IsLeader():
		rep.State, rep.LastContact = Healthy, 0
	case leaderID == "":
		rep.State = NoLeader
	default:
		rep.LastContact = clk.Now().Sub(n.Raft.LastContact())
		if rep.LastContact >= StaleThreshold {
			rep.State = Stale
		} else {
			rep.State = Healthy
		}
	}
	return rep
}

func (n *Node) nodeByID(id string) (meta.Node, bool) { return n.FSM.NodeByID(id) }
