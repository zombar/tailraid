package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/meta"
)

// lostChunk writes name with content and makes its one chunk unreadable, returning the
// chunk's bytes and hash so a test can make it readable again.
func (r *mirrorRig) lostChunk(name, content string) (meta.Hash, []byte) {
	r.t.Helper()
	r.write(meta.RootIno, name, []byte(content))
	for h, b := range r.chunks {
		if string(b) == content[:min(len(content), tcs)] {
			delete(r.chunks, h)
			return h, b
		}
	}
	r.t.Fatal("the chunk was not written")
	return meta.Hash{}, nil
}

func (r *mirrorRig) partial(name string) meta.SnapID {
	r.t.Helper()
	id := r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: name, Now: r.clk.Now().UnixNano()}}).SnapID
	r.drain()
	if row, _ := r.f.SnapshotRow(id); row.State != meta.SnapIncomplete || row.Missing != 1 {
		r.t.Fatalf("snapshot %s: %+v, want incomplete with one missing", name, row)
	}
	return id
}

// The host heals an incomplete snapshot on its tick once the chunk can be read: the
// object is written, the manifest lists nothing and the row is complete; while the chunk
// stays unreadable it proposes no heal and leaves the manifest (docs/16 §5).
// Invariant: I-391
func TestMirrors_HealsIncompleteSnapshot(t *testing.T) {
	r := newMirrorRig(t, pol())
	h, b := r.lostChunk("lost", "this chunk cannot be read")
	id := r.partial("partial")
	n := r.countProposals()
	r.s.tick(context.Background())
	r.drain()
	if n[meta.OpSnapshotHeal] != 0 {
		t.Fatalf("%d heals proposed while the chunk is unreadable", n[meta.OpSnapshotHeal])
	}
	r.chunks[h] = b // its holders are back
	r.clk.Advance(time.Minute)
	r.s.tick(context.Background())
	r.drain()
	if n[meta.OpSnapshotHeal] == 0 {
		t.Fatal("no heal proposed once the chunk could be read")
	}
	for _, sn := range r.f.Snapshots() {
		if sn.State != meta.SnapComplete || sn.Missing != 0 {
			t.Fatalf("after the heal: %+v", sn)
		}
	}
	for _, man := range r.manifests() {
		if len(man.Missing) != 0 {
			t.Fatalf("manifest %d still lists %x", man.ID, man.Missing)
		}
	}
	if got, err := r.read("partial", "/lost"); err != nil || string(got) != "this chunk cannot be read" {
		t.Fatalf("the healed snapshot reads %q, %v", got, err)
	}
	if row, _ := r.f.SnapshotRow(id); row.Name != "partial" {
		t.Fatalf("the row changed beyond its count: %+v", row)
	}
}

// While a live member reads codec 14 or older the host proposes no heal and leaves the
// manifest as it is; once every member reads 15 the next tick heals (docs/16 §5).
// Invariant: I-391
func TestMirrors_NoHealBeforeCodec15(t *testing.T) {
	r := newMirrorRig(t, pol())
	h, b := r.lostChunk("lost", "this chunk cannot be read")
	r.partial("partial")
	r.chunks[h] = b
	codec := uint8(meta.CodecHeal - 1)
	r.s.minCodec = func() uint8 { return codec }
	n := r.countProposals()
	r.s.tick(context.Background())
	r.drain()
	if n[meta.OpSnapshotHeal] != 0 {
		t.Fatalf("a heal proposed while a member reads codec %d", codec)
	}
	for _, man := range r.manifests() {
		if man.Name == "partial" && len(man.Missing) != 1 {
			t.Fatalf("the manifest was healed before the row could be: %+v", man)
		}
	}
	codec = meta.CodecHeal
	r.clk.Advance(time.Minute)
	r.s.tick(context.Background())
	if n[meta.OpSnapshotHeal] == 0 {
		t.Fatal("no heal once every member reads codec 15")
	}
}

// A mirror warns for an incomplete snapshot only while it is the newest finished one: a
// later complete snapshot ends the warning, though the older one stays incomplete (its
// chunk is gone from the volume and can never be had) (docs/16 §5).
// Invariant: I-392
func TestMirrors_WarnsOnlyForNewestIncomplete(t *testing.T) {
	r := newMirrorRig(t, pol())
	warns := func() string {
		r.s.refresh()
		_, w := r.s.report(r.clk.Now(), r.f.Snapshots())
		return strings.Join(w, "\n")
	}
	r.write(meta.RootIno, "a", []byte("bytes"))
	r.lostChunk("lost", "this chunk cannot be read")
	old := r.partial("partial")
	if w := warns(); !strings.Contains(w, "snapshot partial is missing 1 chunks") {
		t.Fatalf("the newest snapshot is incomplete: %q", w)
	}
	r.ok(meta.Command{Op: meta.OpUnlink, Unlink: &meta.Unlink{Parent: meta.RootIno, Name: "lost", Now: 4}})
	r.clk.Advance(time.Minute)
	r.ok(meta.Command{Op: meta.OpSnapshotBegin, SnapshotBegin: &meta.SnapshotBegin{Mirror: r.mirror, Name: "later", Now: r.clk.Now().UnixNano()}})
	r.drain()
	if w := warns(); strings.Contains(w, "missing") {
		t.Fatalf("a complete snapshot came since, yet the mirror warns: %q", w)
	}
	if row, _ := r.f.SnapshotRow(old); row.State != meta.SnapIncomplete {
		t.Fatalf("the older snapshot: %+v, want still incomplete", row)
	}
}
