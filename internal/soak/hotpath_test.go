package soak

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/vfs"
)

// readaheadVolume moves the fake gateway's readahead counters on every whole-file read
// the way a conforming gateway would: every chunk but the first window served, one
// prefetch per chunk after the first.
type readaheadVolume struct {
	Volume
	e      *fakeEnv
	chunks uint64
	move   bool
}

func (v readaheadVolume) Hash(p string) ([32]byte, int64, error) {
	if v.move {
		v.e.mu.Lock()
		v.e.gateway.ReadaheadFetched += v.chunks - 1
		v.e.gateway.ReadaheadJoined += v.chunks - 1
		v.e.mu.Unlock()
	}
	return v.Volume.Hash(p)
}

func runHotPath(t *testing.T, gateways int, move bool, prefetching int) ([]PhaseResult, string) {
	t.Helper()
	e := newFakeEnv(gateways)
	e.caps = Caps{MultiGateway: gateways > 1}
	e.gateway = &admin.GatewayReport{Prefetching: prefetching}
	e.fs = readaheadVolume{Volume: e.fs, e: e, chunks: hotpathChunks, move: move}
	var out bytes.Buffer
	r := fakeRun(e, &out, Phase{Name: "17 hot path", Run: phaseHotPath})
	sum, err := r.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sum.Phases, out.String()
}

func checksNamed(ph PhaseResult, name string) (ok, skipped int) {
	for i := range ph.Checks {
		c := &ph.Checks[i]
		if !strings.HasPrefix(c.Name, name) {
			continue
		}
		switch {
		case c.Skip:
			skipped++
		case c.OK != nil && *c.OK:
			ok++
		}
	}
	return ok, skipped
}

// On a conforming gateway the phase passes every check: the content, the readahead
// counters within their bounds and the prefetches in flight within ReadAhead.
// Invariant: I-166
func TestPhaseHotPath_ConformingGatewayPasses(t *testing.T) {
	phases, out := runHotPath(t, 2, true, vfs.ReadAhead)
	if len(phases) != 1 || phases[0].Failed != 0 {
		t.Fatalf("phase: %+v\n%s", phases, out)
	}
	if ok, _ := checksNamed(phases[0], "readahead served"); ok != 1 {
		t.Fatalf("readahead check not passed:\n%s", out)
	}
	if !strings.Contains(out, "cold read") {
		t.Fatalf("no cold-read event:\n%s", out)
	}
}

// A gateway whose counters do not move, or that reports more prefetches in flight than
// ReadAhead, fails the readahead checks; the content check still passes.
// Invariant: I-166
func TestPhaseHotPath_NonConformingGatewayFails(t *testing.T) {
	phases, out := runHotPath(t, 2, false, vfs.ReadAhead+1)
	if len(phases) != 1 || phases[0].Failed != 2 {
		t.Fatalf("want the served and in-flight checks failed, got %d:\n%s", phases[0].Failed, out)
	}
	if ok, _ := checksNamed(phases[0], "content hash exact"); ok != 1 {
		t.Fatalf("content check did not pass:\n%s", out)
	}
}

// With one gateway and counters that did not move, the client's own cache may have served
// the read: the readahead checks are skipped (reported, I-113), never passed.
// Invariant: I-166
func TestPhaseHotPath_OneGatewayUnmovedCountersSkip(t *testing.T) {
	phases, out := runHotPath(t, 1, false, 0)
	if len(phases) != 1 || phases[0].Failed != 0 {
		t.Fatalf("phase: %+v\n%s", phases, out)
	}
	if ok, skipped := checksNamed(phases[0], "readahead served"); ok != 0 || skipped != 1 {
		t.Fatalf("readahead check ok %d skipped %d:\n%s", ok, skipped, out)
	}
}
