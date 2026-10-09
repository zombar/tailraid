package meta

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Little-endian primitives shared by the command and snapshot codecs. The writer never
// fails; the reader records the first error and all later reads are no-ops, so decoders
// can be written straight-line and check r.err once.

const (
	maxStringLen = 1 << 16 // names, tokens, addresses; generous but bounded
	maxListLen   = 1 << 24 // slices and maps
)

var errShort = errors.New("meta: truncated input")

type writer struct {
	b   []byte
	out io.Writer // when set, spill() hands full buffers to it (snapshots, I-104)
	err error
}

// spillAt is the buffer size at which a streaming writer hands bytes to its sink.
const spillAt = 1 << 20

// spill writes the buffer to out when it is large; the state codec calls it after each
// record so a snapshot of a million chunks never sits in memory twice.
func (w *writer) spill() {
	if w.out == nil || w.err != nil || len(w.b) < spillAt {
		return
	}
	if _, err := w.out.Write(w.b); err != nil {
		w.err = err
	}
	w.b = w.b[:0]
}

// flush writes whatever is buffered to out.
func (w *writer) flush() error {
	if w.err != nil {
		return w.err
	}
	if w.out != nil && len(w.b) > 0 {
		if _, err := w.out.Write(w.b); err != nil {
			return err
		}
		w.b = w.b[:0]
	}
	return nil
}

func (w *writer) u8(v uint8)   { w.b = append(w.b, v) }
func (w *writer) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *writer) u64(v uint64) { w.b = binary.LittleEndian.AppendUint64(w.b, v) }
func (w *writer) i64(v int64)  { w.u64(uint64(v)) }
func (w *writer) uvarint(v uint64) {
	w.b = binary.AppendUvarint(w.b, v)
}

func (w *writer) bytes(p []byte) {
	w.uvarint(uint64(len(p)))
	w.b = append(w.b, p...)
}
func (w *writer) str(s string) { w.bytes([]byte(s)) }
func (w *writer) hash(h Hash)  { w.b = append(w.b, h[:]...) }
func (w *writer) bool(v bool) {
	if v {
		w.u8(1)
	} else {
		w.u8(0)
	}
}

func (w *writer) nodeIdxs(p []NodeIdx) {
	w.uvarint(uint64(len(p)))
	for _, n := range p {
		w.u16(uint16(n))
	}
}

type reader struct {
	b   []byte
	pos int
	err error
	src io.Reader // when set, fill() pulls more bytes as take() needs them (snapshots)
}

// fill makes at least n bytes available from pos when a source is set, compacting the
// consumed prefix first. At end of input fewer bytes stay available and take() reports
// errShort as before.
func (r *reader) fill(n int) {
	if r.src == nil || r.err != nil || r.pos+n <= len(r.b) {
		return
	}
	rem := copy(r.b, r.b[r.pos:])
	r.b, r.pos = r.b[:rem], 0
	for len(r.b) < n {
		if cap(r.b) < n {
			nb := make([]byte, len(r.b), max(n, 2*cap(r.b), 64<<10))
			copy(nb, r.b)
			r.b = nb
		}
		m, err := r.src.Read(r.b[len(r.b):cap(r.b)])
		r.b = r.b[:len(r.b)+m]
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.fail(fmt.Errorf("meta: read: %w", err))
			}
			return
		}
	}
}

func (r *reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n >= 0 {
		r.fill(n)
	}
	if n < 0 || r.pos+n > len(r.b) {
		r.fail(errShort)
		return nil
	}
	p := r.b[r.pos : r.pos+n]
	r.pos += n
	return p
}

func (r *reader) u8() uint8 {
	p := r.take(1)
	if p == nil {
		return 0
	}
	return p[0]
}

func (r *reader) u16() uint16 {
	p := r.take(2)
	if p == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(p)
}

func (r *reader) u32() uint32 {
	p := r.take(4)
	if p == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(p)
}

func (r *reader) u64() uint64 {
	p := r.take(8)
	if p == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(p)
}
func (r *reader) i64() int64 { return int64(r.u64()) }
func (r *reader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	r.fill(binary.MaxVarintLen64)
	v, n := binary.Uvarint(r.b[r.pos:])
	if n <= 0 {
		r.fail(fmt.Errorf("meta: bad uvarint"))
		return 0
	}
	// Reject non-minimal encodings (e.g. 0x80 0x00 for zero) so that decode∘encode is
	// the identity and equal values have one byte representation (I-22).
	if n > 1 && r.b[r.pos+n-1] == 0 {
		r.fail(fmt.Errorf("meta: non-canonical uvarint"))
		return 0
	}
	r.pos += n
	return v
}

func (r *reader) length(limit int) int {
	n := r.uvarint()
	if r.err != nil {
		return 0
	}
	if n <= uint64(limit) {
		r.fill(int(n)) // a streaming source: the bytes must be there, not merely buffered
	}
	if n > uint64(limit) || n > uint64(len(r.b)-r.pos) {
		r.fail(fmt.Errorf("meta: length %d out of range", n))
		return 0
	}
	return int(n)
}

func (r *reader) bytes() []byte {
	n := r.length(maxStringLen)
	p := r.take(n)
	if p == nil {
		return nil
	}
	return append([]byte(nil), p...)
}
func (r *reader) str() string { return string(r.bytes()) }
func (r *reader) hash() Hash {
	var h Hash
	copy(h[:], r.take(32))
	return h
}

func (r *reader) bool() bool {
	switch r.u8() {
	case 0:
		return false
	case 1:
		return true
	default:
		r.fail(fmt.Errorf("meta: bad bool"))
		return false
	}
}

func (r *reader) nodeIdxs() []NodeIdx {
	n := r.length(256)
	if r.err != nil || n == 0 {
		return nil
	}
	out := make([]NodeIdx, n)
	for i := range out {
		out[i] = NodeIdx(r.u16())
	}
	return out
}

// done reports an error if anything failed or bytes remain.
func (r *reader) done() error {
	if r.err != nil {
		return r.err
	}
	r.fill(1) // a source: is there anything after the last record?
	if r.pos != len(r.b) {
		return fmt.Errorf("meta: %d trailing bytes", len(r.b)-r.pos)
	}
	return nil
}
