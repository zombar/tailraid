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
	"github.com/zombar/tailraid/internal/s3"
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
		if l := node.LeaderRPCAddr(); l != "" && !node.IsLeader() { // the leader never asks itself
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

// s3Keys is the credentials this node serves its buckets with: the store's pairs this
// node opens now (docs/19 §6, I-308).
func s3Keys(store *secrets.Store) func() map[string]s3.Credentials {
	return func() map[string]s3.Credentials {
		return cluster.S3KeysFromSecrets(store.Opened(cluster.S3SecretPrefix))
	}
}

// fetchKey fetches the cluster key when the store has one and this device holds no copy,
// and reports whether it did: false when the store has no key or the device already
// holds it (docs/19 §3, I-304).
func fetchKey(ctx context.Context, s *secrets.Store) (bool, error) {
	if s.State.SecretKeyID() == (meta.KeyID{}) {
		return false, nil
	}
	if _, ok := s.Local(); ok {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*keyFetchTimeout)
	defer cancel()
	if _, err := s.Key(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// makeKeyIfLeader makes the cluster key on the leader when the cluster has none yet
// (docs/19 §3): the node that ran init leads first, so a fresh cluster has its key from
// its first tick. A follower makes nothing; a lost race fetches the winner's key.
func makeKeyIfLeader(ctx context.Context, s *secrets.Store, leader bool) (bool, error) {
	if !leader || s.State.SecretKeyID() != (meta.KeyID{}) {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, keyFetchTimeout)
	defer cancel()
	if _, err := s.EnsureKey(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// keepKey runs fetchKey once a minute, so a device that joined, was reinstalled or was
// upgraded opens its buckets' credentials without a secret command run on it (the hot
// path never fetches, I-308). It logs a fetch, and each new reason one failed.
func (a *adminOps) keepKey(ctx context.Context) {
	if a.secrets == nil || !a.healthy() {
		return
	}
	if made, err := makeKeyIfLeader(ctx, a.secrets, a.node.IsLeader()); err != nil {
		a.log.Warn("secrets: the cluster key was not made", "err", err)
	} else if made {
		a.log.Info("secrets: made the cluster key (docs/19 §3)")
	}
	fetched, err := fetchKey(ctx, a.secrets)
	switch {
	case err != nil:
		if why := err.Error(); why != a.keyWhy {
			a.keyWhy = why
			a.log.Warn("secrets: this device holds no copy of the cluster key yet; the store's secrets do not open here", "err", err)
		}
	case fetched:
		a.keyWhy = ""
		a.log.Info("secrets: fetched the cluster key from a member into this device's keychain")
	}
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
