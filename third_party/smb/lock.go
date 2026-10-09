// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// Byte-range locks are what an application uses to say "this part of the file
// is mine for a moment". A database, a spreadsheet, a mail store: every one of
// them takes them, and a server that answers NOT_IMPLEMENTED gets either an
// application that refuses to open the file or -- worse -- one that carries on
// believing it has exclusive access it does not have.
//
// The locks live with the SHARE, not with the connection, because that is the
// point of them: they are how two clients keep out of each other's way.

// Lock element flags.
const (
	lockShared          uint32 = 0x00000001
	lockExclusive       uint32 = 0x00000002
	lockUnlock          uint32 = 0x00000004
	lockFailImmediately uint32 = 0x00000010
)

// A byteLock is one range held by one open handle.
type byteLock struct {
	path      string
	offset    uint64
	length    uint64
	exclusive bool
	owner     [16]byte // the file id that took it
}

// overlaps reports whether two ranges touch. A length of zero locks nothing,
// which is not the same as locking everything.
func (l byteLock) overlaps(path string, off, length uint64) bool {
	if l.path != path || l.length == 0 || length == 0 {
		return false
	}
	return off < l.offset+l.length && l.offset < off+length
}

// lockTable is the set of ranges held on one share.
type lockTable struct {
	mu    sync.Mutex
	locks []byteLock
}

// take grants a range or says why not.
//
// Shared locks coexist with each other; an exclusive one excludes everything.
// A handle never conflicts with ITSELF: an application that locks a range it
// already holds is extending its own reservation, not fighting for it.
func (t *lockTable) take(l byteLock) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, held := range t.locks {
		if held.owner == l.owner || !held.overlaps(l.path, l.offset, l.length) {
			continue
		}
		if held.exclusive || l.exclusive {
			return false
		}
	}
	t.locks = append(t.locks, l)
	return true
}

// release drops one range held by this handle. Unlocking a range nobody holds
// is an error a client is told about: it means the two sides disagree about
// what is locked.
func (t *lockTable) release(l byteLock) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, held := range t.locks {
		if held.owner == l.owner && held.path == l.path &&
			held.offset == l.offset && held.length == l.length {
			t.locks = append(t.locks[:i], t.locks[i+1:]...)
			return true
		}
	}
	return false
}

// releaseAll drops everything a handle held. It runs when the handle closes AND
// when the connection drops: a client that crashes holding a lock must not
// keep the file reserved for the life of the server.
func (t *lockTable) releaseAll(owner [16]byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.locks[:0]
	for _, held := range t.locks {
		if held.owner != owner {
			kept = append(kept, held)
		}
	}
	t.locks = kept
}

// held reports whether a range is locked against a given handle, which is what
// a read or a write has to ask before touching those bytes.
func (t *lockTable) held(path string, off, length uint64, by [16]byte, writing bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range t.locks {
		if l.owner == by || !l.overlaps(path, off, length) {
			continue
		}
		if l.exclusive || writing {
			return true
		}
	}
	return false
}

// A lockElement is one range in a request, and what to do with it.
type lockElement struct {
	l      byteLock
	unlock bool
}

// lock answers an SMB2 LOCK request.
func (c *conn) lock(h header, body []byte) ([]byte, error) {
	if len(body) < 24 {
		return nil, fmt.Errorf("smb: LOCK body of %d bytes is too short", len(body))
	}
	count := int(binary.LittleEndian.Uint16(body[2:]))
	of := c.fileByID(body[8:])
	if of == nil {
		return errorResponse(h, statusFileClosed), nil
	}
	if count == 0 || 24+count*24 > len(body) {
		return errorResponse(h, statusInvalidParameter), nil
	}

	// The elements are applied together or not at all: a client that asked for
	// three ranges and got two would have no way to know which.
	elements := make([]lockElement, 0, count)
	waiting := false
	for i := 0; i < count; i++ {
		e := body[24+i*24:]
		flags := binary.LittleEndian.Uint32(e[16:])
		elements = append(elements, lockElement{
			l: byteLock{
				path:      of.path,
				offset:    binary.LittleEndian.Uint64(e[0:]),
				length:    binary.LittleEndian.Uint64(e[8:]),
				exclusive: flags&lockExclusive != 0,
				owner:     of.id,
			},
			unlock: flags&lockUnlock != 0,
		})
		if flags&(lockUnlock|lockFailImmediately) == 0 {
			waiting = true
		}
	}

	table := &of.share.locks
	var taken []byteLock
	for _, e := range elements {
		if e.unlock {
			if !table.release(e.l) {
				undo(table, taken)
				// RANGE_NOT_LOCKED, because the two sides disagree about what
				// is held, and a client that is told "denied" would retry.
				return errorResponse(h, statusRangeNotLocked), nil
			}
			continue
		}
		if !table.take(e.l) {
			undo(table, taken)
			if !waiting {
				return errorResponse(h, statusLockNotGranted), nil
			}
			// The client asked to WAIT. It is promised an answer, the loop
			// carries on reading -- which is what lets the holder release the
			// lock, and lets this client cancel -- and the answer is sent
			// when one of those happens.
			return c.waitForLocks(h, of, elements), nil
		}
		taken = append(taken, e.l)
	}
	return simpleResponse(h, statusSuccess, 4), nil
}

// waitForLocks answers a lock request that cannot be granted yet, later.
//
// It retries rather than being woken precisely, because the thing it is
// waiting for is another client's release and there is no queue of waiters:
// polling a few times a second costs nothing on an idle share and cannot
// deadlock, which a hand-rolled wakeup between two clients easily can.
func (c *conn) waitForLocks(h header, of *openFile, elements []lockElement) []byte {
	op, interim := c.beginAsync(h, of.id)
	c.mu.Lock()
	c.asyncSession = h.sessionID
	c.mu.Unlock()

	go func() {
		const (
			every = 25 * time.Millisecond
			// A lock nobody releases is not an error, so there is no timeout
			// here: the client gave no deadline and CANCEL is how it takes
			// one. The wait ends when the connection does.
			forever = 0
		)
		_ = forever
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-op.cancel:
				return // whoever cancelled has answered
			case <-ticker.C:
				var taken []byteLock
				ok := true
				for _, e := range elements {
					if e.unlock {
						continue
					}
					if !of.share.locks.take(e.l) {
						ok = false
						break
					}
					taken = append(taken, e.l)
				}
				if !ok {
					undo(&of.share.locks, taken)
					continue
				}
				reply := asyncResponse(h, statusSuccess, op.asyncID, 4)
				binary.LittleEndian.PutUint16(reply[headerLen:], 4)
				c.finishAsync(op, reply)
				return
			}
		}
	}()
	return interim
}

// undo gives back the ranges taken by a request that then failed.
func undo(t *lockTable, taken []byteLock) {
	for _, l := range taken {
		t.release(l)
	}
}
