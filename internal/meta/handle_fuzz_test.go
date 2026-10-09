package meta

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// A handle decodes iff it is exactly HandleLen bytes under the magic, and what decodes
// re-encodes to the same bytes; nothing else is accepted and nothing panics.
//
// Invariant: I-16, I-17
func FuzzDecodeHandle(f *testing.F) {
	f.Add(EncodeHandle(RootIno, 1))
	f.Add(EncodeHandle(Ino(1<<40), 7))
	f.Add([]byte{})
	f.Add(make([]byte, HandleLen-1))
	f.Add(make([]byte, HandleLen))
	f.Add(make([]byte, HandleLen+1))
	f.Fuzz(func(t *testing.T, b []byte) {
		ino, gen, ok := DecodeHandle(b)
		wellFormed := len(b) == HandleLen && binary.BigEndian.Uint32(b[:4]) == HandleMagic
		if ok != wellFormed {
			t.Fatalf("ok=%v for %x (well-formed %v)", ok, b, wellFormed)
		}
		if ok && !bytes.Equal(EncodeHandle(ino, gen), b) {
			t.Fatalf("round trip: %x -> (%d, %d) -> %x", b, ino, gen, EncodeHandle(ino, gen))
		}
	})
}
