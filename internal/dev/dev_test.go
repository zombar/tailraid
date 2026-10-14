package dev

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

func TestDev_FSMHashCorruptAndExit(t *testing.T) {
	a, b := meta.NewFSM(), meta.NewFSM()
	apply := func(f *meta.FSM, idx uint64, c meta.Command) {
		f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: meta.MustEncodeCommand(c)})
	}
	for _, f := range []*meta.FSM{a, b} {
		apply(f, 1, meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{ChunkSize: 65536, K: 2, M: 1, Now: 1}})
	}
	ha, _ := Hash(a)
	hb, _ := Hash(b)
	if ha.Hash != hb.Hash || ha.Hash == "" {
		t.Fatalf("identical FSMs hash differently: %v %v", ha, hb)
	}
	apply(b, 2, meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: "d", Mode: 0o755, Now: 2}})
	if hb2, _ := Hash(b); hb2.Hash == hb.Hash {
		t.Fatal("a changed FSM must hash differently")
	}

	store, err := chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "shards"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	key := chunk.Key([32]byte{1}, 2, 1, 0)
	if _, err := store.Put(key, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	exited := make(chan int, 1)
	srv := httptest.NewServer(handler(a, oneDisk{store, t.TempDir()}, nil, nil, func(code int) { exited <- code }))
	defer srv.Close()
	post := func(path string) int {
		resp, err := http.Post(srv.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if resp, err := http.Get(srv.URL + "/dev/goroutines"); err != nil {
		t.Fatal(err)
	} else {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "goroutine ") || !strings.Contains(string(body), "dev.handler") {
			t.Fatalf("goroutines: %d %.200s", resp.StatusCode, body)
		}
	}
	if code := post("/dev/corrupt?key=nope"); code != http.StatusNotFound {
		t.Fatalf("unknown key: %d", code)
	}
	if code := post("/dev/corrupt?key=" + string(key)); code != http.StatusNoContent {
		t.Fatalf("corrupt: %d", code)
	}
	if _, err := store.Get(key); !errors.Is(err, chunk.ErrCorrupt) {
		t.Fatalf("after corrupt: %v", err)
	}
	resp, err := http.Get(srv.URL + "/dev/fsm-hash")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("fsm-hash: %v %v", err, resp)
	}
	resp.Body.Close()
	if code := post("/dev/exit"); code != http.StatusNoContent {
		t.Fatalf("exit: %d", code)
	}
	select {
	case code := <-exited:
		if code != 3 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exit not called")
	}
}

// oneDisk is a host with one open disk, as /dev/* sees it.
type oneDisk struct {
	store *chunk.Store
	dir   string
}

func (d oneDisk) Paths(k chunk.ShardKey) []string {
	if !d.store.Has(k) {
		return nil
	}
	return []string{d.store.Path(k)}
}

func (d oneDisk) Disks() []DiskInfo {
	used, free, total, _ := d.store.Disk()
	return []DiskInfo{{ID: "d1", Name: "h/1", Used: used, Free: free, Total: total}}
}

func (d oneDisk) Fill(_ string, n uint64) error { return Fill(d.dir, n) }

// twoDisks is a host with two open disks, as /dev/* sees it.
type twoDisks struct{ a, b *chunk.Store }

func (d twoDisks) Paths(k chunk.ShardKey) []string {
	var out []string
	for _, s := range []*chunk.Store{d.a, d.b} {
		if s.Has(k) {
			out = append(out, s.Path(k))
		}
	}
	return out
}

func (d twoDisks) Disks() []DiskInfo         { return nil }
func (d twoDisks) Fill(string, uint64) error { return nil }

// /dev/corrupt reaches every copy of the key on the host: a rebalance within the host
// leaves the old copy on its disk until the sweep, and a corruption that hit only that
// orphan left the placed copy intact (the soak's phase 5 on the harness, 2026-10-11).
// Invariant: I-172
func TestHandler_CorruptEveryCopy(t *testing.T) {
	open := func() *chunk.Store {
		s, err := chunk.Open(sysfs.OS{}, filepath.Join(t.TempDir(), "shards"), 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := open(), open()
	key := chunk.Key([32]byte{2}, 2, 1, 0)
	for _, s := range []*chunk.Store{a, b} {
		if _, err := s.Put(key, []byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(handler(meta.NewFSM(), twoDisks{a, b}, nil, nil, func(int) {}))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/dev/corrupt?key="+string(key), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("corrupt: %d", resp.StatusCode)
	}
	for i, s := range []*chunk.Store{a, b} {
		if _, err := s.Get(key); !errors.Is(err, chunk.ErrCorrupt) {
			t.Fatalf("disk %d after corrupt: %v", i, err)
		}
	}
}

// The hash is a pure function of the applied index it is labelled with: taken while
// entries keep applying, every (applied, hash) pair equals the hash a second FSM gives
// at that index after replaying the same log; the index comes from the snapshot, not
// from a read after it.
// Invariant: I-4
func TestHash_LabelledWithItsSnapshotsIndex(t *testing.T) {
	a, b := meta.NewFSM(), meta.NewFSM()
	cmds := []meta.Command{{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{ChunkSize: 65536, K: 2, M: 1, Now: 1}}}
	for i := range 400 {
		cmds = append(cmds, meta.Command{Op: meta.OpMkdir, Mkdir: &meta.Mkdir{Parent: meta.RootIno, Name: fmt.Sprintf("d%d", i), Mode: 0o755, Now: int64(i + 2)}})
	}
	apply := func(f *meta.FSM, idx int) {
		f.Apply(&raft.Log{Index: uint64(idx + 1), Type: raft.LogCommand, Data: meta.MustEncodeCommand(cmds[idx])}) //nolint:gosec // a test index
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range cmds {
			apply(a, i)
		}
	}()
	var seen []FSMHash
	for {
		h, err := Hash(a)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, h)
		select {
		case <-done:
		default:
			continue
		}
		break
	}
	want := map[uint64]string{}
	for i := range cmds {
		apply(b, i)
		h, err := Hash(b)
		if err != nil {
			t.Fatal(err)
		}
		want[h.Applied] = h.Hash
	}
	for _, h := range seen {
		if h.Applied == 0 {
			continue
		}
		if want[h.Applied] != h.Hash {
			t.Fatalf("hash at applied %d differs from the replay's", h.Applied)
		}
	}
	if len(seen) < 2 {
		t.Logf("only %d hashes taken while applying", len(seen))
	}
}
