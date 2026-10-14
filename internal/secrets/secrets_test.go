package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/meta"
)

// memKeys is a device keychain in memory.
type memKeys struct {
	mu   sync.Mutex
	vals map[string]string
}

func (m *memKeys) Get(a string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[a]
	if !ok {
		return "", "", errors.New("not found")
	}
	return v, "keyring", nil
}

func (m *memKeys) Set(a, v string) (string, error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[a] = v
	return "keyring", nil, nil
}

// cluster is n nodes, each with its own FSM and keychain, and one log: a proposal is
// applied to every FSM in order, as Raft would (replication in miniature).
type cluster struct {
	mu     sync.Mutex
	index  uint64
	fsms   []*meta.FSM
	keys   []*memKeys
	stores []*Store
	// before runs ahead of each proposal, outside the log's lock (a race's barrier)
	before func(c meta.Command)
}

func newCluster(t *testing.T, n int) *cluster {
	t.Helper()
	c := &cluster{}
	id := [16]byte{0xc1, 0x05}
	for i := range n {
		f := meta.NewFSM()
		c.fsms = append(c.fsms, f)
		k := &memKeys{vals: map[string]string{}}
		c.keys = append(c.keys, k)
		self := meta.NodeIdx(i + 1)
		s := &Store{State: f, Keys: k, Self: func() meta.NodeIdx { return self }, Now: func() int64 { return 1 }}
		s.Propose = c.propose
		c.stores = append(c.stores, s)
	}
	if err := c.apply(meta.Command{Op: meta.OpMetaSet, MetaSet: &meta.MetaSet{ClusterID: id, JoinToken: "t", CreatedAt: 1}}); err != nil {
		t.Fatal(err)
	}
	for i, s := range c.stores {
		s.Fetch = c.fetcher(i)
	}
	return c
}

func (c *cluster) apply(cmd meta.Command) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index++
	var err error
	for _, f := range c.fsms {
		r := f.Apply(&raft.Log{Index: c.index, Data: meta.MustEncodeCommand(cmd)}).(meta.Result)
		err = r.Err
	}
	return err
}

func (c *cluster) propose(_ context.Context, cmd meta.Command) error {
	if c.before != nil {
		c.before(cmd)
	}
	return c.apply(cmd)
}

// fetcher asks every other node's Local, as GET /v1/secrets/key does.
func (c *cluster) fetcher(self int) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		for i, s := range c.stores {
			if i == self {
				continue
			}
			if k, ok := s.Local(); ok {
				return k, nil
			}
		}
		return nil, ErrNoKey
	}
}

// A value sealed for one name opens for it under its key and nothing else: another name,
// another key or one changed bit fail, and two seals of one value differ.
// Invariant: I-306
func TestSeal_Open(t *testing.T) {
	key, other := make([]byte, KeySize), make([]byte, KeySize)
	_, _ = rand.Read(key)
	_, _ = rand.Read(other)
	id := [16]byte{1}
	n1, c1, err := Seal(key, id, "tmdb", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	n2, c2, _ := Seal(key, id, "tmdb", []byte("value"))
	if n1 == n2 || bytes.Equal(c1, c2) || bytes.Contains(c1, []byte("value")) {
		t.Error("seals of one value are alike or carry it in the clear")
	}
	good := meta.Secret{Name: "tmdb", Nonce: n1, Cipher: c1}
	if v, err := Open(key, id, good); err != nil || string(v) != "value" {
		t.Fatalf("open: %q %v", v, err)
	}
	moved := good
	moved.Name = "s3/b2"
	flipped := good
	flipped.Cipher = bytes.Clone(c1)
	flipped.Cipher[0] ^= 1
	for name, tc := range map[string]struct {
		key []byte
		id  [16]byte
		s   meta.Secret
	}{"other name": {key, id, moved}, "other key": {other, id, good}, "other cluster": {key, [16]byte{2}, good}, "flipped bit": {key, id, flipped}} {
		if _, err := Open(tc.key, tc.id, tc.s); !errors.Is(err, ErrOpen) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A secret set through one node opens through every other: a node without the key
// fetches it from a member, checks its ID and keeps it in its keychain; the FSM holds
// only the sealed value; a listing names it without its value.
// Invariant: I-303, I-304, I-306
func TestStore_ReplicatedAcrossNodes(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 3)
	if err := c.stores[0].Set(ctx, "tmdb", "k3y-value"); err != nil {
		t.Fatal(err)
	}
	for i, s := range c.stores {
		if v, err := s.Get(ctx, "tmdb"); err != nil || v != "k3y-value" {
			t.Fatalf("node %d: %q %v", i+1, v, err)
		}
		if len(c.keys[i].vals) != 1 {
			t.Errorf("node %d keeps %d keys", i+1, len(c.keys[i].vals))
		}
	}
	for i, f := range c.fsms {
		sec, _ := f.Secret("tmdb")
		if bytes.Contains(sec.Cipher, []byte("k3y-value")) || sec.By != 1 {
			t.Errorf("node %d's FSM: %+v", i+1, sec)
		}
	}
	for _, e := range c.stores[2].List(ctx) {
		if e.Name != "tmdb" || !e.Readable {
			t.Errorf("list: %+v", e)
		}
	}
	// replaced through another node, read through the first
	if err := c.stores[2].Set(ctx, "tmdb", "second"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.stores[0].Get(ctx, "tmdb"); v != "second" {
		t.Errorf("after the replace: %q", v)
	}
	// a removal reaches every node
	if err := c.stores[1].Remove(ctx, "tmdb", false); err != nil {
		t.Fatal(err)
	}
	for i, s := range c.stores {
		if _, err := s.Get(ctx, "tmdb"); !errors.Is(err, ErrNotFound) {
			t.Errorf("node %d after the removal: %v", i+1, err)
		}
	}
}

// A fetched key is used only when its ID is the FSM's; a node never fetches when it
// answers another, and answers only with a key whose ID is the FSM's.
// Invariant: I-304
func TestStore_FetchChecksTheKeyID(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 2)
	if err := c.stores[0].Set(ctx, "tmdb", "v"); err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, KeySize)
	c.stores[1].Fetch = func(context.Context) ([]byte, error) { return wrong, nil }
	if _, err := c.stores[1].Get(ctx, "tmdb"); !errors.Is(err, ErrNoKey) || len(c.keys[1].vals) != 0 {
		t.Errorf("a wrong key: %v, kept %d", err, len(c.keys[1].vals))
	}
	if _, ok := c.stores[1].Local(); ok {
		t.Error("a node without the key answered with one")
	}
	c.keys[1].vals[account(c.fsms[1].Meta().ClusterID)] = "bm90IHRoZSBrZXk=" // a stale value in its keychain
	if _, ok := c.stores[1].Local(); ok {
		t.Error("a node answered with a keychain value whose ID is not the FSM's")
	}
}

// Of two nodes making the first key at once, one key becomes the store's: the other
// node's proposal is refused and it fetches the winner's.
// Invariant: I-305
func TestStore_KeyRace(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 2)
	var both sync.WaitGroup
	both.Add(2)
	c.before = func(cmd meta.Command) {
		if cmd.Op == meta.OpSecretKeyInit {
			both.Done()
			both.Wait() // both have made a key and checked the FSM had none
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { errs[i] = c.stores[i].Set(ctx, []string{"a", "b"}[i], "value") })
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("sets: %v", errs)
	}
	id := c.fsms[0].SecretKeyID()
	for i, s := range c.stores {
		k, err := s.Key(ctx)
		if err != nil || IDOf(k) != id {
			t.Errorf("node %d holds another key: %v", i+1, err)
		}
		for _, n := range []string{"a", "b"} {
			if v, err := s.Get(ctx, n); err != nil || v != "value" {
				t.Errorf("node %d reads %s: %q %v", i+1, n, v, err)
			}
		}
	}
}

// A name off its pattern and a value that is empty, past 16 KiB or on two lines are
// refused before anything is proposed; removing every secret clears the key, and the next
// set makes a new one.
// Invariant: I-307
func TestStore_Bounds_RemoveAll(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 2)
	s := c.stores[0]
	for _, tc := range []struct {
		name, value string
		want        error
	}{
		{"Tmdb", "v", ErrName},
		{"../x", "v", ErrName},
		{"tmdb", "", ErrValue},
		{"tmdb", "a\nb", ErrValue},
		{"tmdb", strings.Repeat("x", meta.MaxSecretValue+1), ErrValue},
		{"tmdb", strings.Repeat("x", meta.MaxSecretValue), nil},
		{"s3/b2", "{}", nil},
	} {
		if err := s.Set(ctx, tc.name, tc.value); !errors.Is(err, tc.want) || tc.want == nil && err != nil {
			t.Errorf("Set(%q, %d bytes): %v, want %v", tc.name, len(tc.value), err, tc.want)
		}
	}
	old := c.fsms[0].SecretKeyID()
	if err := c.stores[1].Remove(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	if len(c.fsms[0].Secrets()) != 0 || c.fsms[0].SecretKeyID() != (meta.KeyID{}) {
		t.Fatal("remove all left secrets or the key ID")
	}
	if err := s.Set(ctx, "tmdb", "fresh"); err != nil {
		t.Fatal(err)
	}
	if id := c.fsms[1].SecretKeyID(); id == (meta.KeyID{}) || id == old {
		t.Error("the set after remove all did not make a new key")
	}
	if v, err := c.stores[1].Get(ctx, "tmdb"); v != "fresh" || err != nil {
		t.Errorf("the other node: %q %v", v, err)
	}
}
