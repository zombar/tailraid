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

// insert adds x, overriding any existing bytes in its range (last writer wins). The
// overlapping run is found by binary search, so a WRITE costs O(log n) plus the splice
// rather than a re-sort of the whole map.
func (e *extents) insert(x extent) {
	if x.n == 0 {
		return
	}
	old := *e
	if n := len(old); n == 0 || old[n-1].end() <= x.off {
		*e = append(*e, x) // past the last extent: a sequential WRITE appends in place (I-399)
		return
	}
	i, j := old.span(x.off, x.n) // [i, j) overlaps x; old[:i] lies before, old[j:] after
	out := make(extents, 0, len(old)+2)
	out = append(out, old[:i]...)
	if i < j && old[i].off < x.off { // keep the left part of the first overlapped extent
		y := old[i]
		out = append(out, extent{off: y.off, n: x.off - y.off, walOff: y.walOff})
	}
	out = append(out, x)
	if i < j && old[j-1].end() > x.end() { // keep the right part of the last one
		y := old[j-1]
		d := x.end() - y.off
		out = append(out, extent{off: x.end(), n: y.end() - x.end(), walOff: y.walOff + int64(d)})
	}
	out = append(out, old[j:]...)
	*e = out
}

// span returns the index range [i, j) of the extents intersecting [off, off+n).
func (e extents) span(off, n uint64) (int, int) {
	i := sort.Search(len(e), func(k int) bool { return e[k].end() > off })
	j := i
	for j < len(e) && e[j].off < off+n {
		j++
	}
	return i, j
}

// overlapping returns the extents intersecting [off, off+n), in order (not clipped). The
// result aliases e and is read-only.
func (e extents) overlapping(off, n uint64) []extent {
	i, j := e.span(off, n)
	return e[i:j]
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
	return e.covers(start, end)
}

// covers reports whether every byte of [start, end) is dirty; an empty range never is.
func (e extents) covers(start, end uint64) bool {
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

// without returns the parts of e outside the chunks idxs (ascending) of size cs, walOff
// adjusted: the dirty bytes a partial flush leaves behind once those chunks are committed.
func (e extents) without(idxs []uint64, cs uint64) extents {
	var out extents
	j := 0
	for _, x := range e {
		cur := x
		for cur.n > 0 {
			for j < len(idxs) && (idxs[j]+1)*cs <= cur.off {
				j++
			}
			if j == len(idxs) || idxs[j]*cs >= cur.end() {
				out = append(out, cur)
				break
			}
			cstart, cend := idxs[j]*cs, (idxs[j]+1)*cs
			if cur.off < cstart { // keep the part before the chunk
				out = append(out, extent{off: cur.off, n: cstart - cur.off, walOff: cur.walOff})
				skip := cstart - cur.off
				cur = extent{off: cstart, n: cur.n - skip, walOff: cur.walOff + int64(skip)}
			}
			if cur.end() <= cend { // the rest lies inside the chunk
				break
			}
			skip := cend - cur.off
			cur = extent{off: cend, n: cur.n - skip, walOff: cur.walOff + int64(skip)}
		}
	}
	return out
}

// clone returns an independent copy.
func (e extents) clone() extents {
	out := make(extents, len(e))
	copy(out, e)
	return out
}

// fileRange is a byte range [start, end) of a file.
type fileRange struct{ start, end uint64 }

// minus is the byte ranges of e that r does not cover, adjacent ones merged: the part of
// a plan a partial Commit made durable, given its residual (both sorted, r within e).
func (e extents) minus(r extents) []fileRange {
	var out []fileRange
	add := func(start, end uint64) {
		if end <= start {
			return
		}
		if n := len(out); n > 0 && out[n-1].end == start {
			out[n-1].end = end
			return
		}
		out = append(out, fileRange{start, end})
	}
	j := 0
	for _, x := range e {
		cur := x.off
		for j < len(r) && r[j].end() <= cur {
			j++
		}
		for k := j; k < len(r) && r[k].off < x.end(); k++ {
			add(cur, r[k].off)
			cur = max(cur, r[k].end())
		}
		add(cur, x.end())
	}
	return out
}

// cleaned drops every piece of e whose bytes were logged below position w and lie inside
// ranges (ascending, disjoint): what a clean record says a Commit made durable (docs/04
// §2.1). Pieces logged at or after w, and those outside the ranges, are kept.
func (e extents) cleaned(w int64, ranges []fileRange) extents {
	out := make(extents, 0, len(e))
	for _, x := range e {
		if x.walOff >= w {
			out = append(out, x)
			continue
		}
		cur := x
		for _, r := range ranges {
			if cur.n == 0 || r.start >= cur.end() {
				break
			}
			if r.end <= cur.off {
				continue
			}
			if cur.off < r.start { // the part before the range stays
				out = append(out, extent{off: cur.off, n: r.start - cur.off, walOff: cur.walOff})
			}
			if r.end >= cur.end() {
				cur.n = 0
				break
			}
			skip := r.end - cur.off
			cur = extent{off: r.end, n: cur.n - skip, walOff: cur.walOff + int64(skip)} //nolint:gosec // within one record
		}
		if cur.n > 0 {
			out = append(out, cur)
		}
	}
	return out
}
