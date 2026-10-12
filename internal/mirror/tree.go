package mirror

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zombar/tailraid/internal/meta"
)

// EntryType is a tree entry's kind, as the volume's FileType.
type EntryType uint8

// Entry types.
const (
	TypeFile    EntryType = EntryType(meta.TFile)
	TypeDir     EntryType = EntryType(meta.TDir)
	TypeSymlink EntryType = EntryType(meta.TSymlink)
)

// Entry is one name in a snapshot's directory (docs/16 §2): what a reader of the volume
// saw at the snapshot's index, plus where its bytes or children are on the mirror.
type Entry struct {
	Name     string
	Type     EntryType
	Mode     uint32
	Uid, Gid uint32
	Mtime    int64
	Ino      uint64 // the live inode then; unique within a snapshot (handles, docs/01)
	Gen      uint32
	Size     uint64      // files
	Chunks   []meta.Hash // files: one hash per chunk slot, HashZero for a hole
	Child    meta.Hash   // directories: the child tree
	Target   string      // symlinks
}

// Tree is one directory of one snapshot, entries in bytewise name order.
type Tree struct{ Entries []Entry }

const (
	treeMagic     = "TRT1"
	maxTreeEntry  = 1 << 24
	maxTargetLen  = 4096
	maxTreeChunks = meta.MaxChunksPerFile
)

// ErrBadTree reports a tree that is not in the canonical form.
var ErrBadTree = errors.New("mirror: malformed tree")

// EncodeTree is the canonical encoding (docs/16 §2): entries sorted bytewise by name,
// unique, so equal directories encode to equal bytes and share one hash.
func EncodeTree(t Tree) ([]byte, error) {
	es := slices.Clone(t.Entries)
	slices.SortFunc(es, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	b := make([]byte, 0, 64+len(es)*96)
	b = append(b, treeMagic...)
	b = binary.AppendUvarint(b, uint64(len(es)))
	for i := range es {
		e := &es[i]
		if i > 0 && es[i-1].Name == e.Name {
			return nil, fmt.Errorf("%w: name %q twice", ErrBadTree, e.Name)
		}
		if err := checkEntry(e); err != nil {
			return nil, err
		}
		b = appendStr(b, e.Name)
		b = append(b, byte(e.Type))
		b = binary.LittleEndian.AppendUint32(b, e.Mode)
		b = binary.LittleEndian.AppendUint32(b, e.Uid)
		b = binary.LittleEndian.AppendUint32(b, e.Gid)
		b = binary.LittleEndian.AppendUint64(b, uint64(e.Mtime))
		b = binary.LittleEndian.AppendUint64(b, e.Ino)
		b = binary.LittleEndian.AppendUint32(b, e.Gen)
		switch e.Type {
		case TypeFile:
			b = binary.LittleEndian.AppendUint64(b, e.Size)
			b = binary.AppendUvarint(b, uint64(len(e.Chunks)))
			for _, h := range e.Chunks {
				b = append(b, h[:]...)
			}
		case TypeDir:
			b = append(b, e.Child[:]...)
		case TypeSymlink:
			b = appendStr(b, e.Target)
		}
	}
	return b, nil
}

// TreeHash is the name a tree is stored under: SHA-256 of its encoding.
func TreeHash(enc []byte) meta.Hash { return sha256.Sum256(enc) }

func checkEntry(e *Entry) error {
	if e.Name == "" || len(e.Name) > meta.MaxNameLen || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, "/\x00") {
		return fmt.Errorf("%w: name %q", ErrBadTree, e.Name)
	}
	switch e.Type {
	case TypeFile:
		if len(e.Chunks) > maxTreeChunks {
			return fmt.Errorf("%w: %d chunks", ErrBadTree, len(e.Chunks))
		}
	case TypeDir:
	case TypeSymlink:
		if len(e.Target) > maxTargetLen {
			return fmt.Errorf("%w: target of %d bytes", ErrBadTree, len(e.Target))
		}
	default:
		return fmt.Errorf("%w: type %d", ErrBadTree, e.Type)
	}
	return nil
}

func appendStr(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// DecodeTree parses a tree and refuses anything EncodeTree would not have written:
// another magic, unsorted or repeated names, an unknown type, short or trailing bytes.
func DecodeTree(b []byte) (Tree, error) {
	d := &dec{b: b}
	if !bytes.Equal(d.take(len(treeMagic)), []byte(treeMagic)) {
		return Tree{}, fmt.Errorf("%w: magic", ErrBadTree)
	}
	n := d.uvarint()
	if n > maxTreeEntry {
		return Tree{}, fmt.Errorf("%w: %d entries", ErrBadTree, n)
	}
	t := Tree{Entries: make([]Entry, 0, min(n, 1<<12))}
	for i := uint64(0); i < n && d.err == nil; i++ {
		e := Entry{
			Name: d.str(meta.MaxNameLen), Type: EntryType(d.u8()), Mode: d.u32(), Uid: d.u32(), Gid: d.u32(),
			Mtime: int64(d.u64()), Ino: d.u64(), Gen: d.u32(),
		}
		switch e.Type {
		case TypeFile:
			e.Size = d.u64()
			c := d.uvarint()
			if c > maxTreeChunks {
				return Tree{}, fmt.Errorf("%w: %d chunks", ErrBadTree, c)
			}
			if c > 0 {
				e.Chunks = make([]meta.Hash, c)
				for j := range e.Chunks {
					copy(e.Chunks[j][:], d.take(32))
				}
			}
		case TypeDir:
			copy(e.Child[:], d.take(32))
		case TypeSymlink:
			e.Target = d.str(maxTargetLen)
		}
		if d.err != nil {
			break
		}
		if err := checkEntry(&e); err != nil {
			return Tree{}, err
		}
		if len(t.Entries) > 0 && t.Entries[len(t.Entries)-1].Name >= e.Name {
			return Tree{}, fmt.Errorf("%w: names out of order at %q", ErrBadTree, e.Name)
		}
		t.Entries = append(t.Entries, e)
	}
	if d.err != nil {
		return Tree{}, d.err
	}
	if d.pos != len(d.b) {
		return Tree{}, fmt.Errorf("%w: %d trailing bytes", ErrBadTree, len(d.b)-d.pos)
	}
	return t, nil
}

// Lookup finds name in t.
func (t Tree) Lookup(name string) (Entry, bool) {
	i, ok := slices.BinarySearchFunc(t.Entries, name, func(e Entry, n string) int { return strings.Compare(e.Name, n) })
	if !ok {
		return Entry{}, false
	}
	return t.Entries[i], true
}

type dec struct {
	b   []byte
	pos int
	err error
}

func (d *dec) take(n int) []byte {
	if d.err != nil || n < 0 || d.pos+n > len(d.b) {
		if d.err == nil {
			d.err = fmt.Errorf("%w: short", ErrBadTree)
		}
		return make([]byte, max(n, 0))
	}
	out := d.b[d.pos : d.pos+n]
	d.pos += n
	return out
}

func (d *dec) u8() uint8   { return d.take(1)[0] }
func (d *dec) u32() uint32 { return binary.LittleEndian.Uint32(d.take(4)) }
func (d *dec) u64() uint64 { return binary.LittleEndian.Uint64(d.take(8)) }

func (d *dec) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b[d.pos:])
	if n <= 0 {
		d.err = fmt.Errorf("%w: varint", ErrBadTree)
		return 0
	}
	d.pos += n
	return v
}

func (d *dec) str(limit int) string {
	n := d.uvarint()
	if n > uint64(limit) {
		d.err = fmt.Errorf("%w: string of %d bytes", ErrBadTree, n)
		return ""
	}
	return string(d.take(int(n)))
}
