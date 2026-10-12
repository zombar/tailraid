// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
)

// The request trace (Server.SetLogger): what a client asked of a file and what it was
// told, one Debug line per CREATE, SET_INFO and CLOSE. A copy that goes wrong is read
// back from it: which handle marked the file delete-on-close, which cleared it, which
// closed first.

// handleName is a handle as the trace prints it: this connection's counter, which is
// what a client's handles of one file are told apart by in a log.
func handleName(id []byte) string {
	if len(id) < 8 {
		return "-"
	}
	return fmt.Sprintf("%d", binary.LittleEndian.Uint64(id))
}

func replyStatus(b []byte) uint32 {
	if len(b) < offStatus+4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b[offStatus:])
}

// traceCreate runs a CREATE and traces it.
func (c *conn) traceCreate(h header, body, msg []byte) ([]byte, error) {
	b, err := c.create(h, body, msg)
	if c.srv.log == nil || len(body) < 56 {
		return b, err
	}
	nameOff := int(binary.LittleEndian.Uint16(body[44:]))
	nameLen := int(binary.LittleEndian.Uint16(body[46:]))
	name := "?"
	if nameOff >= 0 && nameLen >= 0 && nameOff+nameLen <= len(msg) {
		name = fromUTF16le(msg[nameOff : nameOff+nameLen])
	}
	args := []any{"session", h.sessionID, "name", name,
		"access", fmt.Sprintf("%#x", binary.LittleEndian.Uint32(body[24:])),
		"share", fmt.Sprintf("%#x", binary.LittleEndian.Uint32(body[32:])),
		"disposition", binary.LittleEndian.Uint32(body[36:]),
		"options", fmt.Sprintf("%#x", binary.LittleEndian.Uint32(body[40:])),
		"status", fmt.Sprintf("%#x", replyStatus(b))}
	if replyStatus(b) == statusSuccess && len(b) >= headerLen+80 {
		rb := b[headerLen:]
		args = append(args, "action", binary.LittleEndian.Uint32(rb[4:]), "handle", handleName(rb[64:80]))
		if of := c.files[[16]byte(rb[64:80])]; of != nil {
			args = append(args, "ino", of.ino, "created", of.created, "delete_on_close", of.deleteOnClose)
		}
	}
	c.trace("create", args...)
	return b, err
}

// traceSetInfo runs a SET_INFO and traces it, with the disposition it set.
func (c *conn) traceSetInfo(h header, body, msg []byte) ([]byte, error) {
	b, err := c.setInfo(h, body, msg)
	if c.srv.log == nil || len(body) < 32 {
		return b, err
	}
	args := []any{"session", h.sessionID, "handle", handleName(body[16:32]),
		"type", body[2], "class", body[3], "status", fmt.Sprintf("%#x", replyStatus(b))}
	inLen := int(binary.LittleEndian.Uint32(body[4:]))
	inOff := int(binary.LittleEndian.Uint16(body[8:]))
	if inOff >= 0 && inLen > 0 && inOff+inLen <= len(msg) {
		in := msg[inOff : inOff+inLen]
		args = append(args, "in", fmt.Sprintf("%x", in[:min(len(in), 8)]))
	}
	if of := c.fileByID(body[16:]); of != nil {
		args = append(args, "path", of.p(), "ino", of.ino, "delete_on_close", of.deleteOnClose)
	}
	c.trace("set_info", args...)
	return b, err
}

// traceClose traces a CLOSE before running it: the handle is gone after.
func (c *conn) traceClose(h header, body []byte) ([]byte, error) {
	if c.srv.log != nil && len(body) >= 24 {
		args := []any{"session", h.sessionID, "handle", handleName(body[8:24])}
		if of := c.fileByID(body[8:]); of != nil {
			args = append(args, "path", of.p(), "ino", of.ino, "created", of.created,
				"delete_on_close", of.deleteOnClose, "open_handles", of.share.openHandles(of.ino))
		}
		c.trace("close", args...)
	}
	return c.closeFile(h, body)
}

// traceOther runs one of the remaining file commands and traces its handle, its
// parameters and the status it answered (pending: the real reply follows, traced by
// writeAsync).
func (c *conn) traceOther(h header, body, msg []byte, run func() ([]byte, error)) ([]byte, error) {
	b, err := run()
	if c.srv.log == nil {
		return b, err
	}
	u32 := func(o int) uint32 {
		if len(body) < o+4 {
			return 0
		}
		return binary.LittleEndian.Uint32(body[o:])
	}
	u64 := func(o int) uint64 {
		if len(body) < o+8 {
			return 0
		}
		return binary.LittleEndian.Uint64(body[o:])
	}
	var name string
	var args []any
	fid := -1
	switch h.command {
	case cmdRead:
		name, fid = "read", 16
		args = []any{"len", u32(4), "off", u64(8)}
	case cmdWrite:
		name, fid = "write", 16
		args = []any{"len", u32(4), "off", u64(8)}
	case cmdFlush:
		name, fid = "flush", 8
	case cmdQueryInfo:
		name, fid = "query_info", 24
		if len(body) >= 4 {
			args = []any{"type", body[2], "class", body[3], "out_len", u32(4)}
		}
	case cmdIoctl:
		name, fid = "ioctl", 8
		args = []any{"ctl", fmt.Sprintf("%#x", u32(4)), "in_len", u32(28), "max_out", u32(44)}
		if o, n := int(u32(24)), int(u32(28)); n > 0 && o+n <= len(msg) {
			args = append(args, "in", fmt.Sprintf("%x", msg[o:o+n]))
		}
		if b != nil && len(b) > headerLen+48 {
			args = append(args, "out", fmt.Sprintf("%x", b[headerLen+48:]))
		}
	case cmdLock:
		name, fid = "lock", 8
		args = []any{"count", binary.LittleEndian.Uint16(append(body, 0, 0, 0, 0)[2:])}
	case cmdQueryDirectory:
		name, fid = "query_directory", 8
		if len(body) >= 4 {
			args = []any{"class", body[2], "flags", body[3]}
		}
	default:
		return b, err
	}
	args = append([]any{"session", h.sessionID, "msg", h.messageID}, args...)
	if fid >= 0 && len(body) >= fid+16 {
		args = append(args, "handle", handleName(body[fid:fid+16]))
		if of := c.fileByID(body[fid:]); of != nil {
			args = append(args, "path", of.p(), "ino", of.ino)
		}
	}
	if b == nil {
		args = append(args, "status", "pending")
	} else {
		args = append(args, "status", fmt.Sprintf("%#x", replyStatus(b)))
	}
	c.trace(name, args...)
	return b, err
}
