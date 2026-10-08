package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/gurgeh/tailraid/internal/chunk"
	"github.com/gurgeh/tailraid/internal/clock"
	"github.com/gurgeh/tailraid/internal/ec"
	"github.com/gurgeh/tailraid/internal/meta"
	"github.com/gurgeh/tailraid/internal/sysfs"
)

// fabric is an in-process single-gateway cluster: one real FSM applied through a
// synchronous proposer, N real shard stores reachable through a fault-injectable shard
// client, and a fake liveness view. It is the harness for Reader and Flusher tests.
// tb is the subset of testing.TB the fabric needs, so gocheck suites can build one too.
type tb interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	TempDir() string
	Cleanup(func())
}

type fabric struct {
	t      tb
	fsm    *meta.FSM
	clk    *clock.Fake
	self   meta.NodeIdx
	stores map[meta.NodeIdx]*chunk.Store
	cache  *chunk.Cache
	ov     *Overlay

	mu          sync.Mutex
	idx         uint64
	down        map[meta.NodeIdx]bool
	full        map[meta.NodeIdx]bool
	failPutOnce map[meta.NodeIdx]int
	putCount    int
	proposeErr  error // returned once by Propose as a transport failure
	proposed    []meta.Op
	blockPut    map[meta.NodeIdx]chan struct{} // PUT blocks until the channel is closed
}

const (
	fabCS    = uint32(64 << 10) // 64 KiB chunks keep the tests small
	fabSmall = uint32(4 << 10)  // chunks ≤ 4 KiB use k=1
)

func newFabric(t tb, nodes int, k, m uint8) *fabric {
	t.Helper()
	f := &fabric{
		t: t, fsm: meta.NewFSM(), clk: clock.NewFake(time.Unix(1_700_000_000, 0)), self: 1,
		stores: map[meta.NodeIdx]*chunk.Store{}, down: map[meta.NodeIdx]bool{}, full: map[meta.NodeIdx]bool{},
		failPutOnce: map[meta.NodeIdx]int{}, blockPut: map[meta.NodeIdx]chan struct{}{},
	}
	f.apply(meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{ChunkSize: fabCS, K: k, M: m, Now: 1}})
	root := t.TempDir()
	for i := 1; i <= nodes; i++ {
		f.addNode()
	}
	var err error
	f.cache, err = chunk.OpenCache(sysfs.OS{}, filepath.Join(root, "cache"), 1<<30, f.clk)
	if err != nil {
		t.Fatal(err)
	}
	f.ov, err = OpenOverlay(filepath.Join(root, "staging"), fabCS, f.clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.ov.Close() })
	return f
}

func (f *fabric) addNode() meta.NodeIdx {
	f.t.Helper()
	r := f.apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{Name: "n", Kind: meta.KindDevice, Capacity: 1 << 40}}})
	idx := meta.NodeIdx(r.Ino)
	st, err := chunk.Open(sysfs.OS{}, filepath.Join(f.t.TempDir(), "shards"), 1<<40)
	if err != nil {
		f.t.Fatal(err)
	}
	f.stores[idx] = st
	return idx
}

func (f *fabric) apply(c meta.Command) meta.Result {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyLocked(c)
}

func (f *fabric) applyLocked(c meta.Command) meta.Result {
	f.idx++
	res := f.fsm.Apply(&raft.Log{Index: f.idx, Type: raft.LogCommand, Data: meta.EncodeCommand(c)}).(meta.Result)
	f.proposed = append(f.proposed, c.Op)
	return res
}

func (f *fabric) mustApply(c meta.Command) meta.Result {
	f.t.Helper()
	r := f.apply(c)
	if r.Err != nil {
		f.t.Fatalf("op %d: %v", c.Op, r.Err)
	}
	return r
}

// --- meta.Proposer ---

func (f *fabric) Propose(_ context.Context, c meta.Command) (meta.Result, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.proposeErr != nil {
		err := f.proposeErr
		f.proposeErr = nil
		return meta.Result{}, 0, err
	}
	res := f.applyLocked(c)
	return res, f.idx, nil
}

// --- meta.Peers ---

func (f *fabric) Self() meta.NodeIdx { return f.self }

func (f *fabric) Live() []meta.NodeIdx {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []meta.NodeIdx
	for idx := range f.stores {
		if !f.down[idx] {
			out = append(out, idx)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (f *fabric) Rank(nodes []meta.NodeIdx) []meta.NodeIdx {
	out := append([]meta.NodeIdx(nil), nodes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i] == f.self {
			return true
		}
		if out[j] == f.self {
			return false
		}
		return out[i] < out[j]
	})
	return out
}

// --- chunk.ShardClient ---

var errNodeDown = errors.New("fabric: node down")

func (f *fabric) Put(_ context.Context, node meta.NodeIdx, key chunk.ShardKey, body []byte) error {
	f.mu.Lock()
	f.putCount++
	st, ok := f.stores[node]
	down, full := f.down[node], f.full[node]
	if f.failPutOnce[node] > 0 {
		f.failPutOnce[node]--
		f.mu.Unlock()
		return errors.New("fabric: injected put failure")
	}
	block := f.blockPut[node]
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if !ok || down {
		return errNodeDown
	}
	if full {
		return chunk.ErrFull
	}
	_, err := st.Put(key, body)
	return err
}

func (f *fabric) Get(_ context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	f.mu.Lock()
	st, ok := f.stores[node]
	down := f.down[node]
	f.mu.Unlock()
	if !ok || down {
		return nil, errNodeDown
	}
	return st.Get(key)
}

func (f *fabric) Delete(_ context.Context, node meta.NodeIdx, key chunk.ShardKey) error {
	f.mu.Lock()
	st, ok := f.stores[node]
	f.mu.Unlock()
	if !ok {
		return errNodeDown
	}
	_, err := st.Delete(key)
	return err
}

func (f *fabric) setDown(n meta.NodeIdx, down bool) {
	f.mu.Lock()
	f.down[n] = down
	f.mu.Unlock()
}

func (f *fabric) puts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCount
}

func (f *fabric) ops() []meta.Op {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]meta.Op(nil), f.proposed...)
}

// --- components under test ---

func (f *fabric) reader() *Reader {
	return NewReader(f.fsm, f, f, f.cache, &ec.Pool{})
}

// readerNoCache is a reader as another gateway with a cold cache would see the cluster.
func (f *fabric) readerNoCache() *Reader {
	return NewReader(f.fsm, f, f, nil, &ec.Pool{})
}

// fs returns a billy filesystem over the fabric.
func (f *fabric) fs() *FS { return New(f.fsm, f, f.ov, f.reader(), f.clk, 501, 20) }

func (f *fabric) flusher(opts ...func(*Config)) *Flusher {
	cfg := DefaultConfig()
	cfg.SmallChunk = fabSmall
	for _, o := range opts {
		o(&cfg)
	}
	return NewFlusher(f.ov, f.fsm, f, f, f, f.reader(), f.cache, f.clk, cfg, nil)
}

// --- helpers ---

func (f *fabric) create(name string) meta.Ino {
	f.t.Helper()
	return f.mustApply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: meta.RootIno, Name: name, Type: meta.TFile, Mode: 0o644, Now: 1}}).Ino
}

func (f *fabric) inode(ino meta.Ino) meta.Inode {
	f.t.Helper()
	in, ok := f.fsm.GetInode(ino)
	if !ok {
		f.t.Fatalf("inode %d missing", ino)
	}
	return in
}

// shardsOn counts the nodes that hold a shard of h with the given (k,m).
func (f *fabric) shardsOn(h meta.Hash, k, m uint8) []meta.NodeIdx {
	var out []meta.NodeIdx
	for idx, st := range f.stores {
		for i := 0; i < int(k)+int(m); i++ {
			if st.Has(chunk.Key(h, k, m, i)) {
				out = append(out, idx)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// corruptShard flips a byte inside the shard file for key on node.
func (f *fabric) corruptShard(node meta.NodeIdx, key chunk.ShardKey) {
	f.t.Helper()
	var path string
	_ = f.stores[node].Walk(func(k chunk.ShardKey, _ int64, _ time.Time) error {
		if k == key {
			path = f.stores[node].Path(k)
		}
		return nil
	})
	if path == "" {
		f.t.Fatalf("shard %s not on node %d", key, node)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// readFile reads the whole file as a client would: overlay over committed chunks.
func (f *fabric) readFile(ino meta.Ino) []byte {
	f.t.Helper()
	if _, ok := f.fsm.GetInode(ino); !ok {
		f.t.Fatalf("inode %d missing", ino)
	}
	ctx := context.Background()
	rd := f.reader()
	in := f.inode(ino)
	size := in.Size
	if sz, ok := f.ov.Size(ino); ok {
		size = sz
	}
	out := make([]byte, size)
	base := func(off uint64, p []byte) error {
		_, err := rd.ReadAt(ctx, in, p, int64(off))
		return err
	}
	if _, ok := f.ov.Size(ino); ok {
		n, err := f.ov.Read(ino, 0, out, base)
		if err != nil && !errors.Is(err, io.EOF) {
			f.t.Fatal(err)
		}
		return out[:n]
	}
	n, err := rd.ReadAt(ctx, in, out, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		f.t.Fatal(err)
	}
	return out[:n]
}

// eventually polls cond until it holds or the deadline passes, without time.Sleep.
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout: %s", msg)
		case <-tick.C:
		}
	}
}
