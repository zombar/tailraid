package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// Forwarder sends a command to another node's /v1/apply (rpc.Client).
type Forwarder interface {
	ApplyAt(ctx context.Context, addr string, cmd meta.Command) (meta.Result, uint64, error)
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
	switch {
	case err == nil:
		return res, index, nil
	case errors.Is(err, rpc.ErrNoLeader), errors.Is(err, rpc.ErrNotLeader):
		return meta.Result{}, 0, fmt.Errorf("%w: %v", ErrNoLeader, err) //nolint:errorlint // transport detail
	default:
		return meta.Result{}, 0, fmt.Errorf("cluster: forward: %w", err)
	}
}

func (n *Node) applyLocal(ctx context.Context, cmd meta.Command) (meta.Result, uint64, error) {
	timeout := 5 * time.Second
	if d, ok := ctx.Deadline(); ok {
		timeout = time.Until(d)
	}
	fut := n.Raft.Apply(meta.EncodeCommand(cmd), timeout)
	if err := fut.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return meta.Result{}, 0, fmt.Errorf("%w: %v", ErrNotLeader, err) //nolint:errorlint // raft detail
		}
		return meta.Result{}, 0, fmt.Errorf("cluster: apply: %w", err)
	}
	res, ok := fut.Response().(meta.Result)
	if !ok {
		return meta.Result{}, 0, ErrUnknownReply
	}
	return res, fut.Index(), nil
}
