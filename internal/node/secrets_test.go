package node

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/secrets"
)

type memKeychain struct {
	mu   sync.Mutex
	vals map[string]string
}

func (m *memKeychain) Get(a string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.vals[a]; ok {
		return v, "keyring", nil
	}
	return "", "", errors.New("not found")
}

func (m *memKeychain) Set(a, v string) (string, error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[a] = v
	return "keyring", nil, nil
}

// oneNodeStore is a store over one FSM whose proposals apply at once, or fail with the
// error given to refuse.
func oneNodeStore(t *testing.T) (*secrets.Store, func(error)) {
	t.Helper()
	f := meta.NewFSM()
	var index uint64
	var refuse error
	apply := func(c meta.Command) error {
		index++
		return f.Apply(&raft.Log{Index: index, Data: meta.MustEncodeCommand(c)}).(meta.Result).Err
	}
	if err := apply(meta.Command{Op: meta.OpMetaSet, MetaSet: &meta.MetaSet{ClusterID: [16]byte{7}, JoinToken: "t", CreatedAt: 1}}); err != nil {
		t.Fatal(err)
	}
	s := &secrets.Store{
		State: f, Keys: &memKeychain{vals: map[string]string{}},
		Self: func() meta.NodeIdx { return 1 }, Now: func() int64 { return 1 },
		Propose: func(_ context.Context, c meta.Command) error {
			if refuse != nil {
				return refuse
			}
			return apply(c)
		},
	}
	return s, func(err error) { refuse = err }
}

// A device holding no copy of the cluster key fetches it on the maintenance tick, so a
// fresh join opens the store's S3 credentials without a secret command run on it; a
// device that holds it fetches nothing, a store with no key is left alone, and a fetch no
// member answers is reported and tried again next tick.
// Invariant: I-304, I-308
func TestFetchKey_OnTick(t *testing.T) {
	ctx := context.Background()
	s, _ := oneNodeStore(t)
	if fetched, err := fetchKey(ctx, s); fetched || err != nil {
		t.Fatalf("a store with no key: fetched %v, %v", fetched, err)
	}
	if err := s.Set(ctx, "s3/b2", "v"); err != nil {
		t.Fatal(err)
	}
	if fetched, err := fetchKey(ctx, s); fetched || err != nil {
		t.Fatalf("the node that made the key: fetched %v, %v", fetched, err)
	}
	k, _ := s.Local()
	calls := 0
	joined := &secrets.Store{
		State: s.State, Keys: &memKeychain{vals: map[string]string{}},
		Self: func() meta.NodeIdx { return 2 }, Now: func() int64 { return 2 },
		Fetch: func(context.Context) ([]byte, error) { calls++; return k, nil },
	}
	if got := joined.Opened(cluster.S3SecretPrefix); len(got) != 0 {
		t.Fatalf("opened without the key: %v", got)
	}
	if fetched, err := fetchKey(ctx, joined); !fetched || err != nil || calls != 1 {
		t.Fatalf("the joined device: fetched %v, %v, %d fetches", fetched, err, calls)
	}
	if got := joined.Opened(cluster.S3SecretPrefix); got["s3/b2"] != "v" {
		t.Fatalf("opened after the fetch: %v", got)
	}
	if fetched, err := fetchKey(ctx, joined); fetched || err != nil || calls != 1 {
		t.Fatalf("held: fetched %v, %v, %d fetches", fetched, err, calls)
	}
	alone := &secrets.Store{
		State: s.State, Keys: &memKeychain{vals: map[string]string{}},
		Self: func() meta.NodeIdx { return 3 }, Now: func() int64 { return 3 },
		Fetch: func(context.Context) ([]byte, error) { return nil, errors.New("no member answered") },
	}
	for range 2 {
		if fetched, err := fetchKey(ctx, alone); fetched || !errors.Is(err, secrets.ErrNoKey) {
			t.Fatalf("no member: fetched %v, %v", fetched, err)
		}
	}
}

// The leader makes the cluster key on its tick when the cluster has none, once; a
// follower makes nothing (docs/19 §3): the key is there from the cluster's first minute.
// Invariant: I-303, I-305
func TestMakeKey_OnLeaderTick(t *testing.T) {
	ctx := context.Background()
	s, _ := oneNodeStore(t)
	if made, err := makeKeyIfLeader(ctx, s, false); made || err != nil {
		t.Fatalf("a follower: made %v, %v", made, err)
	}
	if _, ok := s.Local(); ok {
		t.Fatal("a follower holds a key")
	}
	if made, err := makeKeyIfLeader(ctx, s, true); !made || err != nil {
		t.Fatalf("the leader: made %v, %v", made, err)
	}
	k, ok := s.Local()
	if !ok || len(k) != secrets.KeySize {
		t.Fatalf("the leader holds no key after making one: %v", ok)
	}
	if made, err := makeKeyIfLeader(ctx, s, true); made || err != nil {
		t.Fatalf("a second tick: made %v, %v (the key is made once)", made, err)
	}
}
