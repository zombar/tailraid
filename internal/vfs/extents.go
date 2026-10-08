// Package vfs implements the gateway-side write buffer (dirty extents backed by a per-inode
// write-ahead log), and later the flusher, reader and billy.Filesystem adapter.
package vfs

import "sort"

// extent is a dirty byte range [off, off+n) whose bytes live in the WAL at walOff
// (the offset of the first data byte; split extents point inside a record's data).
type extent struct {
	off, n uint64
	walOff int64
}

func (x extent) end() uint64 { return x.off + x.n }

// extents is sorted by off, non-overlapping, every n > 0.
type extents []extent

// insert adds x, overriding any existing bytes in its range (last writer wins).
func (e *extents) insert(x extent) {
	if x.n == 0 {
		return
	}
	out := make(extents, 0, len(*e)+2)
	for _, y := range *e {
		switch {
		case y.end() <= x.off || y.off >= x.end():
			out = append(out, y)
		default:
			if y.off < x.off { // keep left part
				out = append(out, extent{off: y.off, n: x.off - y.off, walOff: y.walOff})
			}
			if y.end() > x.end() { // keep right part
				d := x.end() - y.off
				out = append(out, extent{off: x.end(), n: y.end() - x.end(), walOff: y.walOff + int64(d)})
			}
		}
	}
	out = append(out, x)
	sort.Slice(out, func(i, j int) bool { return out[i].off < out[j].off })
	*e = out
}

// overlapping returns the extents intersecting [off, off+n), in order (not clipped).
func (e extents) overlapping(off, n uint64) []extent {
	var out []extent
	end := off + n
	for _, x := range e {
		if x.end() <= off {
			continue
		}
		if x.off >= end {
			break
		}
		out = append(out, x)
	}
	return out
}

// truncate drops bytes at or beyond size.
func (e *extents) truncate(size uint64) {
	out := (*e)[:0]
	for _, x := range *e {
		switch {
		case x.off >= size:
		case x.end() > size:
			out = append(out, extent{off: x.off, n: size - x.off, walOff: x.walOff})
		default:
			out = append(out, x)
		}
	}
	*e = out
}

// maxEnd is the end of the last extent, or 0.
func (e extents) maxEnd() uint64 {
	if len(e) == 0 {
		return 0
	}
	return e[len(e)-1].end()
}

// bytes is the number of dirty bytes.
func (e extents) bytes() uint64 {
	var n uint64
	for _, x := range e {
		n += x.n
	}
	return n
}

// coversChunk reports whether chunk idx of a file of the given size, i.e. the byte range
// [idx*cs, min((idx+1)*cs, size)), is entirely dirty. An empty range is never covered.
func (e extents) coversChunk(idx uint64, cs uint32, size uint64) bool {
	start := idx * uint64(cs)
	end := min(start+uint64(cs), size)
	if end <= start {
		return false
	}
	cursor := start
	for _, x := range e.overlapping(start, end-start) {
		if x.off > cursor {
			return false
		}
		cursor = max(cursor, x.end())
		if cursor >= end {
			return true
		}
	}
	return cursor >= end
}

// clone returns an independent copy.
func (e extents) clone() extents {
	out := make(extents, len(e))
	copy(out, e)
	return out
}
