package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
)

// Membership errors.
var (
	ErrHoldsShards = errors.New("cluster: node still holds shard slots; drain it first or use --force")
	ErrBadMemberOp = errors.New("cluster: bad member operation")
)

var _ rpc.Joiner = (*Node)(nil)
var _ rpc.Applier = (*Node)(nil)

// AppliedIndex implements rpc.Applier.
func (n *Node) AppliedIndex() uint64 { return n.Raft.AppliedIndex() }

// LeaderRPCAddr returns the leader's RPC address from the node table, or "".
func (n *Node) LeaderRPCAddr() string {
	_, id := n.Raft.LeaderWithID()
	if id == "" {
		return ""
	}
	nodes := n.FSM.Nodes()
	for i := range nodes {
		if nodes[i].ID == string(id) && nodes[i].Addr != "" {
			return net.JoinHostPort(nodes[i].Addr, strconv.Itoa(int(nodes[i].RPCPort)))
		}
	}
	return ""
}

// RPCAddrOf resolves a node index to its RPC address (rpc.Resolver).
func (n *Node) RPCAddrOf(idx meta.NodeIdx) (addr string, self, ok bool) {
	nodes := n.FSM.Nodes()
	for i := range nodes {
		if nodes[i].Idx == idx {
			if nodes[i].Kind != meta.KindDevice || nodes[i].Addr == "" {
				return "", false, false
			}
			return net.JoinHostPort(nodes[i].Addr, strconv.Itoa(int(nodes[i].RPCPort))), nodes[i].ID == n.ID, true
		}
	}
	return "", false, false
}

func (n *Node) nodeByIdx(idx meta.NodeIdx) (meta.Node, bool) {
	nodes := n.FSM.Nodes()
	for i := range nodes {
		if nodes[i].Idx == idx {
			return nodes[i], true
		}
	}
	return meta.Node{}, false
}

func raftAddrOf(nd meta.Node) raft.ServerAddress {
	return raft.ServerAddress(net.JoinHostPort(nd.Addr, strconv.Itoa(int(nd.RaftPort))))
}

// RaftAddrFor lets tests substitute in-memory transport addresses for node records.
var RaftAddrFor = raftAddrOf

// Join implements the leader side of `tailraid join` (docs/02 §Bootstrap and join).
func (n *Node) Join(ctx context.Context, req rpc.JoinRequest) (rpc.JoinResponse, error) {
	if !n.IsLeader() {
		if n.LeaderRPCAddr() == "" {
			return rpc.JoinResponse{}, ErrNoLeader
		}
		return rpc.JoinResponse{}, ErrNotLeader
	}
	if len(req.NodeID) != 16 || req.Addr == "" || req.RaftPort == 0 || req.RPCPort == 0 {
		return rpc.JoinResponse{}, fmt.Errorf("%w: node_id, addr and ports are required", ErrBadMemberOp)
	}
	nd := meta.Node{ID: req.NodeID, Name: req.Name, Kind: meta.KindDevice, TSStableID: req.TSStableID, Addr: req.Addr,
		RaftPort: req.RaftPort, RPCPort: req.RPCPort, Voter: req.Voter, Capacity: req.Capacity, Version: req.Version, JoinedAt: time.Now().UnixNano()}
	// re-join of a known node keeps its index
	nodes := n.FSM.Nodes()
	for i := range nodes {
		if nodes[i].ID == req.NodeID {
			if nodes[i].State == meta.Removed {
				return rpc.JoinResponse{}, fmt.Errorf("%w: node %s was removed; reset it before joining again", ErrBadMemberOp, req.NodeID)
			}
			nd.Idx = nodes[i].Idx
		}
	}
	var fut raft.IndexFuture
	if req.Voter {
		fut = n.Raft.AddVoter(raft.ServerID(req.NodeID), RaftAddrFor(nd), 0, 10*time.Second)
	} else {
		fut = n.Raft.AddNonvoter(raft.ServerID(req.NodeID), RaftAddrFor(nd), 0, 10*time.Second)
	}
	if err := fut.Error(); err != nil {
		return rpc.JoinResponse{}, raftErr("add raft server", err)
	}
	res, index, err := n.Propose(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return rpc.JoinResponse{}, fmt.Errorf("cluster: record member: %w", err)
	}
	if err := n.FSM.WaitApplied(ctx, index); err != nil {
		return rpc.JoinResponse{}, err
	}
	m := n.FSM.Meta()
	return rpc.JoinResponse{ClusterID: fmt.Sprintf("%x", m.ClusterID[:]), NodeIdx: meta.NodeIdx(res.Ino), Volume: n.FSM.Volume(), Members: n.FSM.Nodes()}, nil
}

// SlotsHeldBy counts committed shard slots placed on a node.
func (n *Node) SlotsHeldBy(idx meta.NodeIdx) int {
	count := 0
	n.FSM.ForEachChunk(func(c meta.Chunk) bool {
		for _, s := range c.Placement {
			if s == idx {
				count++
			}
		}
		return true
	})
	return count
}

// MemberOp implements remove, update_addr, set_voter, set_capacity and set_state on the leader.
func (n *Node) MemberOp(ctx context.Context, op rpc.MemberOp) error {
	if !n.IsLeader() {
		if n.LeaderRPCAddr() == "" {
			return ErrNoLeader
		}
		return ErrNotLeader
	}
	nd, ok := n.nodeByIdx(op.NodeIdx)
	if !ok {
		return meta.ErrNoSuchNode
	}
	n.log.Info("member op", "op", op.Op, "idx", op.NodeIdx, "node", nd.ID, "state", nd.State, "voter", nd.Voter)
	propose := func(cmd meta.Command) error {
		res, index, err := n.Propose(ctx, cmd)
		if err == nil {
			err = res.Err
		}
		if err != nil {
			return err
		}
		return n.FSM.WaitApplied(ctx, index)
	}
	switch op.Op {
	case "remove":
		if nd.State != meta.Removed && !op.Force && n.SlotsHeldBy(op.NodeIdx) > 0 {
			return ErrHoldsShards
		}
		if nd.State != meta.Removed {
			if err := propose(meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: op.NodeIdx, State: meta.Removed, Now: time.Now().UnixNano()}}); err != nil {
				return err
			}
		}
		if nd.Kind == meta.KindDevice && nd.ID != n.ID {
			if err := n.Raft.RemoveServer(raft.ServerID(nd.ID), 0, 10*time.Second).Error(); err != nil {
				return raftErr("remove raft server", err)
			}
		}
		return nil
	case "set_state":
		if op.State != meta.Active && op.State != meta.Draining {
			return fmt.Errorf("%w: set_state accepts active or draining", ErrBadMemberOp)
		}
		return propose(meta.Command{Op: meta.OpNodeSetState, NodeSetState: &meta.NodeSetState{Idx: op.NodeIdx, State: op.State, Now: time.Now().UnixNano()}})
	case "set_capacity":
		nd.Capacity = op.Capacity
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	case "set_voter":
		if nd.Kind != meta.KindDevice {
			return fmt.Errorf("%w: only devices vote", ErrBadMemberOp)
		}
		var fut raft.IndexFuture
		if op.Voter {
			fut = n.Raft.AddVoter(raft.ServerID(nd.ID), RaftAddrFor(nd), 0, 10*time.Second)
		} else {
			fut = n.Raft.AddNonvoter(raft.ServerID(nd.ID), RaftAddrFor(nd), 0, 10*time.Second)
		}
		if err := fut.Error(); err != nil {
			return raftErr("change suffrage", err)
		}
		nd.Voter = op.Voter
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	case "update_addr":
		if op.Addr == "" {
			return fmt.Errorf("%w: update_addr needs addr", ErrBadMemberOp)
		}
		nd.Addr = op.Addr
		var fut raft.IndexFuture
		if nd.Voter {
			fut = n.Raft.AddVoter(raft.ServerID(nd.ID), RaftAddrFor(nd), 0, 10*time.Second)
		} else {
			fut = n.Raft.AddNonvoter(raft.ServerID(nd.ID), RaftAddrFor(nd), 0, 10*time.Second)
		}
		if err := fut.Error(); err != nil {
			return raftErr("update raft address", err)
		}
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	}
	return fmt.Errorf("%w: unknown op %q", ErrBadMemberOp, op.Op)
}

// raftErr maps Raft's leadership errors onto the not-leader sentinel so RPC callers get
// a redirect and retry, and wraps anything else.
func raftErr(what string, err error) error {
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) || errors.Is(err, raft.ErrLeadershipTransferInProgress) {
		return fmt.Errorf("%w: %s: %v", ErrNotLeader, what, err) //nolint:errorlint // raft detail
	}
	return fmt.Errorf("cluster: %s: %w", what, err)
}
