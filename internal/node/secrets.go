package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// s3Keys is the credentials this node serves its buckets with: the store's, then its
// file's for the names the store cannot serve (docs/19 §6, I-308).
func s3Keys(store *secrets.Store, file func() map[string]s3.Credentials) func() map[string]s3.Credentials {
	return func() map[string]s3.Credentials {
		return cluster.MergeS3Keys(store.Opened(cluster.S3SecretPrefix), file())
	}
}

// migrateLegacy imports this device's s3nodes.yaml and tmdb.key into the store, each
// only into names the store lacks, and removes a file once every name in it is stored
// and opens here (docs/19 §6, I-308). It reports whether neither file is left.
func migrateLegacy(ctx context.Context, store *secrets.Store, dataDir string) (bool, error) {
	files := map[string]map[string]string{}
	if keys, err := cluster.LoadS3Keys(dataDir); err != nil {
		return false, err
	} else if len(keys) > 0 {
		entries := map[string]string{}
		for name, c := range keys {
			v, err := cluster.EncodeS3Secret(c)
			if err != nil {
				return false, err
			}
			n, _ := cluster.S3SecretName(name) // off the pattern: Import leaves it, and the file
			entries[n] = v
		}
		files[cluster.S3KeysFile] = entries
	}
	b, err := os.ReadFile(filepath.Join(dataDir, secrets.TMDBLegacyFile)) //nolint:gosec // the data dir's own key file
	switch {
	case err == nil:
		if k := strings.TrimSpace(string(b)); k != "" {
			files[secrets.TMDBLegacyFile] = map[string]string{secrets.TMDBName: k}
		} else {
			files[secrets.TMDBLegacyFile] = nil // an empty file holds nothing to keep
		}
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	left := 0
	for _, file := range []string{cluster.S3KeysFile, secrets.TMDBLegacyFile} {
		entries, ok := files[file]
		if !ok {
			continue
		}
		done, err := store.Import(ctx, entries)
		if err != nil {
			return false, fmt.Errorf("import %s: %w", file, err)
		}
		if !done {
			left++
			continue
		}
		if err := os.Remove(filepath.Join(dataDir, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return left == 0, nil
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

// keepKey runs fetchKey once a minute, so a device that joined, was reinstalled or was
// upgraded opens its buckets' credentials without a secret command run on it (the hot
// path never fetches, I-308). It logs a fetch, and each new reason one failed.
func (a *adminOps) keepKey(ctx context.Context) {
	if a.secrets == nil || !a.healthy() {
		return
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

// importLegacy runs migrateLegacy once a minute until no old file is left, logging an
// import and each new reason it could not finish (a refusal repeats every minute during a
// rolling upgrade).
func (a *adminOps) importLegacy(ctx context.Context) {
	if a.secrets == nil || a.legacyDone || !a.healthy() {
		return
	}
	had := false
	for _, f := range []string{cluster.S3KeysFile, secrets.TMDBLegacyFile} {
		if _, err := os.Stat(filepath.Join(a.dataDir, f)); err == nil {
			had = true
		}
	}
	if !had {
		a.legacyDone = true
		return
	}
	done, err := migrateLegacy(ctx, a.secrets, a.dataDir)
	switch {
	case err != nil:
		if why := err.Error(); why != a.legacyWhy {
			a.legacyWhy = why
			a.log.Warn("secrets: the old key files are not imported yet; they keep serving", "err", err)
		}
	case done:
		a.legacyDone = true
		a.log.Info("secrets: imported this device's old key files into the store and removed them")
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
