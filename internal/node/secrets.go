package node

import (
	"context"
	"errors"
	"time"

	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/keychain"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/secrets"
)

// keyFetchTimeout bounds one member's answer to a key fetch.
const keyFetchTimeout = 10 * time.Second

// newSecrets is this node's view of the secrets store (docs/19): the device's keychain,
// proposals through the leader, and a key fetch that asks the leader first, then every
// other live device member (I-304).
func newSecrets(node *cluster.Node, prober *cluster.Prober, client *rpc.Client, keys secrets.Keychain, self meta.NodeIdx, clk clock.Clock) *secrets.Store {
	s := &secrets.Store{
		State: node.FSM,
		Keys:  keys,
		Self:  func() meta.NodeIdx { return self },
		Now:   func() int64 { return clk.Now().UnixNano() },
	}
	s.Propose = func(ctx context.Context, c meta.Command) error {
		res, index, err := node.Propose(ctx, c)
		if err == nil {
			err = res.Err
		}
		if err != nil {
			return err
		}
		return node.FSM.WaitApplied(ctx, index)
	}
	s.Fetch = func(ctx context.Context) ([]byte, error) {
		addrs := []string{}
		if l := node.LeaderRPCAddr(); l != "" {
			addrs = append(addrs, l)
		}
		nodes := node.FSM.Nodes()
		for i := range nodes {
			n := &nodes[i]
			if n.Kind == meta.KindDevice && n.State != meta.Removed && n.Idx != self && n.Addr != "" && prober.IsLive(n.Idx) {
				addrs = append(addrs, n.RPCAddr())
			}
		}
		seen := map[string]bool{}
		for _, a := range addrs {
			if seen[a] {
				continue
			}
			seen[a] = true
			c, cancel := context.WithTimeout(ctx, keyFetchTimeout)
			k, err := client.SecretKey(c, a)
			cancel()
			if err == nil {
				return k, nil
			}
		}
		return nil, secrets.ErrNoKey
	}
	return s
}

// deviceKeychain is the keychain of the device a node runs on, under its data dir.
func deviceKeychain(dataDir string) *keychain.Store { return keychain.New(dataDir) }

// secretOp answers secret_set, secret_get, secret_ls and secret_rm (docs/19 §5) on this
// node: it seals, opens and proposes, so a value never travels further than the node the
// CLI talks to, and never into a log line (I-306).
func (a *adminOps) secretOp(ctx context.Context, op rpc.AdminOp) (any, error) {
	if a.secrets == nil {
		return nil, errors.New("secrets: this node has no store")
	}
	switch op.Op {
	case "secret_set":
		if err := a.secrets.Set(ctx, op.Name, op.Value); err != nil {
			return nil, err
		}
		return map[string]string{"name": op.Name}, nil
	case "secret_get":
		v, err := a.secrets.Get(ctx, op.Name)
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": op.Name, "value": v}, nil
	case "secret_ls":
		return a.secrets.List(ctx), nil
	default: // secret_rm
		if err := a.secrets.Remove(ctx, op.Name, op.All); err != nil {
			return nil, err
		}
		return map[string]any{"name": op.Name, "all": op.All}, nil
	}
}
