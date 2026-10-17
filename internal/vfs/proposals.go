package vfs

import (
	"context"
	"strconv"
	"sync/atomic"

	"github.com/zombar/tailraid/internal/meta"
)

// Proposals counts what a gateway proposes, by op (docs/04 §6, docs/12 §8, I-403): the
// filesystem's and the flusher's metadata, so a copy's cost in Raft round trips shows.
// It is a meta.Proposer over the one it wraps.
type Proposals struct {
	meta.Proposer
	n [64]atomic.Uint64
}

// CountProposals wraps p.
func CountProposals(p meta.Proposer) *Proposals { return &Proposals{Proposer: p} }

// Propose counts cmd and proposes it.
func (p *Proposals) Propose(ctx context.Context, cmd meta.Command) (meta.Result, uint64, error) {
	if int(cmd.Op) < len(p.n) {
		p.n[cmd.Op].Add(1)
	}
	return p.Proposer.Propose(ctx, cmd)
}

// Counts is the proposals made since start by op name, the ops never proposed left out.
func (p *Proposals) Counts() map[string]uint64 {
	out := map[string]uint64{}
	for op := range p.n {
		if v := p.n[op].Load(); v > 0 {
			out[OpName(meta.Op(op))] = v //nolint:gosec // op < 64
		}
	}
	return out
}

// opNames are the ops a status report names.
var opNames = map[meta.Op]string{
	meta.OpCreate: "create", meta.OpMkdir: "mkdir", meta.OpUnlink: "unlink", meta.OpRmdir: "rmdir",
	meta.OpRename: "rename", meta.OpSetAttr: "setattr", meta.OpCommit: "commit",
	meta.OpSetPlacement: "set_placement", meta.OpTrash: "trash", meta.OpRestore: "restore",
	meta.OpPurge: "purge", meta.OpSetShardSums: "set_shard_sums",
}

// OpName is op's name in status, "op<n>" for one without.
func OpName(op meta.Op) string {
	if s, ok := opNames[op]; ok {
		return s
	}
	return "op" + strconv.Itoa(int(op))
}
