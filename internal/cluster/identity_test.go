package cluster

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A node's identity files are written atomically and a read error never mints a new
// identity: a data directory whose node_id cannot be read is an error, not a new node.
//
// Invariant: I-96
func TestLoadOrCreateNodeID_ReadErrorIsNotANewIdentity(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateNodeID(dir)
	if err != nil || len(id) != 16 {
		t.Fatalf("first id: %q %v", id, err)
	}
	again, err := LoadOrCreateNodeID(dir)
	if err != nil || again != id {
		t.Fatalf("second read: %q %v, want %q", again, err, id)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".node_id") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "node_id")); string(b) != id+"\n" {
		t.Fatalf("node_id content %q", b)
	}
	// unreadable (a directory in its place): an error, and nothing written beside it
	bad := t.TempDir()
	if err := os.Mkdir(filepath.Join(bad, "node_id"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateNodeID(bad); err == nil {
		t.Fatal("an unreadable node_id produced an identity")
	}
	ents, _ = os.ReadDir(bad)
	if len(ents) != 1 {
		t.Fatalf("an unreadable node_id left %d entries, want the directory alone", len(ents))
	}
	// cluster_id goes through the same path
	if err := WriteClusterID(dir, "00112233445566778899aabbccddeeff"); err != nil {
		t.Fatal(err)
	}
	if cid, err := LoadClusterID(dir); err != nil || cid != [16]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff} {
		t.Fatalf("cluster_id round trip: %x %v", cid, err)
	}
}

// A node that fails to start releases the Raft port and the stores: the next attempt on
// the same data directory and address does not find them held by a dead half-node.
//
// Invariant: I-99
func TestOpenFresh_StartFailureFreesPortAndStores(t *testing.T) {
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	bad := Options{
		DataDir: dir, NodeID: "node-bad-00000000", ClusterID: [16]byte{1}, RaftAddr: addr,
		Timing: &RaftTiming{Heartbeat: time.Nanosecond, Election: time.Nanosecond, LeaderLease: time.Nanosecond, Commit: time.Nanosecond},
	}
	if _, err := OpenFresh(bad); err == nil {
		t.Fatal("raft accepted a nanosecond heartbeat")
	}
	if l, err := net.Listen("tcp", addr); err != nil {
		t.Fatalf("the Raft port is still held after a failed start: %v", err)
	} else {
		l.Close()
	}
	good := bad
	good.Timing = nil
	n, err := Open(good) // the failed attempt initialised the stores; Open takes them over
	if err != nil {
		t.Fatalf("a second start on the same directory and port: %v", err)
	}
	if err := n.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
}
