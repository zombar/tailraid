package node

import (
	"testing"

	"github.com/zombar/tailraid/internal/meta"
)

// A joiner refuses a cluster whose node writes a newer codec than this binary reads;
// an older or absent one (a release before the field) is fine.
//
// Invariant: I-105
func TestCodecCompatible(t *testing.T) {
	for _, c := range []struct {
		leader uint8
		ok     bool
	}{{0, true}, {1, true}, {meta.CodecVersion, true}, {meta.CodecVersion + 1, false}, {255, false}} {
		if err := codecCompatible(c.leader); (err == nil) != c.ok {
			t.Errorf("leader codec %d: err=%v, want ok=%v", c.leader, err, c.ok)
		}
	}
}
