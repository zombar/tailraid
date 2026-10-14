package meta

import "context"

// Proposer submits a command to the replicated log (docs/02). A transport-level failure
// (no leader, leadership lost, timeout) is returned as err and the outcome is unknown;
// an FSM rejection is returned in Result.Err with err == nil. index is the log index the
// command was applied at, for WaitApplied.
type Proposer interface {
	Propose(ctx context.Context, cmd Command) (res Result, index uint64, err error)
}

// PathClass is the class of this gateway's path to a device (docs/20 §5), best first.
// The storage code ranks and schedules by it and never places by it (I-327).
type PathClass int

// The classes: a LAN endpoint, any other direct endpoint, a relay through a member, no
// path at all.
const (
	PathLAN PathClass = iota + 1
	PathWAN
	PathRelay
	PathNone
)

func (c PathClass) String() string {
	switch c {
	case PathLAN:
		return "lan"
	case PathWAN:
		return "wan"
	case PathRelay:
		return "relay"
	default:
		return "none"
	}
}

// Peers is a gateway's local view of node liveness, path and latency (docs/03 §Rank,
// docs/20 §8). It is never replicated.
type Peers interface {
	Self() NodeIdx
	Live() []NodeIdx
	// Rank orders nodes for reads: self and this host's own disks first, then by
	// (path class, RTT), S3 nodes last.
	Rank(nodes []NodeIdx) []NodeIdx
	// Class is the path class to the node's host: PathLAN for this host and its own
	// disks, and for every node of a gateway with no mesh (the in-process harness).
	Class(idx NodeIdx) PathClass
}
