package admin

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
)

// SlotReport is one placement slot of a chunk as seen from the reporting node.
type SlotReport struct {
	Idx     int    `json:"idx"`
	Node    int    `json:"node"` // 0 = missing slot
	Name    string `json:"name,omitempty"`
	Live    bool   `json:"live"`
	Present *bool  `json:"present,omitempty"` // HEAD result; nil when not checked (dead node)
}

// ChunkReport is `tailraid chunk describe` (docs/11 §7): the state derived from the FSM
// copy and liveness, the referencing files, and this node's event ring.
type ChunkReport struct {
	Hash         string            `json:"hash"`
	Found        bool              `json:"found"`
	State        string            `json:"state"` // committed | degraded | unreadable | tombstoned | unknown
	Size         uint32            `json:"size"`
	K            uint8             `json:"k"`
	M            uint8             `json:"m"`
	Refs         uint32            `json:"refs"`
	PolicyStale  bool              `json:"policy_stale"`
	TombstonedAt string            `json:"tombstoned_at,omitempty"`
	LiveSlots    int               `json:"live_slots"`
	Slots        []SlotReport      `json:"slots"`
	Files        []string          `json:"files,omitempty"`
	EventRing    bool              `json:"event_ring"` // false: "(event ring disabled)"
	Events       []meta.ChunkEvent `json:"events,omitempty"`
}

// ShardChecker answers whether a node holds a shard (rpc.Client.Has).
type ShardChecker interface {
	Has(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey) (bool, error)
}

// ErrAmbiguous is returned for a hash prefix matching more than one chunk.
var ErrAmbiguous = errors.New("admin: hash prefix matches several chunks")

// DescribeChunks resolves sel — a hex hash or prefix (≥ 8 hex digits) or a /path (every
// chunk of that file, in order) — and builds one report per chunk. hc may be nil.
func DescribeChunks(ctx context.Context, st *meta.Store, p *cluster.Prober, hc ShardChecker, ring *meta.EventRing, sel string) ([]ChunkReport, error) {
	hashes, err := selectChunks(st, sel)
	if err != nil {
		return nil, err
	}
	out := make([]ChunkReport, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, DescribeChunk(ctx, st, p, hc, ring, h))
	}
	return out, nil
}

func selectChunks(st *meta.Store, sel string) ([]meta.Hash, error) {
	if strings.HasPrefix(sel, "/") {
		in, err := st.Resolve(splitPath(sel))
		if err != nil {
			return nil, err
		}
		if in.Type != meta.TFile {
			return nil, meta.ErrIsDir
		}
		return append([]meta.Hash(nil), in.Chunks...), nil
	}
	sel = strings.ToLower(sel)
	if len(sel) < 8 || len(sel) > 64 || len(sel)%2 != 0 {
		return nil, fmt.Errorf("admin: chunk selector %q: want a /path or 8–64 hex digits", sel)
	}
	prefix, err := hex.DecodeString(sel)
	if err != nil {
		return nil, fmt.Errorf("admin: chunk selector %q: %w", sel, err)
	}
	var matches []meta.Hash
	st.ForEachChunk(func(c meta.Chunk) bool {
		if bytes.Equal(c.Hash[:len(prefix)], prefix) {
			matches = append(matches, c.Hash)
		}
		return len(matches) < 2
	})
	switch len(matches) {
	case 0:
		var h meta.Hash
		copy(h[:], prefix)
		return []meta.Hash{h}, nil // reported as not found (len 32 only); a prefix: still "unknown"
	case 1:
		return matches, nil
	default:
		return nil, ErrAmbiguous
	}
}

func splitPath(p string) []string { return meta.SplitPath(p) }

// DescribeChunk builds the report for one hash.
func DescribeChunk(ctx context.Context, st *meta.Store, p *cluster.Prober, hc ShardChecker, ring *meta.EventRing, h meta.Hash) ChunkReport {
	rep := ChunkReport{Hash: hex.EncodeToString(h[:]), State: "unknown", EventRing: ring != nil, Events: ring.Of(h)}
	if h == meta.HashZero {
		rep.State = "zero"
		return rep
	}
	c, ok := st.GetChunk(h)
	if !ok {
		return rep
	}
	rep.Found = true
	rep.Size, rep.K, rep.M, rep.Refs = c.Size, c.K, c.M, c.Refs
	vol := st.Volume()
	if c.TombstonedAt != 0 {
		rep.TombstonedAt = time.Unix(0, c.TombstonedAt).UTC().Format(time.RFC3339)
	}
	names := map[meta.NodeIdx]string{}
	nodeByIdx := map[meta.NodeIdx]meta.Node{}
	nodes := st.Nodes()
	for i := range nodes {
		names[nodes[i].Idx] = nodes[i].Name
		nodeByIdx[nodes[i].Idx] = nodes[i]
	}
	var lv meta.Liveness = noLiveness{}
	if p != nil {
		lv = p
	}
	present := map[int]bool{}
	for i, n := range c.Placement {
		s := SlotReport{Idx: i, Node: int(n), Name: names[n]}
		if n != 0 {
			s.Live = lv.IsLive(n)
			if s.Live && hc != nil {
				hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				has, err := hc.Has(hctx, n, chunk.Key(h, c.K, c.M, i))
				cancel()
				if err == nil {
					s.Present = &has
					present[i] = has
				}
			}
		}
		rep.Slots = append(rep.Slots, s)
	}
	// the same derivation status, check and the repairer use (I-56), with the presence
	// answers folded in
	health := meta.ChunkState(c, vol, meta.HealthInput{Nodes: nodeByIdx, Live: lv, FillAfter: cluster.FillAfter, Present: present})
	rep.LiveSlots = health.Readable
	rep.PolicyStale = health.PolicyStale
	rep.State = health.State(c.K, c.M)
	rep.Files = FilesReferencing(st, map[meta.Hash]bool{h: true})
	return rep
}

// noLiveness is the view without a prober: nothing is live.
type noLiveness struct{}

func (noLiveness) IsLive(meta.NodeIdx) bool           { return false }
func (noLiveness) DeadFor(meta.NodeIdx) time.Duration { return 0 }
