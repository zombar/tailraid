// Package testharness provides in-process harnesses for the storage stack (docs/09).
package testharness

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/testseed"

	"github.com/hashicorp/raft"
	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/zombar/tailraid/internal/chunk"
	"github.com/zombar/tailraid/internal/clock"
	"github.com/zombar/tailraid/internal/ec"
	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
	"github.com/zombar/tailraid/internal/vfs"
)

// TB is the subset of testing.TB the fabric needs, so gocheck suites can build one too.
type TB interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	TempDir() string
	Cleanup(func())
}

// Fabric is an in-process single-gateway cluster: one real FSM applied through a
// synchronous proposer, N real shard stores reachable through a fault-injectable shard
// client, and a fake liveness view. It is the harness for vfs and gateway tests.
type Fabric struct {
	TB      TB
	FSM     *meta.FSM
	Clk     *clock.Fake
	SelfIdx meta.NodeIdx
	Stores  map[meta.NodeIdx]*chunk.Store
	Cache   *chunk.Cache
	Ov      *vfs.Overlay
	staging string

	mu            sync.Mutex
	idx           uint64
	down          map[meta.NodeIdx]bool
	full          map[meta.NodeIdx]bool
	failPutOnce   map[meta.NodeIdx]int
	failPutWith   map[meta.NodeIdx]error     // every PUT to the node fails with this
	stallPut      map[meta.NodeIdx]*putStall // every PUT to the node blocks until released
	cancelledPuts map[meta.NodeIdx]int       // stalled PUTs whose caller cancelled them
	putCount      int
	gets          map[meta.NodeIdx]int
	proposeErr    error // returned once by Propose as a transport failure
	proposeDown   error // returned by every Propose while set
	stale         bool  // the gateway's health view given to vfs.FS
	proposed      []meta.Op
}

const (
	ChunkSize = uint32(64 << 10) // 64 KiB chunks keep the tests small
)

// NewFabric builds a fabric with the given node count and (k,m) policy.
func NewFabric(t TB, nodes int, k, m uint8) *Fabric {
	t.Helper()
	f := &Fabric{
		TB: t, FSM: meta.NewFSM(), Clk: clock.NewFake(time.Unix(1_700_000_000, 0)), SelfIdx: 1,
		Stores: map[meta.NodeIdx]*chunk.Store{}, down: map[meta.NodeIdx]bool{}, full: map[meta.NodeIdx]bool{},
		failPutOnce: map[meta.NodeIdx]int{}, gets: map[meta.NodeIdx]int{},
	}
	f.Apply(meta.Command{Op: meta.OpSetVolume, SetVolume: &meta.SetVolume{ChunkSize: ChunkSize, K: k, M: m, Now: 1}})
	root := t.TempDir()
	for i := 1; i <= nodes; i++ {
		f.AddNode()
	}
	var err error
	f.Cache, err = chunk.OpenCache(sysfs.OS{}, filepath.Join(root, "cache"), 1<<30, f.Clk)
	if err != nil {
		t.Fatal(err)
	}
	f.staging = filepath.Join(root, "staging")
	f.Ov, err = vfs.OpenOverlay(f.staging, ChunkSize, f.Clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Ov.Close() })
	return f
}

// Staging is the overlay's WAL directory.
func (f *Fabric) Staging() string { return f.staging }

// RestartOverlay closes the overlay and reopens it from its WALs, as a gateway restart
// does; dirty state must survive. Flushers and filesystems built before must be rebuilt.
// Options (caps) apply to the reopened overlay.
func (f *Fabric) RestartOverlay(opts ...vfs.Option) {
	f.TB.Helper()
	if err := f.Ov.Close(); err != nil {
		f.TB.Fatal(err)
	}
	ov, err := vfs.OpenOverlay(f.staging, ChunkSize, f.Clk, opts...)
	if err != nil {
		f.TB.Fatal(err)
	}
	f.Ov = ov
}

// AddNode registers a device node with its own shard store.
func (f *Fabric) AddNode() meta.NodeIdx {
	f.TB.Helper()
	r := f.Apply(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{Name: "n", Kind: meta.KindDevice, Capacity: 1 << 40}}})
	idx := meta.NodeIdx(r.Ino)
	st, err := chunk.Open(sysfs.OS{}, filepath.Join(f.TB.TempDir(), "shards"), 1<<40)
	if err != nil {
		f.TB.Fatal(err)
	}
	f.Stores[idx] = st
	return idx
}

func (f *Fabric) Apply(c meta.Command) meta.Result {
	f.TB.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyLocked(c)
}

func (f *Fabric) applyLocked(c meta.Command) meta.Result {
	f.idx++
	res := f.FSM.Apply(&raft.Log{Index: f.idx, Type: raft.LogCommand, Data: meta.EncodeCommand(c)}).(meta.Result)
	f.proposed = append(f.proposed, c.Op)
	return res
}

// MustApply applies a command and fails the test on an FSM error.
func (f *Fabric) MustApply(c meta.Command) meta.Result {
	f.TB.Helper()
	r := f.Apply(c)
	if r.Err != nil {
		f.TB.Fatalf("op %d: %v", c.Op, r.Err)
	}
	return r
}

// --- meta.Proposer ---

func (f *Fabric) Propose(_ context.Context, c meta.Command) (meta.Result, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.proposeErr != nil {
		err := f.proposeErr
		f.proposeErr = nil
		return meta.Result{}, 0, err
	}
	if f.proposeDown != nil {
		return meta.Result{}, 0, f.proposeDown
	}
	res := f.applyLocked(c)
	return res, f.idx, nil
}

// --- meta.Peers ---

func (f *Fabric) Self() meta.NodeIdx { return f.SelfIdx }

func (f *Fabric) Live() []meta.NodeIdx {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []meta.NodeIdx
	for idx := range f.Stores {
		if !f.down[idx] {
			out = append(out, idx)
		}
	}
	slices.Sort(out)
	return out
}

func (f *Fabric) Rank(nodes []meta.NodeIdx) []meta.NodeIdx {
	out := append([]meta.NodeIdx(nil), nodes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i] == f.SelfIdx {
			return true
		}
		if out[j] == f.SelfIdx {
			return false
		}
		return out[i] < out[j]
	})
	return out
}

// --- meta.Liveness ---

// IsLive reports whether node n is up (not SetDown).
func (f *Fabric) IsLive(n meta.NodeIdx) bool { return !f.IsDown(n) }

// DeadFor is zero for a live node and a day for a down one: past every hold-down.
func (f *Fabric) DeadFor(n meta.NodeIdx) time.Duration {
	if f.IsDown(n) {
		return 24 * time.Hour
	}
	return 0
}

// --- chunk.ShardClient ---

var errNodeDown = errors.New("fabric: node down")

func (f *Fabric) Put(ctx context.Context, node meta.NodeIdx, key chunk.ShardKey, body []byte) error {
	f.mu.Lock()
	f.putCount++
	st, ok := f.Stores[node]
	down, full := f.down[node], f.full[node]
	if f.failPutOnce[node] > 0 {
		f.failPutOnce[node]--
		f.mu.Unlock()
		return errors.New("fabric: injected put failure")
	}
	if err := f.failPutWith[node]; err != nil {
		f.mu.Unlock()
		return err
	}
	stall := f.stallPut[node]
	f.mu.Unlock()
	if stall != nil {
		stall.inflight.Add(1)
		defer stall.inflight.Done()
		stall.enteredOnce.Do(func() { close(stall.entered) })
		select {
		case <-stall.release:
		case <-ctx.Done(): // the caller gave up on this PUT, as the RPC client would
			f.mu.Lock()
			f.cancelledPuts[node]++
			f.mu.Unlock()
			return ctx.Err()
		}
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

func (f *Fabric) Get(_ context.Context, node meta.NodeIdx, key chunk.ShardKey) ([]byte, error) {
	f.mu.Lock()
	f.gets[node]++
	st, ok := f.Stores[node]
	down := f.down[node]
	f.mu.Unlock()
	if !ok || down {
		return nil, errNodeDown
	}
	return st.Get(key)
}

func (f *Fabric) Has(_ context.Context, node meta.NodeIdx, key chunk.ShardKey) (bool, error) {
	f.mu.Lock()
	st, ok := f.Stores[node]
	down := f.down[node]
	f.mu.Unlock()
	if !ok || down {
		return false, errNodeDown
	}
	return st.Has(key), nil
}

func (f *Fabric) Delete(_ context.Context, node meta.NodeIdx, key chunk.ShardKey) error {
	f.mu.Lock()
	st, ok := f.Stores[node]
	f.mu.Unlock()
	if !ok {
		return errNodeDown
	}
	_, err := st.Delete(key)
	return err
}

// SetDown marks a node dead or alive for the shard client and liveness view.
func (f *Fabric) SetDown(n meta.NodeIdx, down bool) {
	f.mu.Lock()
	f.down[n] = down
	f.mu.Unlock()
}

// Puts counts shard PUTs issued so far.
// Gets returns the number of shard GETs issued to node so far.
func (f *Fabric) Gets(node meta.NodeIdx) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[node]
}

func (f *Fabric) Puts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCount
}

// Ops lists the operations applied through the proposer.
func (f *Fabric) Ops() []meta.Op {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]meta.Op(nil), f.proposed...)
}

// --- components under test ---

// Reader returns a reader sharing the fabric cache.
func (f *Fabric) Reader() *vfs.Reader {
	return vfs.NewReader(f.FSM, f, f, f.Cache, &ec.Pool{})
}

// readerNoCache is a reader as another gateway with a cold cache would see the cluster.
func (f *Fabric) ReaderNoCache() *vfs.Reader {
	return vfs.NewReader(f.FSM, f, f, nil, &ec.Pool{})
}

// FS returns a billy filesystem over the fabric.
func (f *Fabric) FS() *vfs.FS {
	fs := vfs.New(f.FSM, f, f.Ov, f.Reader(), f.Clk, 501, 20)
	fs.SetHealth(f.isStale)
	return fs
}

// Flusher returns a flusher with the test chunk policy.
func (f *Fabric) Flusher(opts ...func(*vfs.Config)) *vfs.Flusher {
	cfg := vfs.DefaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return vfs.NewFlusher(f.Ov, f.FSM, f, f, f, f.Reader(), f.Clk, cfg, nil)
}

// --- helpers ---

// Create makes an empty file in the root directory.
func (f *Fabric) Create(name string) meta.Ino {
	f.TB.Helper()
	return f.MustApply(meta.Command{Op: meta.OpCreate, Create: &meta.Create{Parent: meta.RootIno, Name: name, Type: meta.TFile, Mode: 0o644, Now: 1}}).Ino
}

// Inode fetches an inode or fails the test.
func (f *Fabric) Inode(ino meta.Ino) meta.Inode {
	f.TB.Helper()
	in, ok := f.FSM.GetInode(ino)
	if !ok {
		f.TB.Fatalf("inode %d missing", ino)
	}
	return in
}

// shardsOn counts the nodes that hold a shard of h with the given (k,m).
// ShardsOn lists the nodes holding a shard of h with the given (k,m).
func (f *Fabric) ShardsOn(h meta.Hash, k, m uint8) []meta.NodeIdx {
	var out []meta.NodeIdx
	for idx, st := range f.Stores {
		for i := range int(k) + int(m) {
			if st.Has(chunk.Key(h, k, m, i)) {
				out = append(out, idx)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// corruptShard flips a byte inside the shard file for key on node.
// CorruptShard flips a byte inside the shard file for key on node.
func (f *Fabric) CorruptShard(node meta.NodeIdx, key chunk.ShardKey) {
	f.TB.Helper()
	var path string
	_ = f.Stores[node].Walk(func(k chunk.ShardKey, _ int64, _ time.Time) error {
		if k == key {
			path = f.Stores[node].Path(k)
		}
		return nil
	})
	if path == "" {
		f.TB.Fatalf("shard %s not on node %d", key, node)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		f.TB.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil {
		f.TB.Fatal(err)
	}
}

// readFile reads the whole file as a client would: overlay over committed chunks.
// ReadFile reads the whole file as a client would: overlay over committed chunks.
func (f *Fabric) ReadFile(ino meta.Ino) []byte {
	f.TB.Helper()
	if _, ok := f.FSM.GetInode(ino); !ok {
		f.TB.Fatalf("inode %d missing", ino)
	}
	ctx := context.Background()
	rd := f.Reader()
	in := f.Inode(ino)
	size := in.Size
	if sz, ok := f.Ov.Size(ino, in.Size); ok {
		size = sz
	}
	out := make([]byte, size)
	if _, ok := f.Ov.Size(ino, in.Size); ok {
		n, err := f.Ov.Read(ino, in.Size, 0, out, rd.BaseReader(ctx, in))
		if err != nil && !errors.Is(err, io.EOF) {
			f.TB.Fatal(err)
		}
		return out[:n]
	}
	n, err := rd.ReadAt(ctx, in, out, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		f.TB.Fatal(err)
	}
	return out[:n]
}

// Eventually polls cond until it holds or the deadline passes, without time.Sleep.
func Eventually(t *testing.T, cond func() bool, msg string) {
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

// IsDown reports whether a node is marked dead.
func (f *Fabric) IsDown(n meta.NodeIdx) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.down[n]
}

// SetFull makes a node answer ErrFull to PUTs.
func (f *Fabric) SetFull(n meta.NodeIdx, full bool) {
	f.mu.Lock()
	f.full[n] = full
	f.mu.Unlock()
}

// FailPutOnce makes the next count PUTs to node fail.
func (f *Fabric) FailPutOnce(n meta.NodeIdx, count int) {
	f.mu.Lock()
	f.failPutOnce[n] = count
	f.mu.Unlock()
}

// FailPutWith makes every PUT to node fail with err (nil restores service).
func (f *Fabric) FailPutWith(node meta.NodeIdx, err error) {
	f.mu.Lock()
	if f.failPutWith == nil {
		f.failPutWith = map[meta.NodeIdx]error{}
	}
	f.failPutWith[node] = err
	f.mu.Unlock()
}

type putStall struct {
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
	inflight    sync.WaitGroup // PUTs that entered the stall and have not finished
}

// CancelledPuts is the number of stalled PUTs to node that their caller cancelled.
func (f *Fabric) CancelledPuts(node meta.NodeIdx) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelledPuts[node]
}

// StallPut makes every PUT to node block until release is called or the caller cancels;
// entered closes when the first one is waiting. release returns only once every stalled
// PUT has run to completion, so a test's temp dir is not written to after the test ends.
func (f *Fabric) StallPut(node meta.NodeIdx) (entered <-chan struct{}, release func()) {
	s := &putStall{entered: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	if f.stallPut == nil {
		f.stallPut = map[meta.NodeIdx]*putStall{}
		f.cancelledPuts = map[meta.NodeIdx]int{}
	}
	f.stallPut[node] = s
	f.mu.Unlock()
	var once sync.Once
	return s.entered, func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.stallPut, node)
			f.mu.Unlock()
			close(s.release)
		})
		s.inflight.Wait()
	}
}

// ProposeErrOnce makes the next Propose fail as a transport error.
func (f *Fabric) ProposeErrOnce(err error) {
	f.mu.Lock()
	f.proposeErr = err
	f.mu.Unlock()
}

// ProposeDown makes every Propose fail with err (nil restores service).
func (f *Fabric) ProposeDown(err error) {
	f.mu.Lock()
	f.proposeDown = err
	f.mu.Unlock()
}

// SetStale sets the health view the filesystem consults after a failed proposal.
func (f *Fabric) SetStale(stale bool) {
	f.mu.Lock()
	f.stale = stale
	f.mu.Unlock()
}

func (f *Fabric) isStale() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stale
}

// InodeOf resolves a path through the filesystem's fileid.
func (f *Fabric) InodeOf(fs *vfs.FS, name string) meta.Ino {
	f.TB.Helper()
	st, err := fs.Stat(name)
	if err != nil {
		f.TB.Fatal(err)
	}
	return meta.Ino(st.Sys().(*nfsfile.FileInfo).Fileid)
}

// Pattern returns n deterministic bytes derived from seed.
func Pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// FirstDiff returns the first index where a and b differ, or len(a).
func FirstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// Seed returns a seeded RNG, logging the seed so a failure can be replayed with -seed.
func Seed(t *testing.T) *rand.Rand { return testseed.Seed(t) }
