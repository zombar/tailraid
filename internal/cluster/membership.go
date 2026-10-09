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

// Membership errors. A remove refused because the node still holds slots is
// meta.ErrNodeNotEmpty, enforced by the FSM itself (docs/02).
var ErrBadMemberOp = errors.New("cluster: bad member operation")

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
	if nd, ok := n.FSM.NodeByID(string(id)); ok && nd.Addr != "" {
		return nd.RPCAddr()
	}
	return ""
}

// RPCAddrOf resolves a target index to its RPC address (rpc.Resolver): a disk resolves
// to its host's (docs/14 §4).
func (n *Node) RPCAddrOf(idx meta.NodeIdx) (addr string, self, ok bool) {
	nd, found := n.FSM.Node(idx)
	if found && nd.Kind == meta.KindDisk {
		nd, found = n.FSM.Node(nd.Host)
	}
	if !found || nd.Kind != meta.KindDevice || nd.Addr == "" {
		return "", false, false
	}
	return nd.RPCAddr(), nd.ID == n.ID, true
}

// DiskOf names the disk ID of an added-disk target (false for every other kind).
func (n *Node) DiskOf(idx meta.NodeIdx) (string, bool) {
	nd, found := n.FSM.Node(idx)
	if !found || nd.Kind != meta.KindDisk {
		return "", false
	}
	return nd.ID, true
}

func (n *Node) nodeByIdx(idx meta.NodeIdx) (meta.Node, bool) { return n.FSM.Node(idx) }

func raftAddrOf(nd meta.Node) raft.ServerAddress { return raft.ServerAddress(nd.RaftAddr()) }

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
	var err error
	// re-join of a known node keeps its index; a removed record refuses the ID
	n.FSM.ForEachNode(func(cur meta.Node) bool {
		if cur.ID == req.NodeID {
			if cur.State == meta.Removed {
				nd.Idx = 0
				err = fmt.Errorf("%w: node %s was removed; reset it before joining again", ErrBadMemberOp, req.NodeID)
				return false
			}
			nd.Idx = cur.Idx
		}
		return true
	})
	if err != nil {
		return rpc.JoinResponse{}, err
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
	return rpc.JoinResponse{ClusterID: meta.HexID(m.ClusterID), NodeIdx: meta.NodeIdx(res.Ino), Volume: n.FSM.Volume(), Members: n.FSM.Nodes()}, nil
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
		if nd.State != meta.Removed {
			if op.Force {
				// make the degradation explicit: zero every slot on the node, then the
				// repairer fills them from the other copies (docs/02, L-96)
				if err := n.zeroSlots(ctx, op.NodeIdx, propose); err != nil {
					return err
				}
			} else if held := n.SlotsHeldBy(op.NodeIdx); held > 0 {
				return fmt.Errorf("%w: %d slots still on node %d; drain it first or use --force", meta.ErrNodeNotEmpty, held, op.NodeIdx)
			}
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
	case "add_disk":
		// node add-disk on the host: a disk row under it, keyed by the marker's disk ID
		// so a retry updates the row rather than adding a second (docs/14 §2)
		if op.DiskID == "" {
			return fmt.Errorf("%w: add_disk needs disk_id", ErrBadMemberOp)
		}
		disk := meta.Node{ID: op.DiskID, Name: op.Name, Kind: meta.KindDisk, Host: op.NodeIdx, Capacity: op.Capacity, JoinedAt: time.Now().UnixNano()}
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: disk}})
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

// zeroSlots proposes SetPlacement with slot 0 for every chunk that places a shard on idx.
func (n *Node) zeroSlots(ctx context.Context, idx meta.NodeIdx, propose func(meta.Command) error) error {
	var victims []meta.Chunk
	n.FSM.ForEachChunk(func(c meta.Chunk) bool {
		for _, s := range c.Placement {
			if s == idx {
				victims = append(victims, c)
				break
			}
		}
		return true
	})
	for _, c := range victims {
		place := append([]meta.NodeIdx(nil), c.Placement...)
		for i, s := range place {
			if s == idx {
				place[i] = 0
			}
		}
		if err := propose(meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, K: c.K, M: c.M, Placement: place}}); err != nil {
			return fmt.Errorf("zero slots of %x: %w", c.Hash[:8], err)
		}
	}
	return nil
}

// raftErr maps Raft's leadership errors onto the not-leader sentinel so RPC callers get
// a redirect and retry, and wraps anything else.
func raftErr(what string, err error) error {
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) || errors.Is(err, raft.ErrLeadershipTransferInProgress) {
		return fmt.Errorf("%w: %s: %v", ErrNotLeader, what, err) //nolint:errorlint // raft detail
	}
	return fmt.Errorf("cluster: %s: %w", what, err)
}
