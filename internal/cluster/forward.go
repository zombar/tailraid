package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// Forwarder sends a command to another node's /v1/apply, and a join to its /v1/join
// (rpc.Client).
type Forwarder interface {
	ApplyAt(ctx context.Context, addr string, cmd meta.Command) (meta.Result, uint64, error)
	Join(ctx context.Context, addr string, req rpc.JoinRequest) (rpc.JoinResponse, error)
}

// SetForwarder enables leader forwarding for Propose on followers.
func (n *Node) SetForwarder(f Forwarder) {
	n.fwdMu.Lock()
	n.fwd = f
	n.fwdMu.Unlock()
}

// Propose implements meta.Proposer. On the leader it applies through Raft; on a follower
// it forwards to the leader's RPC. Semantics (docs/02 §forwarding): a 421 redirect is
// retried once at the named leader; any other failure is returned with the outcome
// unknown, and callers retry only idempotent commands.
func (n *Node) Propose(ctx context.Context, cmd meta.Command) (meta.Result, uint64, error) {
	if n.IsLeader() {
		return n.applyLocal(ctx, cmd)
	}
	n.fwdMu.Lock()
	fwd := n.fwd
	n.fwdMu.Unlock()
	addr := n.LeaderRPCAddr()
	if fwd == nil || addr == "" {
		if _, id := n.Raft.LeaderWithID(); id == "" {
			return meta.Result{}, 0, ErrNoLeader
		}
		return meta.Result{}, 0, ErrNotLeader
	}
	res, index, err := fwd.ApplyAt(ctx, addr, cmd)
	if errors.Is(err, rpc.ErrNotLeader) {
		if redirect := rpc.LeaderFromErr(err); redirect != "" && redirect != addr {
			res, index, err = fwd.ApplyAt(ctx, redirect, cmd)
		}
	}
	if err == nil {
		return res, index, nil
	}
	return meta.Result{}, 0, forwardErr(err)
}

// forwardErr classifies a forward that returned no result (I-95). A request that never
// left (the dial was refused, or a fault rule dropped it) was not applied, so it is
// retried like a missing leader: the leader is out of reach, which an election or a
// heal mends within seconds. A timeout, an error after the request was sent, or the
// leader's 500 may have applied the command; callers resolve that, never retry it blindly.
func forwardErr(err error) error {
	var opErr *net.OpError
	switch {
	case errors.Is(err, rpc.ErrNoLeader), errors.Is(err, rpc.ErrNotLeader):
		return fmt.Errorf("%w: %v", ErrNoLeader, err) //nolint:errorlint // transport detail
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return fmt.Errorf("%w: leader unreachable: %v", ErrNoLeader, err) //nolint:errorlint // transport detail
	default:
		return fmt.Errorf("%w: forward: %w", meta.ErrUnknownOutcome, err)
	}
}

// applyLocal hands cmd to Raft on the leader. An expired context is refused before
// dispatch (raft disarms its timer for a zero timeout); a cancellation while the future
// is pending is reported as an unknown outcome, because the entry may still commit.
func (n *Node) applyLocal(ctx context.Context, cmd meta.Command) (meta.Result, uint64, error) {
	if err := ctx.Err(); err != nil {
		return meta.Result{}, 0, err
	}
	if n.closed.Load() { // nothing is enqueued behind Close's barrier: not dispatched, so safe to retry elsewhere
		return meta.Result{}, 0, fmt.Errorf("%w: node closing", ErrNotLeader)
	}
	timeout := 5 * time.Second
	if d, ok := ctx.Deadline(); ok {
		timeout = max(time.Until(d), time.Millisecond)
	}
	if n.MinCodec != nil {
		if need := meta.CommandCodec(cmd); need > 1 && n.MinCodec() < need {
			return meta.Result{Err: meta.ErrCodec}, 0, nil // refused before Raft sees it: nothing to retry (I-156)
		}
	}
	encoded, err := meta.EncodeCommand(cmd)
	if err != nil {
		return meta.Result{}, 0, err
	}
	fut := n.Raft.Apply(encoded, timeout)
	done := make(chan error, 1)
	go func() { done <- fut.Error() }() // raft resolves every future: on a stepdown, a timeout, or the barrier Close runs before shutdown
	select {
	case err = <-done:
	case <-ctx.Done():
		return meta.Result{}, 0, fmt.Errorf("%w: %w", meta.ErrUnknownOutcome, ctx.Err())
	}
	if err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return meta.Result{}, 0, fmt.Errorf("%w: %v", ErrNotLeader, err) //nolint:errorlint // raft detail
		}
		// ErrLeadershipLost, ErrEnqueueTimeout, a shutdown: dispatched or racing with a
		// dispatch, so the outcome is unknown (I-95)
		return meta.Result{}, 0, fmt.Errorf("%w: apply: %w", meta.ErrUnknownOutcome, err)
	}
	res, ok := fut.Response().(meta.Result)
	if !ok {
		return meta.Result{}, 0, ErrUnknownReply
	}
	return res, fut.Index(), nil
}
