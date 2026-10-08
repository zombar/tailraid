package dev

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		f.Apply(&raft.Log{Index: idx, Type: raft.LogCommand, Data: meta.EncodeCommand(c)})
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
	srv := httptest.NewServer(handler(a, store, nil, func(code int) { exited <- code }))
	defer srv.Close()
	post := func(path string) int {
		resp, err := http.Post(srv.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
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
