package meta

import "context"

// Proposer submits a command to the replicated log (docs/02). A transport-level failure
// (no leader, leadership lost, timeout) is returned as err and the outcome is unknown;
// an FSM rejection is returned in Result.Err with err == nil. index is the log index the
// command was applied at, for WaitApplied.
type Proposer interface {
	Propose(ctx context.Context, cmd Command) (res Result, index uint64, err error)
}

// Peers is a gateway's local view of node liveness and latency (docs/03 §Rank). It is
// never replicated.
type Peers interface {
	Self() NodeIdx
	Live() []NodeIdx
	// Rank orders nodes for reads: self first, then by RTT, S3 nodes last.
	Rank(nodes []NodeIdx) []NodeIdx
}
