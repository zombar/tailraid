package meta

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// A handle decodes iff it is exactly HandleLen bytes under the magic, and what decodes
// re-encodes to the same bytes; nothing else is accepted and nothing panics. The same
// holds for the 24-byte snapshot layout under its own magic, and no input decodes as both.
//
// Invariant: I-16, I-17, I-185
func FuzzDecodeHandle(f *testing.F) {
	f.Add(EncodeHandle(RootIno, 1))
	f.Add(EncodeHandle(Ino(1<<40), 7))
	f.Add(EncodeSnapHandle(SnapHandle{Snap: 3, Gen: 2, Ino: 9}))
	f.Add(EncodeSnapHandle(SnapHandle{Ino: 4, Gen: SnapGenMirror}))
	f.Add([]byte{})
	f.Add(make([]byte, HandleLen-1))
	f.Add(make([]byte, HandleLen))
	f.Add(make([]byte, HandleLen+1))
	f.Add(make([]byte, SnapHandleLen))
	f.Fuzz(func(t *testing.T, b []byte) {
		ino, gen, ok := DecodeHandle(b)
		wellFormed := len(b) == HandleLen && binary.BigEndian.Uint32(b[:4]) == HandleMagic
		if ok != wellFormed {
			t.Fatalf("ok=%v for %x (well-formed %v)", ok, b, wellFormed)
		}
		if ok && !bytes.Equal(EncodeHandle(ino, gen), b) {
			t.Fatalf("round trip: %x -> (%d, %d) -> %x", b, ino, gen, EncodeHandle(ino, gen))
		}
		sh, sok := DecodeSnapHandle(b)
		snapForm := len(b) == SnapHandleLen && binary.BigEndian.Uint32(b[:4]) == SnapHandleMagic && binary.BigEndian.Uint32(b[20:]) == 0
		if sok != snapForm {
			t.Fatalf("snapshot ok=%v for %x (well-formed %v)", sok, b, snapForm)
		}
		if sok && !bytes.Equal(EncodeSnapHandle(sh), b) {
			t.Fatalf("snapshot round trip: %x -> %+v -> %x", b, sh, EncodeSnapHandle(sh))
		}
		if ok && sok {
			t.Fatalf("%x decodes as both layouts", b)
		}
	})
}

// The snapshot layout is 24 big-endian bytes: magic, SnapID, Gen, Ino, zero (docs/01).
//
// Invariant: I-185
func TestHandle_Snapshot_RoundTrip(t *testing.T) {
	for _, h := range []SnapHandle{
		{},                           // /.snapshots
		{Ino: 7, Gen: SnapGenMirror}, // /.snapshots/<mirror 7>
		{Snap: 1},                    // a snapshot's root
		{Snap: 1<<32 - 1, Gen: 3, Ino: 1 << 40},
	} {
		b := EncodeSnapHandle(h)
		if len(b) != SnapHandleLen || !bytes.Equal(b[:4], []byte("TRHS")) {
			t.Fatalf("%+v: %x", h, b)
		}
		if got, ok := DecodeSnapHandle(b); !ok || got != h {
			t.Fatalf("%+v: decoded %+v ok=%v", h, got, ok)
		}
		if _, _, ok := DecodeHandle(b); ok {
			t.Fatalf("%+v: a snapshot handle decodes as a live one", h)
		}
	}
	if _, ok := DecodeSnapHandle(EncodeHandle(RootIno, 1)); ok {
		t.Fatal("a live handle decodes as a snapshot one")
	}
}
