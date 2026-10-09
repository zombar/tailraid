// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// A pipe is what a client opens on IPC$ when it has a question no filesystem
// can answer. There is one here, \srvsvc, and one question: what shares are
// there. See srvsvc.go for the three formats that answer it.
//
// It is a handle like any other -- CREATE, WRITE, READ, IOCTL, CLOSE -- and
// that is the point: the remote procedure call rides on the file commands
// already implemented, so the only new thing is what the bytes mean.
type pipe struct {
	name string
	// bound records that the client named the interface it wants. A call
	// before a bind is a protocol error rather than a missing feature, and
	// saying which is what lets a client fix its own sequence.
	bound bool
	// maxFrag is how much the client said it can receive at once, from the
	// bind. Replies are cut to it.
	maxFrag int
	// out is what is still to be handed over: one entry per DCE/RPC fragment,
	// the first one partly taken already. FSCTL_PIPE_TRANSCEIVE answers
	// BUFFER_OVERFLOW with as much as fits and the client READs the rest --
	// from wherever the pipe is, because a pipe has no offsets. A read at 0 is
	// not a read of the beginning.
	//
	// A read never crosses a fragment boundary: the client expects the next
	// one to start with a header.
	out [][]byte
}

// pipeStat is what a pipe looks like to code that expects a file: a regular,
// writable, empty one.
//
// A synthetic Stat rather than a special case in every information class: the
// alternative is a nil Stat travelling into attributesOf, and IPC$ has no
// filesystem to ask.
var pipeStat = filesystem.NewStat(sIFREG|0o666, 0, 0)

// openPipe answers CREATE on IPC$.
func (c *conn) openPipe(h header, name string) ([]byte, error) {
	name = strings.TrimPrefix(name, `\`)
	if !strings.EqualFold(name, "srvsvc") {
		// wkssvc, lsarpc, samr, spoolss, netlogon: a client asks for whichever
		// it wants and carries on without the ones that are not there. Naming
		// the refusal is what makes it carry on rather than retry.
		return errorResponse(h, statusObjectNameNotFound), nil
	}
	of := &openFile{path: name, share: c.tree(h).sh, pipe: &pipe{name: name}}
	c.nextFile++
	binary.LittleEndian.PutUint64(of.id[:8], c.nextFile)
	binary.LittleEndian.PutUint64(of.id[8:], uint64(h.treeID))
	c.files[of.id] = of
	c.lastFile = of.id

	const bodyLen = 88
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen+1)...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 89)
	binary.LittleEndian.PutUint32(rb[4:], actionOpened)
	writeTimes(rb[8:], pipeStat)
	binary.LittleEndian.PutUint32(rb[56:], attrNormal)
	copy(rb[64:], of.id[:])
	return b, nil
}

// answer takes one DCE/RPC packet and produces the whole reply.
//
// Nothing here is deferred or asynchronous: the answer is a list this server
// already has in memory. A real Windows pipe can leave a call outstanding;
// this one cannot, which is why there is no queue.
func (p *pipe) answer(srv *Server, user string, in []byte) [][]byte {
	call, ok := parseRPC(in)
	if !ok {
		return nil
	}
	switch call.kind {
	case rpcTypeBind:
		p.bound = true
		p.maxFrag = int(call.maxRecv)
		// The fragment sizes are the client's own, echoed: it offered them and
		// a server may only lower them. What matters is that the RECEIVE size
		// is remembered, because it is the size every reply is cut to.
		return [][]byte{bindAck(call.callID, call.maxXmit, call.maxRecv)}

	case rpcTypeRequest:
		if !p.bound {
			return [][]byte{rpcFault(call.callID, faultProtoErr)}
		}
		if call.opnum != opNetShareEnum {
			return [][]byte{rpcFault(call.callID, faultOpRange)}
		}
		level, ok := netShareEnumLevel(call.stub)
		if !ok {
			return [][]byte{rpcFault(call.callID, faultProtoErr)}
		}
		return rpcResponse(call.callID, netShareEnumStub(level, srv.sharesFor(user)), p.maxFrag)

	default:
		return [][]byte{rpcFault(call.callID, faultProtoErr)}
	}
}

// take hands back up to n bytes and keeps the rest, without crossing a
// fragment boundary.
func (p *pipe) take(n int) (out []byte, more bool) {
	if n < 0 {
		n = 0
	}
	if len(p.out) == 0 {
		return nil, false
	}
	head := p.out[0]
	if len(head) <= n {
		p.out = p.out[1:]
		return head, len(p.out) > 0
	}
	p.out[0] = head[n:]
	return head[:n], true
}

// writePipe is a client putting a call INTO the pipe. The answer is computed
// now and waits for the READ that always follows: a client that writes a
// request and never reads it gets no reply, which is exactly what a pipe does.
func (c *conn) writePipe(h header, of *openFile, data []byte) []byte {
	of.pipe.out = of.pipe.answer(c.srv, c.session(h).user, data)
	const bodyLen = 16
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen)...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 17)
	binary.LittleEndian.PutUint32(rb[4:], uint32(len(data)))
	return b
}

// readPipe drains what is waiting.
func (c *conn) readPipe(h header, of *openFile, length int) []byte {
	if len(of.pipe.out) == 0 {
		// There is nothing in flight. A real pipe would wait for the other
		// end; here there is no other end, and END_OF_FILE is what stops a
		// client reading forever.
		return errorResponse(h, statusEndOfFile)
	}
	out, _ := of.pipe.take(length)
	const bodyLen = 16
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen+len(out))...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 17)
	rb[2] = headerLen + bodyLen
	binary.LittleEndian.PutUint32(rb[4:], uint32(len(out)))
	copy(rb[bodyLen:], out)
	return b
}
