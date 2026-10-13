package smb

import (
	"encoding/binary"
	"testing"
)

// notifyOn sends a CHANGE_NOTIFY on handle dir and returns what the read loop answered.
func notifyOn(t *testing.T, c *conn, dir *openFile, msgID uint64) []byte {
	t.Helper()
	body := make([]byte, 32)
	binary.LittleEndian.PutUint32(body[4:], 4096)
	copy(body[8:], dir.id[:])
	msg := request(cmdChangeNotify, 1, body)
	binary.LittleEndian.PutUint64(msg[offMessageID:], msgID)
	out, err := c.dispatch(msg)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// writeThrough makes a change on the share as a WRITE through this server does.
func writeThrough(t *testing.T, c *conn, dir *openFile, name string, idByte byte) {
	t.Helper()
	w := &openFile{path: "/" + name, share: dir.share}
	w.id[0] = idByte
	c.files[w.id] = w
	wb := make([]byte, 48+5)
	binary.LittleEndian.PutUint16(wb[2:], uint16(headerLen+48))
	binary.LittleEndian.PutUint32(wb[4:], 5)
	copy(wb[16:], w.id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdWrite, wb)); st != statusSuccess {
		t.Fatal("the write failed")
	}
}

func notifiedNames(t *testing.T, reply []byte) []string {
	t.Helper()
	out := reply[headerLen+8:]
	var names []string
	for {
		n := int(binary.LittleEndian.Uint32(out[8:]))
		names = append(names, fromUTF16le(out[12:12+n]))
		next := int(binary.LittleEndian.Uint32(out[0:]))
		if next == 0 {
			return names
		}
		out = out[next:]
	}
}

// The watch belongs to the open, not to one request (MS-FSA 2.1.5.10): a change that
// happens after a notification was answered and before the client asks again is kept,
// and the next CHANGE_NOTIFY is answered with it at once. Before, the watch lived only as
// long as one request, so every change in that gap was lost and Explorer's window stayed
// stale through a copy.
// Invariant: I-264
func TestChangeNotify_WatchOutlivesTheRequest(t *testing.T) {
	c, dir := opened(t, &tinyFS{body: []byte("hello")}, "/", false)
	c.nc = &captureConn{}
	if h, _ := parseHeader(notifyOn(t, c, dir, 1)); h.status != statusPending {
		t.Fatalf("the first CHANGE_NOTIFY answered %#x, want STATUS_PENDING", h.status)
	}
	writeThrough(t, c, dir, "file.txt", 7)
	if names := notifiedNames(t, (c.nc.(*captureConn)).await(t)); len(names) != 1 || names[0] != "file.txt" {
		t.Fatalf("first notification %v", names)
	}
	// in the gap before the client asks again
	writeThrough(t, c, dir, "file.txt", 8)
	out := notifyOn(t, c, dir, 2)
	h, _ := parseHeader(out)
	if h.status != statusSuccess {
		t.Fatalf("a CHANGE_NOTIFY with a change already kept answered %#x, want it at once", h.status)
	}
	if names := notifiedNames(t, out); len(names) != 1 || names[0] != "file.txt" {
		t.Fatalf("the change made between the requests: %v, want [file.txt]", names)
	}
}

// Closing a handle answers what still waits on it: a CHANGE_NOTIFY with
// STATUS_NOTIFY_CLEANUP (MS-SMB2 3.3.5.10), a byte-range lock wait with
// STATUS_CANCELLED; and the watch ends with the handle. Before, both were stopped without
// an answer, leaving the client's request and its credits outstanding.
// Invariant: I-264
func TestClose_AnswersWhatWaitsOnTheHandle(t *testing.T) {
	c, dir := opened(t, &tinyFS{body: []byte("hello")}, "/", false)
	c.nc = &captureConn{}
	interim, _ := parseHeader(notifyOn(t, c, dir, 3))
	closeBody := make([]byte, 24)
	copy(closeBody[8:], dir.id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdClose, closeBody)); st != statusSuccess {
		t.Fatal("closing failed")
	}
	final, _ := parseHeader((c.nc.(*captureConn)).await(t))
	if final.status != statusNotifyCleanup || final.asyncID != interim.asyncID {
		t.Fatalf("the pending CHANGE_NOTIFY was answered %#x under %d, want NOTIFY_CLEANUP under %d", final.status, final.asyncID, interim.asyncID)
	}
	if n := dir.share.watchers.count(); n != 0 {
		t.Fatalf("%d watches outlived their handle", n)
	}

	// a lock wait on a closing handle
	c2, f := opened(t, &tinyFS{body: []byte("hello")}, "/file.txt", false)
	c2.nc = &captureConn{}
	holder := &openFile{path: "/file.txt", share: f.share}
	holder.id[0] = 9
	c2.files[holder.id] = holder
	lock := func(of *openFile, flags uint32) []byte {
		b := make([]byte, 48)
		binary.LittleEndian.PutUint16(b[2:], 1)
		copy(b[8:], of.id[:])
		binary.LittleEndian.PutUint64(b[32:], 100)
		binary.LittleEndian.PutUint32(b[40:], flags)
		return mustDispatch(t, c2, cmdLock, b)
	}
	if st := statusOf(t, lock(holder, lockExclusive|lockFailImmediately)); st != statusSuccess {
		t.Fatal("the holder did not get the lock")
	}
	waiting, _ := parseHeader(lock(f, lockExclusive))
	if waiting.status != statusPending {
		t.Fatalf("the waiter answered %#x, want STATUS_PENDING", waiting.status)
	}
	cb := make([]byte, 24)
	copy(cb[8:], f.id[:])
	if st := statusOf(t, mustDispatch(t, c2, cmdClose, cb)); st != statusSuccess {
		t.Fatal("closing the waiter failed")
	}
	final, _ = parseHeader((c2.nc.(*captureConn)).await(t))
	if final.status != statusCancelled || final.asyncID != waiting.asyncID {
		t.Fatalf("the lock wait was answered %#x under %d, want STATUS_CANCELLED under %d", final.status, final.asyncID, waiting.asyncID)
	}
}
