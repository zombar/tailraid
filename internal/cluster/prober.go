package cluster

import (
	"context"
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
	DeadSince time.Time // zero while live
	Down      bool      // MemberDown already emitted
	Lost      bool      // MemberLost already emitted
}

// HealthClient probes a node (rpc.Client).
type HealthClient interface {
	Health(ctx context.Context, addr string) (rpc.Health, error)
}

// Prober keeps an all-to-all liveness view and implements meta.Peers (docs/05 §4).
type Prober struct {
	node    *Node
	client  HealthClient
	clk     clock.Clock
	timeout time.Duration
	// SelfHealth supplies this node's own health row (free bytes, version); optional.
	SelfHealth func() rpc.Health

	mu     sync.Mutex
	probes map[meta.NodeIdx]*Probe
	events chan Event
}

var _ meta.Peers = (*Prober)(nil)

// NewProber wires a prober for node. client may be nil in single-node setups.
func NewProber(node *Node, client HealthClient, clk clock.Clock) *Prober {
	return &Prober{node: node, client: client, clk: clk, timeout: 3 * time.Second, probes: map[meta.NodeIdx]*Probe{}, events: make(chan Event, 64)}
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

// Tick probes every device member once (exported for tests driving the fake clock).
func (p *Prober) Tick(ctx context.Context) {
	nodes := p.node.FSM.Nodes()
	now := p.clk.Now()
	var wg sync.WaitGroup
	for i := range nodes {
		nd := nodes[i]
		if nd.Kind != meta.KindDevice || nd.State == meta.Removed {
			continue
		}
		if nd.ID == p.node.ID {
			h := rpc.Health{Applied: p.node.AppliedIndex()}
			if p.SelfHealth != nil {
				h = p.SelfHealth()
			}
			p.record(nd.Idx, now, 0, true, h)
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
		go func(idx meta.NodeIdx, addr string) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, p.timeout)
			defer cancel()
			start := p.clk.Now()
			h, err := p.client.Health(pctx, addr)
			p.record(idx, p.clk.Now(), p.clk.Now().Sub(start), err == nil, h)
		}(nd.Idx, addr)
	}
	wg.Wait()
	p.holdDowns(now)
}

func (p *Prober) record(idx meta.NodeIdx, now time.Time, rtt time.Duration, ok bool, h rpc.Health) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.probes[idx]
	if pr == nil {
		pr = &Probe{}
		p.probes[idx] = pr
	}
	if ok {
		wasDead := !pr.DeadSince.IsZero()
		pr.LastOK, pr.RTT, pr.Free, pr.Applied, pr.Version = now, rtt, h.Free, h.Applied, h.Version
		pr.DeadSince, pr.Down, pr.Lost = time.Time{}, false, false
		if wasDead {
			p.emit(Event{Kind: MemberUp, Idx: idx, Since: now})
		}
		return
	}
	if pr.DeadSince.IsZero() && now.Sub(pr.LastOK) >= LiveThreshold {
		pr.DeadSince = now
	}
}

// holdDowns emits MemberDown/MemberLost once a node has been dead long enough.
func (p *Prober) holdDowns(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for idx, pr := range p.probes {
		if pr.DeadSince.IsZero() {
			continue
		}
		if !pr.Down && now.Sub(pr.DeadSince) >= FillAfter {
			pr.Down = true
			p.emit(Event{Kind: MemberDown, Idx: idx, Since: pr.DeadSince})
		}
		if !pr.Lost && now.Sub(pr.DeadSince) >= EvictAfter {
			pr.Lost = true
			p.emit(Event{Kind: MemberLost, Idx: idx, Since: pr.DeadSince})
		}
	}
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
	p.mu.Lock()
	pr, ok := p.probes[idx]
	p.mu.Unlock()
	if ok && !pr.LastOK.IsZero() {
		return p.clk.Now().Sub(pr.LastOK) < LiveThreshold
	}
	if nd, found := p.node.nodeByIdx(idx); found && nd.JoinedAt > 0 {
		return p.clk.Now().Sub(time.Unix(0, nd.JoinedAt)) < LiveThreshold
	}
	return false
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
	nodes := p.node.FSM.Nodes()
	var out []meta.NodeIdx
	for i := range nodes {
		if nodes[i].State == meta.Removed {
			continue
		}
		if p.IsLive(nodes[i].Idx) {
			out = append(out, nodes[i].Idx)
		}
	}
	return out
}

// Rank orders self first, then by RTT ascending, unknown last.
func (p *Prober) Rank(nodes []meta.NodeIdx) []meta.NodeIdx {
	self := p.Self()
	snap := p.Snapshot()
	out := append([]meta.NodeIdx(nil), nodes...)
	rtt := func(idx meta.NodeIdx) time.Duration {
		if idx == self {
			return -1
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

func (n *Node) nodeByID(id string) (meta.Node, bool) {
	nodes := n.FSM.Nodes()
	for i := range nodes {
		if nodes[i].ID == id {
			return nodes[i], true
		}
	}
	return meta.Node{}, false
}
