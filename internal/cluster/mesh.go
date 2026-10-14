package cluster

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// The mesh's membership (docs/20 §4): the introduce that gives a joiner its address as a
// pending row, and the expiry of pending rows no join followed.

// ErrNoIntroduce is a mesh join whose key was never introduced.
var ErrNoIntroduce = errors.New("cluster: no introduce for that key")

// ParseMeshKey reads a base64 public key as the join request carries it.
func ParseMeshKey(s string) (meta.MeshKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != len(meta.MeshKey{}) {
		return meta.MeshKey{}, fmt.Errorf("%w: bad mesh key %q", ErrBadMemberOp, s)
	}
	var k meta.MeshKey
	copy(k[:], b)
	return k, nil
}

// Introduce answers an introduce (docs/20 §4, I-319): the key's address, as a pending
// row the leader allocates or the row the key already has. Any member may run it: the
// proposal is forwarded to the leader, and the member waits for its own apply so the
// join that follows through this member finds the row.
func (n *Node) Introduce(ctx context.Context, key meta.MeshKey) (netip.Prefix, error) {
	if key.IsZero() {
		return netip.Prefix{}, fmt.Errorf("%w: a zero key", ErrBadMemberOp)
	}
	res, index, err := n.Propose(ctx, meta.Command{Op: meta.OpPendingAdd, PendingAdd: &meta.PendingAdd{Key: key, Now: time.Now().UnixNano()}})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("cluster: introduce: %w", err)
	}
	if err := n.FSM.WaitApplied(ctx, index); err != nil {
		return netip.Prefix{}, err
	}
	addr, err := netip.ParseAddr(res.Addr)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("cluster: introduce: the leader answered %q", res.Addr)
	}
	sub := n.FSM.Meta().MeshPrefix()
	if !sub.IsValid() {
		return netip.Prefix{}, meta.ErrNoSubnet
	}
	return netip.PrefixFrom(addr, sub.Bits()), nil
}

// ExpirePending removes the pending rows older than meta.PendingTTL as of now (I-319);
// the leader runs it on its tick. It reports how many it removed.
func (n *Node) ExpirePending(ctx context.Context, now time.Time) (int, error) {
	if !n.IsLeader() {
		return 0, nil
	}
	removed := 0
	for _, p := range n.FSM.Pending() {
		if now.Sub(time.Unix(0, p.At)) < meta.PendingTTL {
			continue
		}
		err := n.proposeApplied(ctx, meta.Command{Op: meta.OpPendingDelete, PendingDelete: &meta.PendingDelete{Key: p.Key, Now: now.UnixNano()}})
		if errors.Is(err, meta.ErrNoPending) {
			continue // its join landed meanwhile
		}
		if err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
