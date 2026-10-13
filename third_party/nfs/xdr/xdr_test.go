package xdr

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"testing"
)

// A length field is believed only as far as the request holds the bytes: a 12-byte body
// whose opaque, string or array claims 4 GiB is refused without allocating it.
// Invariant: I-244
func TestRead_LengthBoundedByBody(t *testing.T) {
	huge := func() *bytes.Reader {
		var b [12]byte
		binary.BigEndian.PutUint32(b[:], 0xFFFFFFF0)
		return bytes.NewReader(b[:])
	}
	type withBytes struct{ B []byte }
	type withString struct{ S string }
	type withArray struct{ A []uint64 }
	cases := []struct {
		name string
		read func(r *bytes.Reader) error
	}{
		{"ReadOpaque", func(r *bytes.Reader) error { _, err := ReadOpaque(r); return err }},
		{"Read opaque field", func(r *bytes.Reader) error { var v withBytes; return Read(r, &v) }},
		{"Read string field", func(r *bytes.Reader) error { var v withString; return Read(r, &v) }},
		{"Read array field", func(r *bytes.Reader) error { var v withArray; return Read(r, &v) }},
	}
	for _, tc := range cases {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := tc.read(huge())
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Errorf("%s: a 4 GiB length in a 12-byte body was accepted", tc.name)
		}
		if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
			t.Errorf("%s: allocated %d bytes for a 12-byte body", tc.name, n)
		}
	}
	// {0, 1, max-1, max, max+1}: an opaque that fits the body is read whole
	for _, n := range []int{0, 1, 7, 8} {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(n))
		b.Write(bytes.Repeat([]byte{7}, 8)) // 8 bytes follow: n ≤ 8 fits
		got, err := ReadOpaque(bytes.NewReader(b.Bytes()))
		if err != nil || len(got) != n {
			t.Errorf("opaque of %d in a body of 8: %d bytes, %v", n, len(got), err)
		}
	}
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(9))
	b.Write(bytes.Repeat([]byte{7}, 8))
	if _, err := ReadOpaque(bytes.NewReader(b.Bytes())); err == nil {
		t.Error("an opaque of 9 in a body of 8 was accepted")
	}
}
