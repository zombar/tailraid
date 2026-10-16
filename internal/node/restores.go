package node

import (
	"context"
	"slices"
	"time"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/restore"
	"github.com/zombar/tailraid/internal/rpc"
)

// clusterRestores is every restore job of every live host, the same list on every node
// (I-365): this host's from its own list, another's from its GET /v1/restores while its
// last /health counted any. A host that does not answer in time is left out until the next
// build.
func clusterRestores(ctx context.Context, nodes []meta.Node, probes map[meta.NodeIdx]cluster.Probe, live func(meta.NodeIdx) bool, self meta.NodeIdx, local []rpc.RestoreJob, fetch func(ctx context.Context, idx meta.NodeIdx) ([]rpc.RestoreJob, error)) []rpc.RestoreJob {
	out := slices.Clone(local)
	for i := range nodes {
		n := &nodes[i]
		if n.Kind != meta.KindDevice || n.Idx == self || probes[n.Idx].Restores == 0 || !live(n.Idx) {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, restoreFetchTimeout)
		jobs, err := fetch(fctx, n.Idx)
		cancel()
		if err == nil {
			out = append(out, jobs...)
		}
	}
	restore.Sort(out)
	return out
}

// restoreFetchTimeout bounds one host's GET /v1/restores in a status build.
const restoreFetchTimeout = 2 * time.Second
