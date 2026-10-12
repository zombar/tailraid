package node

import (
	"context"
	"fmt"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/mirror"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/vfs"
)

// get is mirror row idx when it is open on this host.
func (s *mirrorSet) get(idx meta.NodeIdx) (*mirror.Mirror, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.open[idx]
	if !ok || d.m == nil {
		return nil, false
	}
	return d.m, true
}

// mirrorSource is how the gateways read a mirror (docs/16 §6): from the disk when it is
// open on this host, else from its host over the node RPC. A remote mirror is online
// while its host is live; a host that has the disk detached answers "not open", which a
// read sees as EIO.
type mirrorSource struct {
	local  *mirrorSet
	client *rpc.Client
	st     *meta.Store
	live   func(meta.NodeIdx) bool
}

var _ vfs.MirrorSource = mirrorSource{}

func (s mirrorSource) Tree(ctx context.Context, m meta.NodeIdx, h meta.Hash) (mirror.Tree, error) {
	if mm, ok := s.local.get(m); ok {
		return mm.Tree(h)
	}
	if s.mine(m) {
		return mirror.Tree{}, fmt.Errorf("mirror %d is not attached here", m)
	}
	return s.client.MirrorTree(ctx, m, h)
}

func (s mirrorSource) Object(ctx context.Context, m meta.NodeIdx, h meta.Hash) ([]byte, error) {
	if mm, ok := s.local.get(m); ok {
		return mm.Object(h)
	}
	if s.mine(m) {
		return nil, fmt.Errorf("mirror %d is not attached here", m)
	}
	return s.client.MirrorObject(ctx, m, h)
}

func (s mirrorSource) Online(m meta.NodeIdx) bool {
	if _, ok := s.local.get(m); ok {
		return true
	}
	row, ok := s.st.Node(m)
	if !ok || row.Kind != meta.KindMirror || row.State == meta.Removed || s.mine(m) {
		return false
	}
	return s.live(row.Host)
}

// mine is whether mirror m's row is on this host (which then knows it is not attached).
func (s mirrorSource) mine(m meta.NodeIdx) bool {
	row, ok := s.st.Node(m)
	self, selfOK := s.local.self()
	return ok && selfOK && row.Host == self
}
