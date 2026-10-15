package node

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/zombar/tailraid/internal/clock"

	"github.com/zombar/tailraid/internal/config"
	"github.com/zombar/tailraid/internal/mesh"
	"github.com/zombar/tailraid/internal/meta"
)

func key(b byte) meta.MeshKey {
	var k meta.MeshKey
	for i := range k {
		k[i] = b + byte(i)
	}
	return k
}

// The device's peers are exactly the device rows with a key that are not removed, with
// their candidates, plus the pending rows with none; self, disks, buckets, keyless and
// removed rows are left out, and the order is by key.
// Invariant: I-317
func TestMeshPeers_FromRowsAndPending(t *testing.T) {
	self := key(1)
	rows := []meta.Node{
		{Idx: 1, Kind: meta.KindDevice, State: meta.Active, Addr: "100.70.3.1", Key: self},
		{Idx: 2, Kind: meta.KindDevice, State: meta.Active, Addr: "100.70.3.2", Key: key(2), Endpoints: []string{"192.168.1.2:20483", "vps.example.com:20483", "[2001:db8::2]:20483"}},
		{Idx: 3, Kind: meta.KindDevice, State: meta.Removed, Addr: "100.70.3.3", Key: key(3)},
		{Idx: 4, Kind: meta.KindDevice, State: meta.Active, Addr: "100.70.3.4"}, // keyless: not a peer
		{Idx: 5, Kind: meta.KindDisk, Host: 2, Key: key(5)},
		{Idx: 6, Kind: meta.KindS3, Key: key(6)},
		{Idx: 7, Kind: meta.KindDevice, State: meta.Draining, Addr: "100.70.3.7", Key: key(7)},
		{Idx: 8, Kind: meta.KindDevice, State: meta.Active, Addr: "not an address", Key: key(8)},
		{Idx: 9, Kind: meta.KindDevice, State: meta.Active, Addr: "100.70.3.9", Key: key(2)}, // a duplicate key keeps the first
		{Idx: 3, Kind: meta.KindDevice, State: meta.Active, Addr: "100.70.3.3", Key: key(3)}, // a stale seed copy of the removed row: still no peer (I-318)
	}
	pending := []meta.Pending{{Key: key(9), Addr: "100.70.3.9"}, {Key: self, Addr: "100.70.3.1"}, {Key: key(2), Addr: "100.70.3.2"}}
	got := meshPeers(rows, pending, mesh.Key(self))
	var keys []byte
	for _, p := range got {
		keys = append(keys, p.Key[0])
	}
	if !slices.Equal(keys, []byte{2, 7, 9}) {
		t.Fatalf("peers %v, want keys 2, 7 and 9", keys)
	}
	if p := got[0]; p.Idx != 2 || p.Addr.String() != "100.70.3.2" || len(p.Candidates) != 2 || p.Candidates[0].String() != "192.168.1.2:20483" {
		t.Fatalf("peer 2: %+v (a name is no candidate here)", p)
	}
	if p := got[2]; p.Idx < 1<<31 || len(p.Candidates) != 0 || p.Addr.String() != "100.70.3.9" {
		t.Fatalf("the pending peer: %+v", p)
	}
	if meshPeers(nil, nil, mesh.Key(self)) != nil {
		t.Fatal("no rows, no peers")
	}
}

// mesh.endpoint reaches the peers as addresses: a name is resolved when the candidates
// are published (a peer never resolves a name, parseCandidates drops one), an address
// passes as it is, and a name that does not resolve contributes nothing.
// Invariant: I-322
func TestCandidates_ConfiguredNameResolvedAtPublish(t *testing.T) {
	ctx := context.Background()
	got := configuredEndpoints(ctx, "localhost:20483")
	if len(got) == 0 || !slices.Contains(got, "127.0.0.1:20483") && !slices.Contains(got, "[::1]:20483") {
		t.Fatalf("localhost resolved to %v", got)
	}
	for _, e := range got {
		if _, err := netip.ParseAddrPort(e); err != nil {
			t.Fatalf("a published endpoint that is not an address: %q", e)
		}
	}
	if got := configuredEndpoints(ctx, "203.0.113.5:20483"); !slices.Equal(got, []string{"203.0.113.5:20483"}) {
		t.Fatalf("an address: %v", got)
	}
	if got := configuredEndpoints(ctx, "no-such-host.invalid:20483"); len(got) != 0 {
		t.Fatalf("an unresolvable name published %v", got)
	}
	if got := configuredEndpoints(ctx, ""); got != nil {
		t.Fatalf("nothing configured: %v", got)
	}
	if cands := parseCandidates([]string{"vps.example.com:20483", "203.0.113.5:20483"}); len(cands) != 1 {
		t.Fatalf("a peer resolved a name: %v", cands)
	}
}

// Candidates: the configured endpoint first, then the host's addresses, then what
// members see; deduplicated, at most meta.MaxEndpoints, no loopback.
// Invariant: I-322
func TestCandidates_OrderDedupCap(t *testing.T) {
	seen := []string{"203.0.113.9:20483", "203.0.113.9:20483", "198.51.100.1:1", "198.51.100.2:1", "198.51.100.3:1", "198.51.100.4:1", "198.51.100.5:1", "198.51.100.6:1", "198.51.100.7:1", "198.51.100.8:1"}
	got := candidates(20483, []string{"203.0.113.7:20483"}, "203.0.113.9:20483", seen)
	if got[0] != "203.0.113.7:20483" || got[1] != "203.0.113.9:20483" {
		t.Fatalf("the configured endpoint comes first, the gateway's mapping second (docs/20 §7): %v", got)
	}
	if len(got) > meta.MaxEndpoints {
		t.Fatalf("%d candidates, at most %d", len(got), meta.MaxEndpoints)
	}
	dup := map[string]bool{}
	for _, e := range got {
		if dup[e] {
			t.Fatalf("duplicate %s in %v", e, got)
		}
		dup[e] = true
		if strings.HasPrefix(e, "127.") || strings.HasPrefix(e, "[::1]") {
			t.Fatalf("loopback candidate %s", e)
		}
	}
	none := candidates(20483, nil, "", nil)
	for _, e := range none {
		if _, err := netip.ParseAddrPort(e); err != nil {
			t.Fatalf("a local candidate that is not an address: %s", e)
		}
	}
}

// A state whose device rows carry no key predates the mesh and is refused with the way
// out; a removed keyless row does not count.
// Invariant: I-326
func TestKeylessCheck(t *testing.T) {
	f := meta.NewFSM()
	var index uint64
	apply := func(n meta.Node) {
		t.Helper()
		index++
		if res := f.Apply(&raft.Log{Index: index, Data: meta.MustEncodeCommand(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: n}})}).(meta.Result); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	apply(meta.Node{ID: "abcdefabcdefabcd", Name: "pike", Kind: meta.KindDevice, Addr: "100.70.3.1", Key: key(1)})
	if err := keylessCheck(f); err != nil {
		t.Fatalf("a keyed row: %v", err)
	}
	apply(meta.Node{ID: "0123456789abcdef", Name: "old", Kind: meta.KindDevice, Addr: "100.64.0.9"})
	err := keylessCheck(f)
	if err == nil || !strings.Contains(err.Error(), "old") || !strings.Contains(err.Error(), "tailraid init") {
		t.Fatalf("a keyless row: %v", err)
	}
}

// The mesh file and the config give the CLI its node's address.
// Invariant: I-325
func TestLocalAddr_MeshFileThenConfig(t *testing.T) {
	home := t.TempDir()
	cfg := config.Default()
	cfg.Path = filepath.Join(home, "config.yaml")
	cfg.Listen.RPC = 20481
	if _, err := LocalAddr(cfg); err == nil {
		t.Fatal("no address anywhere must fail")
	}
	cfg.Mesh.Address = "100.70.3.2/24"
	if got, err := LocalAddr(cfg); err != nil || got != "100.70.3.2:20481" {
		t.Fatalf("from the config: %q %v", got, err)
	}
	if err := writeMeshFile(home, netip.MustParseAddr("100.70.3.5"), "utun4", 20483, 30000); err != nil {
		t.Fatal(err)
	}
	if got, err := LocalAddr(cfg); err != nil || got != "100.70.3.5:30000" {
		t.Fatalf("from the mesh file: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(home, MeshFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LocalAddr(cfg); err != nil || got != "100.70.3.2:20481" {
		t.Fatalf("a corrupt mesh file falls back to the config: %q %v", got, err)
	}
	fake := filepath.Join(home, "fake.json")
	if err := os.WriteFile(fake, []byte(`{"addr":"127.0.0.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Dev.Enabled, cfg.Dev.MeshFake = true, fake
	if got, err := LocalAddr(cfg); err != nil || got != "127.0.0.1:20481" {
		t.Fatalf("the fake: %q %v", got, err)
	}
}

// A member reports a caller's source only for a direct path: a relayed peer's endpoint
// is the relay's and a fanning-out peer has none, so neither becomes a candidate the
// caller publishes as its own.
// Invariant: I-322
func TestSeenFrom_DirectOnly_Table(t *testing.T) {
	k := mesh.Key{1}
	ep := netip.MustParseAddrPort("203.0.113.9:20483")
	for _, tc := range []struct {
		name  string
		peers []mesh.PeerStatus
		want  string
	}{
		{"direct", []mesh.PeerStatus{{Key: k, Endpoint: ep}}, ep.String()},
		{"relayed", []mesh.PeerStatus{{Key: k, Endpoint: ep, Relay: true}}, ""},
		{"fanning out", []mesh.PeerStatus{{Key: k, Fanout: true}}, ""},
		{"no endpoint", []mesh.PeerStatus{{Key: k}}, ""},
		{"another peer", []mesh.PeerStatus{{Key: mesh.Key{2}, Endpoint: ep}}, ""},
	} {
		if got := directEndpoint(tc.peers, k); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// serve listens only once the interface's address takes a bind: a probe that fails a
// few times (Windows holds a fresh address tentative for a moment) is retried every
// 200 ms, one that never succeeds fails after the wait with the last error, and a
// cancelled context ends the wait.
// Invariant: I-324
func TestWaitBindable_Table(t *testing.T) {
	addr := netip.MustParseAddr("100.64.0.2")
	for _, tc := range []struct {
		name    string
		failFor int // probes that fail before one succeeds; -1: never succeeds
		cancel  bool
		wantErr bool
		probes  int
	}{
		{"first try", 0, false, false, 1},
		{"after three", 3, false, false, 4},
		{"never", -1, false, true, 76}, // 15 s of 200 ms steps plus the first
		{"cancelled", -1, true, true, 1},
	} {
		clk := clock.NewFake(time.Unix(1_800_000_000, 0))
		ctx, cancel := context.WithCancel(context.Background())
		n := 0
		probe := func(a netip.Addr) error {
			if a != addr {
				t.Fatalf("%s: probed %s", tc.name, a)
			}
			n++
			if tc.failFor >= 0 && n > tc.failFor {
				return nil
			}
			if tc.cancel {
				cancel()
			}
			return errors.New("bind: the requested address is not valid in its context")
		}
		done := make(chan error, 1)
		go func() { done <- waitBindable(ctx, addr, probe, clk, 15*time.Second) }()
		// the fake clock advances each pending timer as it appears
		for {
			select {
			case err := <-done:
				if (err != nil) != tc.wantErr || n != tc.probes {
					t.Errorf("%s: err %v, %d probes, want err %v and %d", tc.name, err, n, tc.wantErr, tc.probes)
				}
				cancel()
				goto next
			default:
			}
			if clk.Pending() > 0 {
				clk.Advance(200 * time.Millisecond)
			} else {
				time.Sleep(time.Millisecond) //nolint:forbidigo // yielding to the goroutine between timers
			}
		}
	next:
	}
}
