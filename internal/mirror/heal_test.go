package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// Heal takes a chunk off an incomplete manifest's missing list once the mirror holds it:
// one a later snapshot wrote as an object leaves without a read, one the pool reads now
// is verified and written, and one still unreadable or whose bytes do not match its hash
// stays; the manifest is rewritten in place with what remains (its ID, name and root
// kept), and nothing is rewritten when nothing could be healed (docs/16 §5).
// Invariant: I-391
func TestMirror_Heal_FromObjectsAndPool(t *testing.T) {
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	chunk := func(b byte) []byte { return bytes.Repeat([]byte{b}, testCS) }
	hash := func(b []byte) meta.Hash { return meta.Hash(sha256.Sum256(b)) }
	later, pool, gone, liar := chunk(1), chunk(2), chunk(3), chunk(4)
	v.put(1, "f", 10, slices.Concat(chunk(9), later, pool, gone, liar))
	for _, c := range [][]byte{later, pool, gone, liar} {
		v.unreadable[hash(c)] = true
	}
	first := snap(t, m, v, 1)
	if len(first.Missing) != 4 {
		t.Fatalf("first manifest lists %d missing, want 4", len(first.Missing))
	}
	// a later snapshot wrote one of them: its holders were back for that sync
	v.unreadable[hash(later)] = false
	v.put(1, "g", 11, later)
	if s2 := snap(t, m, v, 2); len(s2.Missing) != 3 {
		t.Fatalf("second manifest lists %d missing", len(s2.Missing))
	}
	reads := map[meta.Hash]int{}
	read := func(_ context.Context, h meta.Hash) ([]byte, error) {
		reads[h]++
		switch h {
		case hash(pool):
			return pool, nil
		case hash(liar):
			return chunk(5), nil // not the bytes the hash names
		}
		return nil, errNoChunk
	}
	left, err := m.Heal(context.Background(), 1, read)
	if err != nil || left != 2 {
		t.Fatalf("heal: %d left, %v; want 2", left, err)
	}
	if reads[hash(later)] != 0 {
		t.Error("a chunk the mirror already held was read from the pool")
	}
	if !m.HasObject(hash(pool)) || m.HasObject(hash(liar)) || m.HasObject(hash(gone)) {
		t.Fatal("the objects after the heal are wrong")
	}
	mans, err := m.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	got := mans[0]
	if got.ID != 1 || got.Name != first.Name || got.Root != first.Root || got.Files != first.Files {
		t.Fatalf("the healed manifest changed more than its list: %+v", got)
	}
	want := []HexHash{HexHash(hash(gone)), HexHash(hash(liar))}
	slices.SortFunc(want, func(a, b HexHash) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(got.Missing, want) {
		t.Fatalf("missing after the heal %x, want %x", got.Missing, want)
	}
	complete(t, m)
	// nothing more can be had: the manifest is left as it is
	before := mustRead(t, m, 1)
	if left, err := m.Heal(context.Background(), 1, read); err != nil || left != 2 {
		t.Fatalf("second heal: %d left, %v", left, err)
	}
	if !bytes.Equal(mustRead(t, m, 1), before) {
		t.Fatal("a heal that healed nothing rewrote the manifest")
	}
	// the second snapshot shares the chunk the first heal wrote: it leaves without a read
	before2 := reads[hash(pool)]
	if left, err := m.Heal(context.Background(), 2, read); err != nil || left != 2 || reads[hash(pool)] != before2 {
		t.Fatalf("heal of the second: %d left, %v, %d pool reads", left, err, reads[hash(pool)]-before2)
	}
}

var errNoChunk = errors.New("fewer than k live slots")

func mustRead(t *testing.T, m *Mirror, id uint32) []byte {
	t.Helper()
	b, err := readAll(m.fsys, m.manifestPath(id))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
