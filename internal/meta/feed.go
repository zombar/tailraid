package meta

import (
	"slices"
	"sort"
)

// FeedMax bounds the change feed: past it the oldest half is dropped and the floor rises
// (docs/02 §The change feed, I-386). 2^19 records of 16 bytes is 8 MiB.
const FeedMax = 1 << 19

// changeFeed is which inodes each applied index created, changed or deleted, complete
// for every index after floor. The applier's, under the FSM lock; never encoded.
type changeFeed struct {
	floor uint64
	log   []feedEnt // in index order
	max   int
}

type feedEnt struct {
	index uint64
	ino   Ino
}

func (c *changeFeed) add(index uint64, ino Ino) {
	if n := len(c.log); n > 0 && c.log[n-1] == (feedEnt{index, ino}) {
		return // the same inode twice in one apply
	}
	c.log = append(c.log, feedEnt{index, ino})
	if len(c.log) > c.max {
		drop := len(c.log) / 2
		c.floor = c.log[drop-1].index
		for drop < len(c.log) && c.log[drop].index == c.floor { // an index is whole or gone
			drop++
		}
		c.log = slices.Delete(c.log, 0, drop)
	}
}

// reset empties the feed, complete from index on (a Restore).
func (c *changeFeed) reset(index uint64) {
	c.floor, c.log = index, nil
}

// since lists the distinct inodes changed after index after, in inode order; false when
// the feed does not reach back to it.
func (c *changeFeed) since(after uint64) ([]Ino, bool) {
	if after < c.floor {
		return nil, false
	}
	i := sort.Search(len(c.log), func(i int) bool { return c.log[i].index > after })
	out := make([]Ino, 0, len(c.log)-i)
	for _, e := range c.log[i:] {
		out = append(out, e.ino)
	}
	slices.Sort(out)
	return slices.Compact(out), true
}

// Changes lists the inodes created, changed or deleted by the entries after index after,
// up to upto, the applied index the answer runs to: O(the changes), never a walk
// (docs/02 §The change feed, I-386). complete is false when the feed does not reach back
// to after (a node started from a snapshot, an overflow, an index not applied yet): the
// caller walks once and continues from upto. A changed directory stands for itself; a
// caller that cares about what lies under it visits that subtree.
func (f *FSM) Changes(after uint64) (inos []Ino, upto uint64, complete bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	upto = f.st.Applied
	if after > upto {
		return nil, upto, false
	}
	inos, complete = f.feed.since(after)
	return inos, upto, complete
}

// touch records ino as changed by the entry being applied.
func (a *applier) touch(inos ...Ino) {
	for _, ino := range inos {
		a.feed.add(a.index, ino)
	}
}
