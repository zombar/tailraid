package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/s3"
)

// Membership errors. A remove refused because the node still holds slots is
// meta.ErrNodeNotEmpty, enforced by the FSM itself (docs/02).
var ErrBadMemberOp = errors.New("cluster: bad member operation")

var (
	_ rpc.Joiner  = (*Node)(nil)
	_ rpc.Applier = (*Node)(nil)
)

// AppliedIndex implements rpc.Applier.
// AppliedIndex is the FSM's applied index: what this node has actually applied, which is
// what the DELETE fence (docs/06) and the health row promise. Raft's own AppliedIndex
// only proves dispatch to the FSM goroutine.
func (n *Node) AppliedIndex() uint64 { return n.FSM.Applied() }

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
	if found && (nd.Kind == meta.KindDisk || nd.Kind == meta.KindMirror) { // served by its host
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

// replaceStaleServer removes a Raft server that holds the joiner's address under
// another node ID (a device that was reset), after checking that the server's member row
// names the joiner's device (its mesh key, docs/20 §4; its tailnet device before); a
// different device at a known address is refused (I-94). It returns that row's index so
// the join re-keys the row to the new ID instead of retiring it: the device's disks and
// their shards stay registered under it. Nothing to replace returns 0.
func (n *Node) replaceStaleServer(req rpc.JoinRequest, key meta.MeshKey, addr raft.ServerAddress) (meta.NodeIdx, error) {
	cfg := n.Raft.GetConfiguration()
	if err := cfg.Error(); err != nil {
		return 0, raftErr("read configuration", err)
	}
	var idx meta.NodeIdx
	for _, s := range cfg.Configuration().Servers {
		if s.Address != addr || string(s.ID) == req.NodeID {
			continue
		}
		if old, known := n.FSM.NodeByID(string(s.ID)); known {
			switch {
			case !key.IsZero() && !old.Key.IsZero() && old.Key != key:
				return 0, fmt.Errorf("%w: address %s belongs to node %s, another device", ErrBadMemberOp, req.Addr, old.ID)
			case key.IsZero() && old.TSStableID != "" && old.TSStableID != req.TSStableID:
				return 0, fmt.Errorf("%w: address %s belongs to node %s of tailnet node %s, not %s", ErrBadMemberOp, req.Addr, old.ID, old.TSStableID, req.TSStableID)
			}
			idx = old.Idx
		}
		if err := n.Raft.RemoveServer(s.ID, 0, 10*time.Second).Error(); err != nil {
			return 0, raftErr("remove stale raft server", err)
		}
		n.log.Info("replaced the raft server at the joiner's address", "old", s.ID, "new", req.NodeID, "addr", addr)
	}
	return idx, nil
}

// Join implements the leader side of `tailraid join` (docs/02 §Bootstrap and join).
func (n *Node) Join(ctx context.Context, req rpc.JoinRequest) (rpc.JoinResponse, error) {
	n.memberMu.Lock()
	defer n.memberMu.Unlock()
	if !n.IsLeader() {
		if n.LeaderRPCAddr() == "" {
			return rpc.JoinResponse{}, ErrNoLeader
		}
		return rpc.JoinResponse{}, ErrNotLeader
	}
	if len(req.NodeID) != 16 || req.RaftPort == 0 || req.RPCPort == 0 || (req.Addr == "" && req.Key == "") {
		return rpc.JoinResponse{}, fmt.Errorf("%w: node_id, addr and ports are required", ErrBadMemberOp)
	}
	nd := meta.Node{
		ID: req.NodeID, Name: req.Name, Kind: meta.KindDevice, TSStableID: req.TSStableID, Addr: req.Addr,
		RaftPort: req.RaftPort, RPCPort: req.RPCPort, Voter: req.Voter, Version: req.Version, JoinedAt: time.Now().UnixNano(), // no budget: a device stores nothing (docs/14 §9)
	}
	var err error
	// on the mesh (docs/20 §4) the address is the one the introduce gave the key: a
	// pending row, or the active row the key already holds (a re-join after reset)
	var key meta.MeshKey
	if req.Key != "" {
		if key, err = ParseMeshKey(req.Key); err != nil {
			return rpc.JoinResponse{}, err
		}
		nd.Key, nd.MeshPort = key, req.MeshPort
		if req.SeenFrom != "" {
			nd.Endpoints = []string{req.SeenFrom}
		}
		switch {
		case func() bool { row, ok := n.FSM.NodeByKey(key); nd.Addr = row.Addr; return ok }():
		case func() bool { p, ok := n.FSM.PendingByKey(key); nd.Addr = p.Addr; return ok }():
		default:
			return rpc.JoinResponse{}, ErrNoIntroduce
		}
		if req.Addr != "" && req.Addr != nd.Addr {
			return rpc.JoinResponse{}, fmt.Errorf("%w: the key's address is %s, not %s", ErrBadMemberOp, nd.Addr, req.Addr)
		}
	}
	// re-join of a known node keeps its index; a removed record refuses the ID, and so
	// does a different tailnet identity (I-94): node IDs are public (/health), the
	// StableID is what WhoIs vouched for
	n.FSM.ForEachNode(func(cur meta.Node) bool {
		if cur.ID == req.NodeID {
			if cur.State == meta.Removed {
				nd.Idx = 0
				err = fmt.Errorf("%w: node %s was removed; reset it before joining again", ErrBadMemberOp, req.NodeID)
				return false
			}
			switch {
			case !key.IsZero() && !cur.Key.IsZero() && cur.Key != key:
				nd.Idx = 0
				err = fmt.Errorf("%w: node %s belongs to another device", ErrBadMemberOp, req.NodeID)
				return false
			case key.IsZero() && cur.TSStableID != "" && cur.TSStableID != req.TSStableID:
				nd.Idx = 0
				err = fmt.Errorf("%w: node %s belongs to tailnet node %s, not %s", ErrBadMemberOp, req.NodeID, cur.TSStableID, req.TSStableID)
				return false
			}
			nd.Idx = cur.Idx
		}
		return true
	})
	if err != nil {
		return rpc.JoinResponse{}, err
	}
	// a device that was reset joins again from its old Raft address under a new node ID:
	// the old server would make the configuration refuse the address, so it is removed
	// and the old row re-keyed to the new ID (its disks stay its disks), provided the
	// caller is the same tailnet device the row named (I-94)
	if idx, err := n.replaceStaleServer(req, key, n.raftAddrFor(nd)); err != nil {
		return rpc.JoinResponse{}, err
	} else if idx != 0 && nd.Idx == 0 {
		nd.Idx = idx
	}
	// the Raft server first, the row second: the row's entry is then the newest, and
	// the joiner's first snapshot, which Raft refuses until the FSM has passed the
	// configuration entry, follows the row's apply (node.snapshotAfterJoin); on the
	// mesh the joiner holds the bootstrap as a peer before this call, so the leader's
	// first dial is accepted when the bootstrap leads and retried otherwise
	var fut raft.IndexFuture
	if req.Voter {
		fut = n.Raft.AddVoter(raft.ServerID(req.NodeID), n.raftAddrFor(nd), 0, 10*time.Second)
	} else {
		fut = n.Raft.AddNonvoter(raft.ServerID(req.NodeID), n.raftAddrFor(nd), 0, 10*time.Second)
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

// MemberOp implements remove, update_addr, update_endpoints, set_version, set_voter,
// set_capacity, set_state, add_disk and add_s3 on the leader.
func (n *Node) MemberOp(ctx context.Context, op rpc.MemberOp) error {
	n.memberMu.Lock()
	defer n.memberMu.Unlock()
	if !n.IsLeader() {
		if n.LeaderRPCAddr() == "" {
			return ErrNoLeader
		}
		return ErrNotLeader
	}
	// the row is read as of every entry this leader has committed (I-177): a leader
	// elected before its FSM replayed its log would otherwise rewrite the row from the
	// snapshot's copy, since an upsert replaces the whole row
	if err := n.Raft.Barrier(10 * time.Second).Error(); err != nil {
		return raftErr("apply the log before the member op", err)
	}
	if op.Op == "add_s3" {
		return n.addS3(ctx, op)
	}
	nd, ok := n.nodeByIdx(op.NodeIdx)
	if !ok {
		return meta.ErrNoSuchNode
	}
	n.log.Info("member op", "op", op.Op, "idx", op.NodeIdx, "node", nd.ID, "state", nd.State, "voter", nd.Voter)
	if nd.State == meta.Removed && op.Op != "remove" {
		// the row keeps a dead ID and address for the record: promote or update_addr
		// would put that server back into the Raft configuration (I-136)
		return fmt.Errorf("%w: node %d (%s) was removed", ErrBadMemberOp, op.NodeIdx, nd.Name)
	}
	propose := func(cmd meta.Command) error { return n.proposeApplied(ctx, cmd) }
	switch op.Op {
	case "remove":
		if nd.State != meta.Removed {
			if op.Force {
				// make the degradation explicit: zero every slot on the node, then the
				// repairer fills them from the other copies (docs/02, L-96)
				if err := n.zeroSlots(op.NodeIdx, propose); err != nil {
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
		if nd.Kind == meta.KindDevice {
			return fmt.Errorf("%w: a device has no budget; set_capacity names a disk (docs/14 §9)", ErrBadMemberOp)
		}
		nd.Capacity = op.Capacity
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	case "add_disk":
		// node add-disk on the host: a disk row under it, keyed by the marker's disk ID
		// so a retry updates the row rather than adding a second (docs/14 §2)
		if op.DiskID == "" {
			return fmt.Errorf("%w: add_disk needs disk_id", ErrBadMemberOp)
		}
		disk := meta.Node{ID: op.DiskID, Name: n.diskName(op), Kind: meta.KindDisk, Host: op.NodeIdx, Capacity: op.Capacity, JoinedAt: time.Now().UnixNano()}
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: disk}})
	case "add_mirror":
		// mirror add on the host: a mirror row under it, keyed by the marker's mirror ID,
		// so adding a moved disk on another host re-homes its row (docs/16 §1)
		if op.DiskID == "" {
			return fmt.Errorf("%w: add_mirror needs disk_id (the mirror's ID)", ErrBadMemberOp)
		}
		m := meta.Node{ID: op.DiskID, Name: op.Name, Kind: meta.KindMirror, Host: op.NodeIdx, JoinedAt: time.Now().UnixNano()}
		nodes := n.FSM.Nodes()
		live := map[string]bool{}
		for i := range nodes {
			if cur := &nodes[i]; cur.Kind == meta.KindMirror && cur.State != meta.Removed {
				live[cur.Name] = true
			}
		}
		for i := range nodes {
			cur := &nodes[i]
			switch {
			case cur.ID == op.DiskID && cur.State != meta.Removed:
				m.Idx, m.Name, m.JoinedAt = cur.Idx, cur.Name, cur.JoinedAt // a re-home keeps the row's name
			case cur.ID == op.DiskID && m.Idx == 0 && !live[cur.Name]:
				m.Name = cur.Name // a disk removed and added back takes its old name, so its paths stay
			}
		}
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: m}})
	case "set_voter":
		if nd.Kind != meta.KindDevice {
			return fmt.Errorf("%w: only devices vote", ErrBadMemberOp)
		}
		row := nd
		row.Voter = op.Voter
		record := func() error {
			return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: row}})
		}
		// a leader that demotes itself steps down once the new configuration commits, so
		// the row is recorded first, while it can still propose (I-145)
		self := !op.Voter && nd.ID == n.ID
		if self {
			if err := record(); err != nil {
				return err
			}
		}
		if err := n.setSuffrage(nd, op.Voter); err != nil {
			return err
		}
		if self {
			return nil
		}
		return record()
	case "set_version":
		// the row carries the version serve runs, not the one it joined with (I-133)
		if op.Version == "" {
			return fmt.Errorf("%w: set_version needs version", ErrBadMemberOp)
		}
		nd.Version = op.Version
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	case "update_endpoints":
		// a device's candidates (docs/20 §5, I-322): the RPC server admits the op for
		// the caller's own row only; the FSM bounds the list
		if nd.Kind != meta.KindDevice {
			return fmt.Errorf("%w: update_endpoints names a device", ErrBadMemberOp)
		}
		nd.Endpoints = op.Endpoints
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	case "update_addr":
		if op.Addr == "" {
			return fmt.Errorf("%w: update_addr needs addr", ErrBadMemberOp)
		}
		nd.Addr = op.Addr
		var fut raft.IndexFuture
		if nd.Voter {
			fut = n.Raft.AddVoter(raft.ServerID(nd.ID), n.raftAddrFor(nd), 0, 10*time.Second)
		} else {
			fut = n.Raft.AddNonvoter(raft.ServerID(nd.ID), n.raftAddrFor(nd), 0, 10*time.Second)
		}
		if err := fut.Error(); err != nil {
			return raftErr("update raft address", err)
		}
		return propose(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
	}
	return fmt.Errorf("%w: unknown op %q", ErrBadMemberOp, op.Op)
}

// diskName is the name the leader gives a disk: the one it already has on a retry, else
// the requested name if no other disk has it, else "<host>/<n>" with the first free n.
// The caller's name comes from a status report that may be a moment old, so the leader,
// which sees every add, settles it.
func (n *Node) diskName(op rpc.MemberOp) string {
	taken := map[string]bool{}
	nodes := n.FSM.Nodes()
	for i := range nodes {
		nd := &nodes[i]
		if nd.Kind != meta.KindDisk {
			continue
		}
		if nd.ID == op.DiskID {
			return nd.Name
		}
		taken[nd.Name] = true
	}
	if op.Name != "" && !taken[op.Name] {
		return op.Name
	}
	host, _ := n.nodeByIdx(op.NodeIdx)
	for i := 1; ; i++ {
		if name := fmt.Sprintf("%s/%d", host.Name, i); !taken[name] {
			return name
		}
	}
}

// zeroSlots proposes SetPlacement with slot 0 for every chunk that places a shard on idx.
func (n *Node) zeroSlots(idx meta.NodeIdx, propose func(meta.Command) error) error {
	var victims []meta.Chunk
	n.FSM.ForEachChunk(func(c meta.Chunk) bool {
		if slices.Contains(c.Placement, idx) {
			victims = append(victims, c)
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
		err := propose(meta.Command{Op: meta.OpSetPlacement, SetPlacement: &meta.SetPlacement{Hash: c.Hash, K: c.K, M: c.M, Placement: place, Expect: c.Placement}})
		if errors.Is(err, meta.ErrConflict) {
			continue // re-placed meanwhile (I-102): whatever landed no longer names idx, or the repairer will see it
		}
		if err != nil {
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

// setSuffrage gives nd's Raft server the vote or takes it away. Taking it is DemoteVoter:
// raft's AddNonvoter on a server that already votes changes only its address, which left
// a demoted device voting while its row said otherwise (I-145).
func (n *Node) setSuffrage(nd meta.Node, voter bool) error {
	id := raft.ServerID(nd.ID)
	var fut raft.IndexFuture
	switch {
	case voter:
		fut = n.Raft.AddVoter(id, n.raftAddrFor(nd), 0, 10*time.Second)
	case n.votes(id):
		fut = n.Raft.DemoteVoter(id, 0, 10*time.Second)
	default:
		fut = n.Raft.AddNonvoter(id, n.raftAddrFor(nd), 0, 10*time.Second)
	}
	if err := fut.Error(); err != nil {
		return raftErr("change suffrage", err)
	}
	return nil
}

// votes reports whether id is a voter in the current Raft configuration.
func (n *Node) votes(id raft.ServerID) bool {
	f := n.Raft.GetConfiguration()
	if f.Error() != nil {
		return false
	}
	for _, s := range f.Configuration().Servers {
		if s.ID == id {
			return s.Suffrage == raft.Voter
		}
	}
	return false
}

// proposeApplied proposes cmd and waits until this node has applied it.
func (n *Node) proposeApplied(ctx context.Context, cmd meta.Command) error {
	res, index, err := n.Propose(ctx, cmd)
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return err
	}
	return n.FSM.WaitApplied(ctx, index)
}

// addS3 registers a bucket as an S3 node, once (docs/10 §4.1, I-137): the name must be
// free, the endpoint and bucket not registered under any prefix, and the ID either new or
// the S3 row an earlier attempt of the same add created.
func (n *Node) addS3(ctx context.Context, op rpc.MemberOp) error {
	sp := op.S3
	if sp == nil || op.Name == "" || op.Capacity == 0 || sp.Bucket == "" {
		return fmt.Errorf("%w: add_s3 needs a name, a bucket and a capacity", ErrBadMemberOp)
	}
	if len(sp.ID) != 16 {
		return fmt.Errorf("%w: add_s3 needs a 16-character id", ErrBadMemberOp)
	}
	u, err := s3.ParseEndpoint(sp.Endpoint)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadMemberOp, err)
	}
	endpoint := strings.ToLower(u.Scheme + "://" + u.Host)
	nd := meta.Node{
		ID: sp.ID, Name: op.Name, Kind: meta.KindS3, Capacity: op.Capacity, JoinedAt: time.Now().UnixNano(),
		S3Endpoint: endpoint, S3Bucket: sp.Bucket, S3Prefix: strings.Trim(sp.Prefix, "/"), S3Region: sp.Region, S3PathStyle: sp.PathStyle,
	}
	nodes := n.FSM.Nodes()
	for i := range nodes {
		cur := &nodes[i]
		if cur.State == meta.Removed {
			continue
		}
		if cur.ID == sp.ID {
			if cur.Kind != meta.KindS3 {
				return fmt.Errorf("%w: id %s belongs to %s", ErrBadMemberOp, sp.ID, cur.Name)
			}
			nd.Idx, nd.JoinedAt = cur.Idx, cur.JoinedAt // a retry of the add that created it
			continue
		}
		if cur.Name == op.Name {
			return fmt.Errorf("%w: a member is already named %q", ErrBadMemberOp, op.Name)
		}
		if cur.Kind == meta.KindS3 && cur.S3Endpoint == endpoint && cur.S3Bucket == sp.Bucket {
			return fmt.Errorf("%w: bucket %s at %s is already S3 node %s (one row per bucket: a second would be counted as another failure domain)", ErrBadMemberOp, sp.Bucket, endpoint, cur.Name)
		}
	}
	n.log.Info("member op", "op", op.Op, "name", op.Name, "endpoint", endpoint, "bucket", sp.Bucket, "prefix", nd.S3Prefix)
	return n.proposeApplied(ctx, meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: nd}})
}
