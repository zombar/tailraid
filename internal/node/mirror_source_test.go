package node

import (
	"context"
	"os"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// The gateways read a mirror from its disk when it is open on this host; a mirror of this
// host whose disk is detached is offline at once (no RPC to itself); a mirror on another
// host is online while that host is live (docs/16 §6).
//
// Invariant: I-185
func TestMirrorSource_LocalDetachedRemote(t *testing.T) {
	r := newMirrorRig(t, pol())
	r.write(meta.RootIno, "doc", []byte("bytes on the mirror"))
	r.s.tick(context.Background())
	r.drain()
	sn := r.f.Snapshots()[0]
	hostLive := false
	src := mirrorSource{local: r.s, st: r.f, live: func(meta.NodeIdx) bool { return hostLive }}

	if !src.Online(r.mirror) {
		t.Fatal("an open local mirror is offline")
	}
	tr, err := src.Tree(context.Background(), r.mirror, sn.Root)
	if err != nil || len(tr.Entries) != 1 || tr.Entries[0].Name != "doc" {
		t.Fatalf("local tree: %+v %v", tr, err)
	}
	if b, err := src.Object(context.Background(), r.mirror, tr.Entries[0].Chunks[0]); err != nil || len(b) == 0 || string(b) != "bytes on the mirror"[:len(b)] {
		t.Fatalf("local object: %q %v", b, err)
	}

	away := r.path + ".away"
	if err := os.Rename(r.path, away); err != nil {
		t.Fatal(err)
	}
	r.s.refresh()
	if src.Online(r.mirror) {
		t.Fatal("a detached local mirror is online")
	}
	if _, err := src.Tree(context.Background(), r.mirror, sn.Root); err == nil {
		t.Fatal("a detached local mirror served a tree")
	}

	other := meta.NodeIdx(r.ok(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "dev-2", Name: "h2", Kind: meta.KindDevice, Addr: "100.64.0.2"}}}).Ino)
	remote := meta.NodeIdx(r.ok(meta.Command{Op: meta.OpNodeUpsert, NodeUpsert: &meta.NodeUpsert{Node: meta.Node{ID: "m-2", Name: "h2/m1", Kind: meta.KindMirror, Host: other}}}).Ino)
	if src.Online(remote) {
		t.Fatal("a mirror on a dead host is online")
	}
	hostLive = true
	if !src.Online(remote) {
		t.Fatal("a mirror on a live host is offline")
	}
	if src.Online(other) {
		t.Fatal("a device row is a mirror")
	}
}
