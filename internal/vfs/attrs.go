package vfs

import (
	"math"

	"github.com/zombar/tailraid/internal/meta"
)

// A file being written lands whole (docs/04 §6, I-401): the mode, owner and times a
// client sets on a file this gateway holds unflushed data for wait in the overlay, logged
// in its WAL, and ride the flush's Commit.

// heldMask is what a held SETATTR may carry: no flags (a directory's) and no size (a
// truncate, which the overlay takes as one).
const heldMask = meta.AttrMode | meta.AttrUid | meta.AttrGid | meta.AttrAtime | meta.AttrMtime

// timeMask is the part a later write or truncate supersedes (I-230).
const timeMask = meta.AttrAtime | meta.AttrMtime

var attrBits = [5]uint8{meta.AttrMode, meta.AttrUid, meta.AttrGid, meta.AttrAtime, meta.AttrMtime}

// pendingAttrs are held attributes with the log position of the record each field came
// from: a segment holding one is never dropped (I-397), and a commit drops the fields
// logged before its plan was captured.
type pendingAttrs struct {
	a   meta.CommitAttrs
	pos [len(attrBits)]int64
}

func (p *pendingAttrs) empty() bool { return p.a.Mask == 0 }

// set takes a's fields, logged at pos.
func (p *pendingAttrs) set(a meta.CommitAttrs, pos int64) {
	for i, b := range attrBits {
		if a.Mask&b != 0 {
			p.copyField(b, a)
			p.pos[i] = pos
		}
	}
}

// over takes q's fields over p's: q is newer.
func (p *pendingAttrs) over(q pendingAttrs) {
	for i, b := range attrBits {
		if q.a.Mask&b != 0 {
			p.copyField(b, q.a)
			p.pos[i] = q.pos[i]
		}
	}
}

func (p *pendingAttrs) copyField(b uint8, a meta.CommitAttrs) {
	p.a.Mask |= b
	switch b {
	case meta.AttrMode:
		p.a.Mode = a.Mode
	case meta.AttrUid:
		p.a.Uid = a.Uid
	case meta.AttrGid:
		p.a.Gid = a.Gid
	case meta.AttrAtime:
		p.a.Atime = a.Atime
	case meta.AttrMtime:
		p.a.Mtime = a.Mtime
	}
}

// clear drops the fields in mask.
func (p *pendingAttrs) clear(mask uint8) {
	p.a.Mask &^= mask
	if mask&meta.AttrMode != 0 {
		p.a.Mode = 0
	}
	if mask&meta.AttrUid != 0 {
		p.a.Uid = 0
	}
	if mask&meta.AttrGid != 0 {
		p.a.Gid = 0
	}
	if mask&meta.AttrAtime != 0 {
		p.a.Atime = 0
	}
	if mask&meta.AttrMtime != 0 {
		p.a.Mtime = 0
	}
}

// dropBefore clears the fields logged below position w: a Commit landed them.
func (p *pendingAttrs) dropBefore(w int64) {
	for i, b := range attrBits {
		if p.a.Mask&b != 0 && p.pos[i] < w {
			p.clear(b)
		}
	}
}

// at records that every held field is now logged at pos (a re-log, a rotation, a
// compaction).
func (p *pendingAttrs) at(pos int64) {
	for i, b := range attrBits {
		if p.a.Mask&b != 0 {
			p.pos[i] = pos
		}
	}
}

// lowest is the log position of the oldest record a held field came from, MaxInt64 for
// none.
func (p *pendingAttrs) lowest() int64 {
	low := int64(math.MaxInt64)
	for i, b := range attrBits {
		if p.a.Mask&b != 0 {
			low = min(low, p.pos[i])
		}
	}
	return low
}
