package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/cluster"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/s3"
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

// The old files move into the store: each entry the store lacks is stored, a name it
// holds keeps the store's value, and a file goes only once every name in it is stored
// and opens; a store that refuses (a member on an older codec) leaves both files serving.
// Invariant: I-308
func TestMigrateLegacy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, refuse := oneNodeStore(t)
	newer := s3.Credentials{AccessKey: "AK", SecretKey: "rotated"}
	v, _ := cluster.EncodeS3Secret(newer)
	if err := s.Set(ctx, "s3/offsite", v); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]s3.Credentials{"offsite": {AccessKey: "AK", SecretKey: "old"}, "second": {AccessKey: "AK2", SecretKey: "two"}, "Upper": {AccessKey: "AK3", SecretKey: "three"}} {
		if err := cluster.SaveS3Key(dir, name, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, secrets.TMDBLegacyFile), []byte("tmdb-key\n"), 0o600); err != nil { // as `media key` wrote it before the store
		t.Fatal(err)
	}
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }

	refuse(meta.ErrCodec)
	if done, err := migrateLegacy(ctx, s, dir); done || !errors.Is(err, meta.ErrCodec) || !exists(cluster.S3KeysFile) || !exists(secrets.TMDBLegacyFile) {
		t.Fatalf("refused: done %v, %v", done, err)
	}
	refuse(nil)
	if done, err := migrateLegacy(ctx, s, dir); done || err != nil {
		t.Fatalf("with a name off the pattern: done %v, %v", done, err)
	}
	if exists(secrets.TMDBLegacyFile) || !exists(cluster.S3KeysFile) {
		t.Fatal("the tmdb file stayed, or the s3 file went with Upper unmoved")
	}
	if k, err := s.Get(ctx, "tmdb"); k != "tmdb-key" || err != nil {
		t.Errorf("tmdb: %q %v", k, err)
	}
	keys := cluster.MergeS3Keys(s.Opened(cluster.S3SecretPrefix), nil)
	if keys["offsite"] != newer || keys["second"].SecretKey != "two" || len(keys) != 2 {
		t.Errorf("stored credentials: %+v", keys)
	}
	// the operator renames the odd entry away: the next pass removes the file
	if err := os.Remove(filepath.Join(dir, cluster.S3KeysFile)); err != nil {
		t.Fatal(err)
	}
	if err := cluster.SaveS3Key(dir, "second", s3.Credentials{AccessKey: "AK2", SecretKey: "two"}); err != nil {
		t.Fatal(err)
	}
	if done, err := migrateLegacy(ctx, s, dir); !done || err != nil || exists(cluster.S3KeysFile) {
		t.Fatalf("every name stored: done %v, %v, file %v", done, err, exists(cluster.S3KeysFile))
	}
	if done, err := migrateLegacy(ctx, s, dir); !done || err != nil {
		t.Fatalf("nothing left: done %v, %v", done, err)
	}
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
