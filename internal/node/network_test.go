package node

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/raft"

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

// Candidates: the configured endpoint first, then the host's addresses, then what
// members see; deduplicated, at most meta.MaxEndpoints, no loopback.
// Invariant: I-322
func TestCandidates_OrderDedupCap(t *testing.T) {
	seen := []string{"203.0.113.9:20483", "203.0.113.9:20483", "198.51.100.1:1", "198.51.100.2:1", "198.51.100.3:1", "198.51.100.4:1", "198.51.100.5:1", "198.51.100.6:1", "198.51.100.7:1", "198.51.100.8:1"}
	got := candidates(20483, "vps.example.com:20483", seen)
	if got[0] != "vps.example.com:20483" {
		t.Fatalf("the configured endpoint comes first: %v", got)
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
	none := candidates(20483, "", nil)
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
