package search

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/zombar/tailraid/internal/meta"
)

// A segment (docs/18 §4) is one immutable file: a header (magic, version, the chain,
// the pass's applied index), the doc table with an offset table so an inode is found
// by binary search, the front-coded term dictionary in blocks of dictBlock terms whose
// first term is written whole so a term is found by binary search over the blocks, the
// postings, the catalogue, and a trailer with the body's length and SHA-256. A reader
// keeps the bytes and decodes what a query touches.
const (
	segMagic   = "TRSI"
	segVersion = 1
	dictBlock  = 32
	trailerLen = 4 + 8 + sha256.Size
	// RebuildAt is the chain length past which a pass merges the chain and its batch
	// into one segment (docs/18 §4, I-206).
	RebuildAt = 8
)

// Doc kinds (docs/18 §2).
const (
	KindText  uint8 = 1
	KindAudio uint8 = 2
)

// Doc is one indexed file.
type Doc struct {
	Ino    meta.Ino
	Gen    uint32
	Chunks [sha256.Size]byte // sha256 of the chunk list: the content identity (docs/18 §2)
	Kind   uint8
	Year   int
	Title  string
	Artist string
	Album  string
	Genre  string
}

// Posting is one term in one doc.
type Posting struct {
	Doc int // index into the segment's doc table
	TF  int
	Off int // byte offset of the term's first occurrence (0 for a tag)
}

// ErrSegment is a segment whose trailer, header or layout does not parse.
var ErrSegment = errors.New("search: bad segment")

// ChunksHash is a file's content identity: the hash of its chunk list.
func ChunksHash(in meta.Inode) [sha256.Size]byte {
	h := sha256.New()
	for _, c := range in.Chunks {
		h.Write(c[:])
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// --- bytes

type cursor struct {
	b   []byte
	i   int
	err error
}

func (c *cursor) uvarint() uint64 {
	if c.err != nil {
		return 0
	}
	v, n := binary.Uvarint(c.b[c.i:])
	if n <= 0 {
		c.err = ErrSegment
		return 0
	}
	c.i += n
	return v
}

func (c *cursor) int() int {
	v := c.uvarint()
	if v > 1<<40 {
		c.err = ErrSegment
		return 0
	}
	return int(v) //nolint:gosec // bounded above
}

func (c *cursor) take(n int) []byte {
	if c.err != nil {
		return nil
	}
	if n < 0 || c.i+n > len(c.b) {
		c.err = ErrSegment
		return nil
	}
	b := c.b[c.i : c.i+n]
	c.i += n
	return b
}

func (c *cursor) str() string { return string(c.take(c.int())) }

func (c *cursor) u8() uint8 {
	b := c.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (c *cursor) u32at(off int) int {
	if off < 0 || off+4 > len(c.b) {
		c.err = ErrSegment
		return 0
	}
	return int(binary.LittleEndian.Uint32(c.b[off:]))
}

type buf struct{ b []byte }

func (w *buf) uvarint(v uint64) { w.b = binary.AppendUvarint(w.b, v) }
func (w *buf) int(v int)        { w.uvarint(uint64(max(v, 0))) } //nolint:gosec // clamped
func (w *buf) str(s string)     { w.int(len(s)); w.b = append(w.b, s...) }
func (w *buf) u8(v uint8)       { w.b = append(w.b, v) }
func (w *buf) u32(v int)        { w.b = binary.LittleEndian.AppendUint32(w.b, uint32(v)) } //nolint:gosec // section offsets
func (w *buf) raw(p []byte)     { w.b = append(w.b, p...) }

// --- writer

// Encoder writes one segment: the header and the docs first, then every term in
// sorted order with its postings in doc order, then the catalogue and the trailer.
type Encoder struct {
	out, terms, post buf
	blocks           []int
	nTerms           int
	prev             string
}

// NewEncoder starts a segment of seq extending chain, built at the FSM's applied
// index, over docs (sorted by inode).
func NewEncoder(seq uint64, chain []uint64, applied uint64, docs []Doc) *Encoder {
	_ = seq // the sequence is the file's name, not its content: two leaders' bytes compare
	e := &Encoder{}
	e.out.raw([]byte(segMagic))
	e.out.u8(segVersion)
	e.out.int(len(chain))
	for _, s := range chain {
		e.out.uvarint(s)
	}
	e.out.uvarint(applied)
	var ents buf
	offs := make([]int, len(docs))
	for i, d := range docs {
		offs[i] = len(ents.b)
		ents.uvarint(uint64(d.Ino))
		ents.uvarint(uint64(d.Gen))
		ents.raw(d.Chunks[:])
		ents.u8(d.Kind)
		ents.int(d.Year)
		ents.str(d.Title)
		ents.str(d.Artist)
		ents.str(d.Album)
		ents.str(d.Genre)
	}
	e.out.int(len(docs))
	for _, o := range offs {
		e.out.u32(o)
	}
	e.out.int(len(ents.b))
	e.out.raw(ents.b)
	return e
}

// Term adds term, which sorts after the previous one, with posts in doc order.
func (e *Encoder) Term(term string, posts []Posting) {
	if e.nTerms%dictBlock == 0 {
		e.blocks = append(e.blocks, len(e.terms.b))
		e.prev = ""
	}
	shared := commonPrefix(e.prev, term)
	e.terms.int(shared)
	e.terms.str(term[shared:])
	e.terms.int(len(e.post.b))
	e.post.int(len(posts))
	last := 0
	for _, p := range posts {
		e.post.int(p.Doc - last)
		last = p.Doc
		e.post.int(p.TF)
		e.post.int(p.Off)
	}
	e.prev = term
	e.nTerms++
}

// Finish appends the catalogue and the trailer and returns the segment's bytes.
func (e *Encoder) Finish(cat []Entry) []byte {
	e.out.int(e.nTerms)
	e.out.int(len(e.blocks))
	for _, o := range e.blocks {
		e.out.u32(o)
	}
	e.out.int(len(e.terms.b))
	e.out.raw(e.terms.b)
	e.out.int(len(e.post.b))
	e.out.raw(e.post.b)
	c := encodeCatalog(cat)
	e.out.int(len(c))
	e.out.raw(c)
	sum := sha256.Sum256(e.out.b)
	n := len(e.out.b)
	e.out.raw([]byte(segMagic))
	e.out.b = binary.LittleEndian.AppendUint64(e.out.b, uint64(n)) //nolint:gosec // a length
	e.out.raw(sum[:])
	return e.out.b
}

func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	// never split a multi-byte character
	for i > 0 && i < len(b) && b[i]&0xC0 == 0x80 {
		i--
	}
	return i
}

// Encode writes a whole segment from a term map: the non-merging pass's way.
func Encode(seq uint64, chain []uint64, applied uint64, docs []Doc, terms map[string][]Posting, cat []Entry) []byte {
	e := NewEncoder(seq, chain, applied, docs)
	keys := make([]string, 0, len(terms))
	for t := range terms {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	for _, t := range keys {
		e.Term(t, terms[t])
	}
	return e.Finish(cat)
}

// --- reader

// Reader is a decoded segment, searched in place.
type Reader struct {
	Seq     uint64
	Applied uint64
	Chain   []uint64
	b       []byte // the body: everything before the trailer
	size    int    // the file's length, trailer included
	nDocs   int
	docOffs int // the doc offset table (nDocs × u32, relative to docEnts)
	docEnts int
	nTerms  int
	nBlocks int
	blkOffs int // the block offset table (nBlocks × u32, relative to terms)
	terms   int
	post    int
	cat     int
	catEnd  int
}

// Decode verifies a segment's trailer and parses its layout.
func Decode(seq uint64, b []byte) (*Reader, error) {
	if len(b) < trailerLen+5 {
		return nil, fmt.Errorf("%w: %d bytes", ErrSegment, len(b))
	}
	t := b[len(b)-trailerLen:]
	n := binary.LittleEndian.Uint64(t[4:12])
	if string(t[:4]) != segMagic || n != uint64(len(b)-trailerLen) {
		return nil, fmt.Errorf("%w: trailer", ErrSegment)
	}
	if sum := sha256.Sum256(b[:n]); !bytes.Equal(sum[:], t[12:]) {
		return nil, fmt.Errorf("%w: trailer hash", ErrSegment)
	}
	c := &cursor{b: b[:n]}
	if string(c.take(4)) != segMagic || c.u8() != segVersion {
		return nil, fmt.Errorf("%w: header", ErrSegment)
	}
	r := &Reader{Seq: seq, b: b[:n], size: len(b)}
	nc := c.int()
	for i := 0; i < nc && c.err == nil; i++ {
		r.Chain = append(r.Chain, c.uvarint())
	}
	r.Applied = c.uvarint()
	r.nDocs = c.int()
	r.docOffs = c.i
	c.take(4 * r.nDocs)
	l := c.int()
	r.docEnts = c.i
	c.take(l)
	r.nTerms = c.int()
	r.nBlocks = c.int()
	r.blkOffs = c.i
	c.take(4 * r.nBlocks)
	l = c.int()
	r.terms = c.i
	c.take(l)
	l = c.int()
	r.post = c.i
	c.take(l)
	l = c.int()
	r.cat = c.i
	c.take(l)
	r.catEnd = c.i
	if c.err != nil || c.i != len(c.b) || r.nBlocks != (r.nTerms+dictBlock-1)/dictBlock {
		return nil, fmt.Errorf("%w: layout", ErrSegment)
	}
	return r, nil
}

// Len is the segment file's length.
func (r *Reader) Len() int { return r.size }

// NumDocs is the doc table's length.
func (r *Reader) NumDocs() int { return r.nDocs }

// Doc decodes doc i.
func (r *Reader) Doc(i int) Doc {
	c := &cursor{b: r.b}
	c.i = r.docEnts + c.u32at(r.docOffs+4*i)
	d := Doc{Ino: meta.Ino(c.uvarint()), Gen: uint32(c.uvarint())} //nolint:gosec // written from a uint32
	copy(d.Chunks[:], c.take(sha256.Size))
	d.Kind = c.u8()
	d.Year = c.int()
	d.Title, d.Artist, d.Album, d.Genre = c.str(), c.str(), c.str(), c.str()
	if c.err != nil {
		return Doc{}
	}
	return d
}

func (r *Reader) docIno(i int) meta.Ino {
	c := &cursor{b: r.b}
	c.i = r.docEnts + c.u32at(r.docOffs+4*i)
	return meta.Ino(c.uvarint())
}

// FindDoc is the doc table's index of ino.
func (r *Reader) FindDoc(ino meta.Ino) (int, bool) {
	i := sort.Search(r.nDocs, func(i int) bool { return r.docIno(i) >= ino })
	return i, i < r.nDocs && r.docIno(i) == ino
}

// termIter walks the dictionary in order.
type termIter struct {
	r    *Reader
	c    cursor
	i    int
	prev string
}

func (r *Reader) iter(block int) *termIter {
	it := &termIter{r: r, c: cursor{b: r.b}, i: block * dictBlock}
	it.c.i = r.terms + it.c.u32at(r.blkOffs+4*block)
	return it
}

// next decodes the next term and its postings offset; ok is false at the end.
func (it *termIter) next() (term string, postOff int, ok bool) {
	if it.i >= it.r.nTerms || it.c.err != nil {
		return "", 0, false
	}
	if it.i%dictBlock == 0 {
		it.c.i = it.r.terms + it.c.u32at(it.r.blkOffs+4*(it.i/dictBlock))
		it.prev = ""
	}
	shared := it.c.int()
	if shared > len(it.prev) {
		it.c.err = ErrSegment
		return "", 0, false
	}
	term = it.prev[:shared] + it.c.str()
	postOff = it.c.int()
	if it.c.err != nil {
		return "", 0, false
	}
	it.prev = term
	it.i++
	return term, postOff, true
}

// blockFirst is the first term of block i, written whole.
func (r *Reader) blockFirst(i int) string {
	t, _, _ := r.iter(i).next()
	return t
}

// Lookup finds term exactly.
func (r *Reader) Lookup(term string) (postOff int, ok bool) {
	bi := sort.Search(r.nBlocks, func(i int) bool { return r.blockFirst(i) > term }) - 1
	if bi < 0 {
		return 0, false
	}
	it := r.iter(bi)
	for range dictBlock {
		t, off, ok := it.next()
		if !ok || t > term {
			return 0, false
		}
		if t == term {
			return off, true
		}
	}
	return 0, false
}

// Prefix is every term starting with p, with its postings offset, in order.
func (r *Reader) Prefix(p string) map[string]int {
	out := map[string]int{}
	if r.nTerms == 0 {
		return out
	}
	// the last block whose first term is ≤ p holds the first candidate
	bi := max(sort.Search(r.nBlocks, func(i int) bool { return r.blockFirst(i) > p })-1, 0)
	it := r.iter(bi)
	for {
		t, off, ok := it.next()
		if !ok || (t > p && !hasPrefix(t, p)) {
			return out
		}
		if hasPrefix(t, p) {
			out[t] = off
		}
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// Postings decodes the list at off.
func (r *Reader) Postings(off int) []Posting {
	c := &cursor{b: r.b, i: r.post + off}
	n := c.int()
	if c.err != nil || n > len(r.b) {
		return nil
	}
	out := make([]Posting, 0, n)
	doc := 0
	for range n {
		doc += c.int()
		p := Posting{Doc: doc, TF: c.int(), Off: c.int()}
		if c.err != nil || doc >= r.nDocs {
			return nil
		}
		out = append(out, p)
	}
	return out
}

// Catalog decodes the catalogue.
func (r *Reader) Catalog() []Entry { return decodeCatalog(r.b[r.cat:r.catEnd]) }

// catalogBytes is the encoded catalogue, for the no-change comparison.
func (r *Reader) catalogBytes() []byte { return r.b[r.cat:r.catEnd] }
