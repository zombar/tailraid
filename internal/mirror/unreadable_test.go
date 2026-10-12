package mirror

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/zombar/tailraid/internal/meta"
	"github.com/zombar/tailraid/internal/sysfs"
)

// Invariant: I-181
// Lifecycle: L-215
func TestMirror_UnreadableChunk_Incomplete_RetriedNext(t *testing.T) {
	m, _ := newMirror(t, sysfs.OS{})
	v := newVolume()
	bad := bytes.Repeat([]byte{8}, testCS)
	v.put(1, "f", 10, append(bytes.Repeat([]byte{9}, testCS), bad...))
	h := meta.Hash(sha256.Sum256(bad))
	v.unreadable[h] = true
	first := snap(t, m, v, 1)
	if len(first.Missing) != 1 || meta.Hash(first.Missing[0]) != h || first.Files != 1 {
		t.Fatalf("first manifest %+v: want the unreadable chunk listed and the rest written", first)
	}
	if m.HasObject(h) {
		t.Fatal("an unreadable chunk has an object")
	}
	complete(t, m)
	// the holders are back: the next sync to this mirror fetches it, nothing else
	v.unreadable = map[meta.Hash]bool{}
	reads := v.reads
	second := snap(t, m, v, 2)
	if len(second.Missing) != 0 || !m.HasObject(h) || v.reads-reads != 1 {
		t.Fatalf("second manifest %+v, %d reads", second, v.reads-reads)
	}
}
