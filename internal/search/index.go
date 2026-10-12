package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/zombar/tailraid/internal/meta"
)

// Dir is the index's folder in the volume (docs/18 §4).
const Dir = "/" + meta.ReservedDir + "/index"

// Source is what the index and the pass read from the FSM: meta.Store.
type Source interface {
	ForEachInode(fn func(meta.Inode) bool)
	GetInode(meta.Ino) (meta.Inode, bool)
	PathOf(meta.Ino) ([]string, error)
	Resolve([]string) (meta.Inode, error)
	ReadDir(meta.Ino) ([]meta.Inode, error)
	Lookup(meta.Ino, string) (meta.Inode, bool)
	Applied() uint64
	Libraries() []meta.Library
	Volume() meta.Volume
}

// Volume is the index's folder as the pass and the readers use it.
type Volume interface {
	// Create makes name exclusively: an existing name is ErrExist (the compare-and-swap of I-206).
	Create(ctx context.Context, name string) error
	// WriteFile writes data whole: a temp sibling, flushed, then renamed over name.
	WriteFile(ctx context.Context, name string, data []byte) error
	ReadFile(ctx context.Context, name string) ([]byte, error)
	// ReadPrefix reads the first n bytes of in (fewer at its end).
	ReadPrefix(ctx context.Context, in meta.Inode, n int) ([]byte, error)
	Remove(ctx context.Context, name string) error
}

// Index is the current chain: the newest complete segment and the ones it extends,
// newest first (docs/18 §4).
type Index struct {
	Segs    []*Reader
	Bytes   int64
	Broken  int    // segments whose trailer failed
	MaxSeq  uint64 // the highest sequence named in the folder, complete or not
	dir     meta.Ino
	version uint64
}

func segName(seq uint64) string { return strconv.FormatUint(seq, 10) + ".seg" }

func parseSeg(name string) (uint64, bool) {
	s, ok := strings.CutSuffix(name, ".seg")
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseUint(s, 10, 64)
	return seq, err == nil && strconv.FormatUint(seq, 10) == s
}

// Load reads the chain: the highest segment whose trailer verifies and the segments
// its header names, each verified, a failing one skipped (I-206). A missing folder is
// an empty index.
func Load(ctx context.Context, src Source, vol Volume) (*Index, error) {
	x := &Index{}
	dir, err := src.Resolve(meta.SplitPath(Dir))
	if errors.Is(err, meta.ErrNotExist) {
		return x, nil
	}
	if err != nil {
		return nil, fmt.Errorf("search: index folder: %w", err)
	}
	x.dir, x.version = dir.Ino, dir.Version
	kids, err := src.ReadDir(dir.Ino)
	if err != nil {
		return nil, fmt.Errorf("search: index folder: %w", err)
	}
	var seqs []uint64
	for i := range kids {
		if seq, ok := parseSeg(kids[i].Name); ok && kids[i].Type == meta.TFile {
			seqs = append(seqs, seq)
			x.MaxSeq = max(x.MaxSeq, seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] > seqs[j] })
	load := func(seq uint64) *Reader {
		b, err := vol.ReadFile(ctx, Dir+"/"+segName(seq))
		if err != nil {
			x.Broken++
			return nil
		}
		r, err := Decode(seq, b)
		if err != nil {
			x.Broken++
			return nil
		}
		return r
	}
	var head *Reader
	for _, s := range seqs {
		if head = load(s); head != nil {
			break
		}
	}
	if head == nil {
		return x, nil
	}
	x.Segs = append(x.Segs, head)
	x.Bytes += int64(head.Len())
	chain := append([]uint64(nil), head.Chain...)
	sort.Slice(chain, func(i, j int) bool { return chain[i] > chain[j] })
	for _, s := range chain {
		if r := load(s); r != nil {
			x.Segs = append(x.Segs, r)
			x.Bytes += int64(r.Len())
		}
	}
	return x, nil
}

// Seqs names the chain's segments, newest first.
func (x *Index) Seqs() []uint64 {
	out := make([]uint64, len(x.Segs))
	for i, s := range x.Segs {
		out[i] = s.Seq
	}
	return out
}

// Docs counts the chain's doc entries.
func (x *Index) Docs() int {
	n := 0
	for _, s := range x.Segs {
		n += s.NumDocs()
	}
	return n
}

// FindDoc is the newest doc entry of ino: the segment's index in Segs and the doc's.
func (x *Index) FindDoc(ino meta.Ino) (seg, doc int, ok bool) {
	for i, s := range x.Segs {
		if d, ok := s.FindDoc(ino); ok {
			return i, d, true
		}
	}
	return 0, 0, false
}

// Stale reports whether the folder changed since the load: a reader reloads then.
func (x *Index) Stale(src Source) bool {
	dir, err := src.Resolve(meta.SplitPath(Dir))
	if err != nil {
		return x.dir != 0
	}
	return dir.Ino != x.dir || dir.Version != x.version
}

// isExist is the losing create: the vfs maps meta.ErrExist to the OS's.
func isExist(err error) bool { return errors.Is(err, meta.ErrExist) || errors.Is(err, os.ErrExist) }
