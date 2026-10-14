package soak_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/secrets"
	"github.com/zombar/tailraid/internal/soak"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/testseed"
)

// deviceKeys is the keychains of the harness's devices, by node position: a device's
// keychain outlives a restart of its node, as the OS keyring does.
type deviceKeys struct {
	mu   sync.Mutex
	vals map[int]map[string]string
}

var harnessKeys = struct {
	sync.Mutex
	byEnv map[*harnessEnv]*deviceKeys
}{byEnv: map[*harnessEnv]*deviceKeys{}}

func (e *harnessEnv) keys() *deviceKeys {
	harnessKeys.Lock()
	defer harnessKeys.Unlock()
	k, ok := harnessKeys.byEnv[e]
	if !ok {
		k = &deviceKeys{vals: map[int]map[string]string{}}
		harnessKeys.byEnv[e] = k
	}
	return k
}

// keychainOf is one device's keychain.
type keychainOf struct {
	k *deviceKeys
	i int
}

func (c keychainOf) Get(a string) (string, string, error) {
	c.k.mu.Lock()
	defer c.k.mu.Unlock()
	v, ok := c.k.vals[c.i][a]
	if !ok {
		return "", "", errors.New("not found")
	}
	return v, "keyring", nil
}

func (c keychainOf) Set(a, v string) (string, error, error) {
	c.k.mu.Lock()
	defer c.k.mu.Unlock()
	if c.k.vals[c.i] == nil {
		c.k.vals[c.i] = map[string]string{}
	}
	c.k.vals[c.i][a] = v
	return "keyring", nil, nil
}

// secretStore is the store as harness node i sees it now: its current FSM and proposer,
// its device's keychain, and a key fetch that asks every other live node.
func (e *harnessEnv) secretStore(i int) *secrets.Store {
	e.mu.RLock()
	nd := e.c.Nodes[i]
	e.mu.RUnlock()
	self := nd.Idx()
	s := &secrets.Store{
		State: nd.Node.FSM,
		Keys:  keychainOf{e.keys(), i},
		Self:  func() meta.NodeIdx { return self },
		Now:   func() int64 { return time.Now().UnixNano() },
	}
	s.Propose = func(ctx context.Context, c meta.Command) error {
		res, index, err := nd.Node.Propose(ctx, c)
		if err == nil {
			err = res.Err
		}
		if err != nil {
			return err
		}
		return nd.Node.FSM.WaitApplied(ctx, index)
	}
	s.Fetch = func(context.Context) ([]byte, error) {
		for j := range e.c.Nodes {
			if j == i || !e.c.Alive(j) {
				continue
			}
			if k, ok := e.secretStore(j).Local(); ok {
				return k, nil
			}
		}
		return nil, secrets.ErrNoKey
	}
	return s
}

func (e *harnessEnv) SecretSet(ctx context.Context, n soak.NodeRef, name, value string) error {
	return e.secretStore(e.node(n).I).Set(ctx, 0, name, value)
}

func (e *harnessEnv) SecretGet(ctx context.Context, n soak.NodeRef, name string) (string, error) {
	return e.secretStore(e.node(n).I).Get(ctx, name)
}

func (e *harnessEnv) SecretList(ctx context.Context, n soak.NodeRef) (json.RawMessage, error) {
	return json.Marshal(e.secretStore(e.node(n).I).List(ctx))
}

func (e *harnessEnv) SecretRemove(ctx context.Context, n soak.NodeRef, name string) error {
	return e.secretStore(e.node(n).I).Remove(ctx, 0, name, false)
}

// ForgetSecretKey empties the device's keychain: the store is built afresh on every call,
// so nothing in memory remembers the key either.
func (e *harnessEnv) ForgetSecretKey(_ context.Context, n soak.NodeRef) error {
	k := e.keys()
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.vals, e.node(n).I)
	return nil
}

var _ soak.SecretEnv = (*harnessEnv)(nil)

// Phase 24 on the in-process cluster: a secret set through one node reads back through
// every node over real Raft, a node without its key fetches it, the others read it with
// its setter killed, and a removal replicates; every check runs, none is skipped.
//
// Invariant: I-303, I-304, I-306, I-307
func TestSoak_SecretsAgainstHarness(t *testing.T) {
	c := testharness.NewCluster(t, 4, 2, 1)
	defer c.Close()
	env := &harnessEnv{t: t, c: c}
	var jsonl bytes.Buffer
	seed, rng := testseed.New(t)
	run := &soak.Run{Env: env, Profile: harnessProfile(), Out: &jsonl, Rng: rng, Seed: seed, Phases: soak.StandardPhases("24")}
	sum, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Phases) != 1 || sum.Phases[0].Skipped {
		t.Fatalf("phases: %+v", sum.Phases)
	}
	ph := sum.Phases[0]
	for _, chk := range ph.Checks {
		if chk.OK == nil || !*chk.OK {
			t.Errorf("seed %d: %s: %s", seed, chk.Name, chk.Text)
		}
	}
	if n := len(c.Nodes); len(ph.Checks) != n+n+1+1+1+1+n { // (a) and (b) per node, (c), (d), (e), the removal, and (f) per node
		t.Errorf("%d checks, want %d", len(ph.Checks), 3*n+4)
	}
}
