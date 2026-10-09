// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"sync"
)

// Some requests cannot be answered when they arrive, and must not be refused
// either: a client waiting for a byte-range lock, or asking to be told when a
// directory changes, is asking the server to answer LATER.
//
// SMB2 says how. The server sends an interim reply with STATUS_PENDING and an
// AsyncId, keeps reading, and sends the real reply -- same MessageId, same
// AsyncId -- whenever it is ready. The client may send CANCEL with that
// AsyncId to give up first.
//
// The consequence for this server is that a reply can now be written by a
// goroutine that is not the one reading, so writes are serialised and the
// per-connection state is locked. Before this, the loop read one message,
// answered it, and read the next: everything was serial by construction.

// The async header differs from the sync one in a single place: the eight
// bytes that hold Reserved and TreeId are one AsyncId instead.
const offAsyncID = 32

// A pendingOp is a request the server has promised to answer later.
type pendingOp struct {
	command   command
	messageID uint64
	asyncID   uint64
	file      [16]byte // the handle it belongs to, so closing that handle ends it
	cancel    chan struct{}
	once      sync.Once
}

// stop wakes whatever is waiting on this operation. It is safe to call twice,
// because a cancel and a completion can race and both are legitimate.
func (p *pendingOp) stop() { p.once.Do(func() { close(p.cancel) }) }

// beginAsync registers an operation and returns the interim reply to send.
func (c *conn) beginAsync(h header, file [16]byte) (*pendingOp, []byte) {
	c.mu.Lock()
	c.nextAsync++
	op := &pendingOp{
		command:   h.command,
		messageID: h.messageID,
		asyncID:   c.nextAsync,
		file:      file,
		cancel:    make(chan struct{}),
	}
	c.waiting[op.asyncID] = op
	c.mu.Unlock()

	// STATUS_PENDING with the standard nine-byte error body: this is not the
	// answer, it is the promise of one.
	b := asyncResponse(h, statusPending, op.asyncID, 9)
	binary.LittleEndian.PutUint16(b[headerLen:], 9)
	return op, b
}

// finishAsync sends the real reply and forgets the operation.
func (c *conn) finishAsync(op *pendingOp, reply []byte) {
	c.mu.Lock()
	_, live := c.waiting[op.asyncID]
	delete(c.waiting, op.asyncID)
	sess := c.sessions[c.asyncSession]
	c.mu.Unlock()
	if !live {
		// Cancelled, and already answered by whoever cancelled it.
		return
	}
	if sess != nil {
		signMessage(c.dialect, sess.signingKey, reply)
	}
	c.send(reply)
}

// asyncResponse builds the header of a reply that carries an AsyncId.
func asyncResponse(req header, status uint32, asyncID uint64, bodyLen int) []byte {
	b := append(responseTo(req, status), make([]byte, bodyLen)...)
	flags := binary.LittleEndian.Uint32(b[offFlags:]) | flagAsyncCommand
	binary.LittleEndian.PutUint32(b[offFlags:], flags)
	binary.LittleEndian.PutUint64(b[offAsyncID:], asyncID)
	return b
}

// write sends one frame, from whichever goroutine has one to send.
func (c *conn) send(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c.nc, b)
}

// cancel answers an SMB2 CANCEL: the client has given up on an operation.
//
// A cancelled operation is answered with STATUS_CANCELLED rather than left
// unanswered -- a client that is told nothing keeps the message id reserved
// and, worse, keeps waiting.
func (c *conn) cancel(h header) ([]byte, error) {
	c.mu.Lock()
	var found *pendingOp
	for _, op := range c.waiting {
		// A cancel names the operation by its AsyncId when the interim reply
		// carried one, and by its MessageId otherwise.
		if (h.flags&flagAsyncCommand != 0 && op.asyncID == h.asyncID) ||
			(h.flags&flagAsyncCommand == 0 && op.messageID == h.messageID) {
			found = op
			break
		}
	}
	if found != nil {
		delete(c.waiting, found.asyncID)
	}
	sess := c.sessions[h.sessionID]
	c.mu.Unlock()

	if found == nil {
		// Nothing to cancel: the operation finished on its own between the
		// client deciding and the message arriving. Saying nothing is right --
		// the answer it wanted has already been sent.
		return nil, nil
	}
	found.stop()
	reply := asyncResponse(header{command: found.commandOf(), messageID: found.messageID},
		statusCancelled, found.asyncID, 9)
	binary.LittleEndian.PutUint16(reply[headerLen:], 9)
	if sess != nil {
		signMessage(c.dialect, sess.signingKey, reply)
	}
	return nil, c.send(reply)
}

// commandOf is what the cancelled operation was. It is kept on the operation
// because the cancel message does not carry it.
func (p *pendingOp) commandOf() command { return p.command }

// cancelForFile ends every operation waiting on one handle. It runs when the
// handle closes and when the connection drops: an operation waiting on a file
// nobody has open any more will never complete on its own.
func (c *conn) cancelForFile(id [16]byte) {
	c.mu.Lock()
	var ended []*pendingOp
	for key, op := range c.waiting {
		if op.file == id {
			ended = append(ended, op)
			delete(c.waiting, key)
		}
	}
	c.mu.Unlock()
	for _, op := range ended {
		op.stop()
	}
}
