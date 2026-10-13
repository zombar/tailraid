// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"
)

// CHANGE_NOTIFY is how a file manager keeps its window right: it asks to be
// told when a directory changes instead of asking again every second. Refusing
// it works -- a client falls back to polling -- but the window is then wrong
// for as long as the poll interval, and the polling costs a listing each time.
//
// ⛔ WHAT THIS CAN AND CANNOT SEE. The changes reported are the ones that go
// THROUGH THIS SERVER. A file written into the image by something else -- the
// program that made it, another server on the same image, the driver's own API
// -- is invisible here, because nothing underneath tells us. Watching the
// image itself would mean a filesystem-level notification the drivers do not
// have. A client that must not miss those has to keep polling, and this is
// stated rather than left to be discovered.

// What a client asks to be told about. Only the flag that changes behaviour is
// named: the rest of the filter is honoured by reporting everything, which is
// what a server may do.
const watchTree uint16 = 0x0001

// The actions a notification carries.
const (
	actionAdded    uint32 = 0x00000001
	actionRemoved  uint32 = 0x00000002
	actionModified uint32 = 0x00000003
)

// A change is one thing that happened, as a client wants to hear it: the name
// relative to the directory being watched.
type change struct {
	path   string // the full path inside the share
	action uint32
}

// A watcher is the watch one open directory handle keeps (MS-FSA 2.1.5.10): made by its
// first CHANGE_NOTIFY and kept until CLOSE, it collects changes between requests, so one
// that happens after a notification was answered and before the client asks again is
// not lost (I-264). Before, it lived only as long as one request.
type watcher struct {
	dir      string
	tree     bool
	events   chan change
	overflow int32 // 1 once a change found the queue full: the next answer is "look again" (atomic)
}

// watchers is the set of them on one share.
type watchers struct {
	mu   sync.Mutex
	list []*watcher
}

func (w *watchers) add(x *watcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = append(w.list, x)
}

func (w *watchers) remove(x *watcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, c := range w.list {
		if c == x {
			w.list = append(w.list[:i], w.list[i+1:]...)
			return
		}
	}
}

// count is how many watches are outstanding. It exists because a test wants
// to know, and reaching into the slice to find out is exactly the race the
// mutex is there to prevent -- one the detector duly found.
func (w *watchers) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.list)
}

// post tells every watcher that cares. It never blocks: a watcher whose queue
// is full is left full, and the reply it eventually sends says "too much
// changed, look again" rather than a list with holes in it.
func (w *watchers) post(c change) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, x := range w.list {
		if !x.covers(c.path) {
			continue
		}
		select {
		case x.events <- c:
		default:
			atomic.StoreInt32(&x.overflow, 1)
		}
	}
}

// covers reports whether a change is one this watcher asked about: in the
// directory itself, or anywhere below it when the client asked to watch the
// tree.
func (x *watcher) covers(p string) bool {
	dir := path.Dir(p)
	if dir == x.dir {
		return true
	}
	return x.tree && strings.HasPrefix(dir+"/", strings.TrimSuffix(x.dir, "/")+"/")
}

// notify records a change, if anybody asked. It is called from the handlers
// that make one, while they hold the share's lock.
func (s *share) notify(p string, action uint32) {
	s.watchers.post(change{path: p, action: action})
}

// changeNotify answers an SMB2 CHANGE_NOTIFY: never at once, because there is
// nothing to say yet.
func (c *conn) changeNotify(h header, body []byte) ([]byte, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("smb: CHANGE_NOTIFY body of %d bytes is too short", len(body))
	}
	flags := binary.LittleEndian.Uint16(body[2:])
	outMax := int(binary.LittleEndian.Uint32(body[4:]))
	of := c.fileByID(body[8:])
	if of == nil {
		return errorResponse(h, statusFileClosed), nil
	}
	if !of.dir {
		return errorResponse(h, statusInvalidParameter), nil
	}

	// The watch is the handle's: made by its first CHANGE_NOTIFY, kept until CLOSE. A
	// queue of 64 holds a window's worth; a burst past it overflows and says so instead of
	// queueing megabytes nobody will read.
	x := of.watch
	if x == nil {
		x = &watcher{dir: of.p(), tree: flags&watchTree != 0, events: make(chan change, 64)}
		of.watch = x
		of.share.watchers.add(x)
	}
	// changes kept since the last answer are answered at once
	select {
	case first := <-x.events:
		status, out := notifyBody(x, first, outMax)
		reply := append(responseTo(h, status), make([]byte, 8+len(out))...)
		putNotify(reply, out)
		return reply, nil
	default:
	}
	if atomic.SwapInt32(&x.overflow, 0) == 1 {
		x.drain()
		reply := append(responseTo(h, statusNotifyEnumDir), make([]byte, 8)...)
		putNotify(reply, nil)
		return reply, nil
	}

	op, interim := c.beginAsync(h, of.id)

	go func() {
		select {
		case <-op.cancel:
			// The client gave up (answered CANCELLED) or the handle closed (answered
			// NOTIFY_CLEANUP): whoever ended it has answered.
			return
		case first := <-x.events:
			status, out := notifyBody(x, first, outMax)
			reply := asyncResponse(h, status, op.asyncID, 8+len(out))
			putNotify(reply, out)
			c.finishAsync(op, reply)
		}
	}()
	return interim, nil
}

// notifyBody is the status and entries of an answer that starts with first: the changes
// queued, or NOTIFY_ENUM_DIR ("too much changed, look again") when they overflowed the
// queue or the client's buffer; a list with holes in it would be worse than no list.
func notifyBody(x *watcher, first change, outMax int) (uint32, []byte) {
	out, overflowed := encodeChanges(x, first, outMax)
	if atomic.SwapInt32(&x.overflow, 0) == 1 || overflowed {
		x.drain() // the client looks again: what is still queued is in what it will read
		return statusNotifyEnumDir, nil
	}
	return statusSuccess, out
}

// drain drops the changes queued on the watch.
func (x *watcher) drain() {
	for {
		select {
		case <-x.events:
		default:
			return
		}
	}
}

// putNotify writes a CHANGE_NOTIFY reply's body: its size, the buffer's offset and length.
func putNotify(reply, out []byte) {
	rb := reply[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 9)
	binary.LittleEndian.PutUint16(rb[2:], headerLen+8)
	binary.LittleEndian.PutUint32(rb[4:], uint32(len(out)))
	copy(rb[8:], out)
}

// encodeChanges lays out what happened, starting with the one that woke the
// watcher and taking whatever else is already queued.
func encodeChanges(x *watcher, first change, outMax int) (out []byte, overflowed bool) {
	queued := []change{first}
	for {
		select {
		case c := <-x.events:
			queued = append(queued, c)
			continue
		default:
		}
		break
	}
	lastStart := -1
	for _, c := range queued {
		name := utf16le(strings.TrimPrefix(strings.TrimPrefix(c.path, x.dir), "/"))
		entry := make([]byte, (12+len(name)+3)/4*4)
		binary.LittleEndian.PutUint32(entry[4:], c.action)
		binary.LittleEndian.PutUint32(entry[8:], uint32(len(name)))
		copy(entry[12:], name)
		if outMax > 0 && len(out)+len(entry) > outMax {
			return nil, true
		}
		if lastStart >= 0 {
			binary.LittleEndian.PutUint32(out[lastStart:], uint32(len(out)-lastStart))
		}
		lastStart = len(out)
		out = append(out, entry...)
	}
	return out, false
}
