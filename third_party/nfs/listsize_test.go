package nfs

import (
	"bytes"
	"strings"
	"testing"

	"github.com/willscott/go-nfs/xdr"
)

// The size functions agree byte for byte with what the encoder writes, for names on
// either side of XDR's four-byte padding and at the 255-byte limit, with and without
// attributes and a handle.
// Invariant: I-248
func TestListEntrySizes_MatchTheEncoder(t *testing.T) {
	var fa bytes.Buffer
	if err := xdr.Write(&fa, FileAttribute{}); err != nil {
		t.Fatal(err)
	}
	if fa.Len() != fattr3Size {
		t.Fatalf("an fattr3 encodes to %d bytes, fattr3Size is %d", fa.Len(), fattr3Size)
	}
	for _, n := range []int{0, 1, 3, 4, 5, 254, 255} {
		name := strings.Repeat("a", n)
		var b bytes.Buffer
		_ = xdr.Write(&b, readDirEntity{Name: []byte(name)})
		if got := uint32(b.Len()); got != entry3Size(name) { // the entry's trailing flag stands for the one before it
			t.Errorf("name of %d: entry3 encodes to %d, entry3Size says %d", n, got, entry3Size(name))
		}
		for _, h := range []int{-1, 0, 16, 21, 64} {
			for _, attrs := range []bool{false, true} {
				e := readDirPlusEntity{Name: []byte(name)}
				if attrs {
					e.Attributes = &FileAttribute{}
				}
				if h >= 0 {
					hb := make([]byte, h)
					e.Handle = &hb
				}
				b.Reset()
				if err := xdr.Write(&b, e); err != nil {
					t.Fatal(err)
				}
				if got := uint32(b.Len()); got != entryPlus3Size(name, attrs, h) {
					t.Errorf("name %d, attrs %v, handle %d: entryplus3 encodes to %d, entryPlus3Size says %d", n, attrs, h, got, entryPlus3Size(name, attrs, h))
				}
			}
		}
	}
}
