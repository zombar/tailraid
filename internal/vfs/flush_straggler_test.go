package vfs_test

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/vfs"
)

// marks records the nodes the flusher marked suspect.
type marks struct {
	mu    sync.Mutex
	nodes map[meta.NodeIdx]int
}

func (m *marks) mark(n meta.NodeIdx) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nodes == nil {
		m.nodes = map[meta.NodeIdx]int{}
	}
	m.nodes[n]++
}

func (m *marks) count(n meta.NodeIdx) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nodes[n]
}

// Once k acks are in, a PUT still pending at the straggler grace is cut exactly as at
// the deadline: cancelled, its slot re-placed, its node marked suspect, and the flush
// completes within the grace rather than the 10 s deadline.
// Invariant: I-169
// Lifecycle: L-209, L-190
func TestFlusher_StragglerGrace_CutsWaitAfterKAcks(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 7)
	must(t, f.Ov.Write(ino, 0, 0, data))
	h := sha256Of(data)
	cands := meta.Candidates(f.FSM.Nodes(), func(meta.NodeIdx) bool { return true })
	victim := meta.PlaceKM(h, cands, 3, 0)[2]
	entered, release := f.StallPut(victim)
	defer release()
	cfg := vfs.DefaultConfig()
	cfg.PutDeadline, cfg.StragglerMin = 100*time.Second, 2*time.Second
	fl := f.Flusher(func(c *vfs.Config) { *c = cfg })
	var m marks
	fl.Suspect = m.mark
	done := make(chan error, 1)
	go func() { done <- fl.FlushInode(ctx, ino) }()
	<-entered
	testharness.Eventually(t, func() bool { return len(f.ShardsOn(h, 2, 1)) == 2 }, "live PUTs never landed")
	// The clock is advanced in grace-sized steps until the straggler is cut. The grace
	// is four times the p95 of the live PUTs as the fake clock measured them (a step or
	// two of it may land inside a measurement), so the exact value is pinned by the
	// table test; here the wait must end far short of the deadline.
	var err error
	var advanced time.Duration
	testharness.Eventually(t, func() bool {
		if f.CancelledPuts(victim) == 0 {
			f.Clk.Advance(cfg.StragglerMin / 4)
			advanced += cfg.StragglerMin / 4
		}
		select {
		case err = <-done:
			return true
		default:
			return false
		}
	}, "flush past the straggler grace")
	must(t, err)
	if advanced >= cfg.PutDeadline/4 {
		t.Fatalf("the flush needed %s of clock: the deadline, not the grace, ended the wait", advanced)
	}
	if f.CancelledPuts(victim) != 1 {
		t.Fatalf("stalled PUT cancelled %d times, want 1", f.CancelledPuts(victim))
	}
	if m.count(victim) != 1 {
		t.Fatalf("victim marked suspect %d times, want 1", m.count(victim))
	}
	release()
	c, ok := f.FSM.GetChunk(h)
	if !ok {
		t.Fatal("chunk not committed")
	}
	for _, s := range c.Placement {
		if s == 0 || s == victim {
			t.Fatalf("placement %v should be full and avoid the stalled node %d", c.Placement, victim)
		}
	}
	if got := f.ReadFile(ino); !bytes.Equal(got, data) {
		t.Fatalf("content differs at %d", testharness.FirstDiff(got, data))
	}
}

// The grace is four times the recent p95 within [StragglerMin, PutDeadline].
// Invariant: I-169
func TestFlusher_StragglerGrace_Table(t *testing.T) {
	const floor, deadline = 2 * time.Second, 10 * time.Second
	for _, tc := range []struct{ p95, want time.Duration }{
		{0, floor},
		{floor - 1, 4 * (floor - 1)},
		{floor/4 - 1, floor},
		{floor / 4, floor},
		{deadline / 4, deadline},
		{deadline/4 + 1, deadline},
		{deadline, deadline},
	} {
		if got := vfs.StragglerGrace(tc.p95, floor, deadline); got != tc.want {
			t.Errorf("p95 %s: grace %s, want %s", tc.p95, got, tc.want)
		}
	}
}

// The grace never cuts a wait before k acks are in: with one of k holders still
// answering slowly, twice the grace passes and the chunk keeps waiting; when the slow
// holders answer, the chunk commits fully placed.
// Invariant: I-169
func TestFlusher_StragglerGrace_NotBeforeKAcks(t *testing.T) {
	f := testharness.NewFabric(t, 3, 2, 1)
	ino := f.Create("a")
	data := testharness.Pattern(int(testharness.ChunkSize), 9)
	must(t, f.Ov.Write(ino, 0, 0, data))
	h := sha256Of(data)
	cands := meta.Candidates(f.FSM.Nodes(), func(meta.NodeIdx) bool { return true })
	place := meta.PlaceKM(h, cands, 3, 0)
	var entered []<-chan struct{}
	var releases []func()
	for _, v := range place[1:] { // two of three holders answer late: one ack < k meanwhile
		e, release := f.StallPut(v)
		entered = append(entered, e)
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	cfg := vfs.DefaultConfig()
	cfg.PutDeadline, cfg.StragglerMin = 10*time.Second, 2*time.Second
	fl := f.Flusher(func(c *vfs.Config) { *c = cfg })
	done := make(chan error, 1)
	go func() { done <- fl.FlushInode(ctx, ino) }()
	for _, e := range entered {
		<-e
	}
	// twice the grace passes with one ack: nothing ends the wait
	for range 8 {
		f.Clk.Advance(cfg.StragglerMin / 4)
		select {
		case err := <-done:
			t.Fatalf("flush ended (%v) with fewer than k acks", err)
		default:
		}
	}
	for _, release := range releases {
		release()
	}
	must(t, <-done)
	c, ok := f.FSM.GetChunk(h)
	if !ok {
		t.Fatal("chunk not committed")
	}
	for i, s := range c.Placement {
		if s != place[i] {
			t.Fatalf("placement %v, want the original %v: nothing was cut", c.Placement, place)
		}
	}
}

// suspectPeers is the fabric with a gateway-side suspect set: Live() excludes a marked
// node, as the prober does (I-170).
type suspectPeers struct {
	*testharness.Fabric
	marks
}

func (p *suspectPeers) Live() []meta.NodeIdx {
	var out []meta.NodeIdx
	for _, n := range p.Fabric.Live() {
		if p.count(n) == 0 {
			out = append(out, n)
		}
	}
	return out
}

// A node that was cut as a straggler is marked suspect, and the next chunk places around
// it without a PUT to it.
// Invariant: I-170
// Lifecycle: L-210
func TestFlusher_SuspectNodeExcludedFromNextChunk(t *testing.T) {
	f := testharness.NewFabric(t, 4, 2, 1)
	sp := &suspectPeers{Fabric: f}
	cfg := vfs.DefaultConfig()
	cfg.PutDeadline, cfg.StragglerMin = 100*time.Second, 2*time.Second
	fl := vfs.NewFlusher(f.Ov, f.FSM, f, sp, f, f.Reader(), f.Clk, cfg, nil)
	fl.Suspect = sp.mark

	a := f.Create("a")
	dataA := testharness.Pattern(int(testharness.ChunkSize), 10)
	must(t, f.Ov.Write(a, 0, 0, dataA))
	cands := meta.Candidates(f.FSM.Nodes(), func(meta.NodeIdx) bool { return true })
	hA := sha256Of(dataA)
	victim := meta.PlaceKM(hA, cands, 3, 0)[2]
	entered, release := f.StallPut(victim)
	done := make(chan error, 1)
	go func() { done <- fl.FlushInode(ctx, a) }()
	<-entered
	testharness.Eventually(t, func() bool { return len(f.ShardsOn(hA, 2, 1)) == 2 }, "live PUTs never landed")
	var err error
	testharness.Eventually(t, func() bool {
		if f.CancelledPuts(victim) == 0 {
			f.Clk.Advance(cfg.StragglerMin / 4)
		}
		select {
		case err = <-done:
			return true
		default:
			return false
		}
	}, "first flush past the grace")
	must(t, err)
	release()
	if sp.count(victim) == 0 {
		t.Fatal("the straggler was not marked suspect")
	}

	b := f.Create("b")
	dataB := testharness.Pattern(int(testharness.ChunkSize), 11)
	must(t, f.Ov.Write(b, 0, 0, dataB))
	puts := f.Puts()
	must(t, fl.FlushInode(ctx, b))
	c, ok := f.FSM.GetChunk(sha256Of(dataB))
	if !ok {
		t.Fatal("second chunk not committed")
	}
	for _, s := range c.Placement {
		if s == 0 || s == victim {
			t.Fatalf("placement %v should be full and avoid the suspect node %d", c.Placement, victim)
		}
	}
	if f.Puts()-puts != 3 {
		t.Fatalf("%d PUTs for the second chunk, want 3 (none to the suspect node)", f.Puts()-puts)
	}
}
