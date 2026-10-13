package rpctransports

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The length-prefixed framing of the stream protocols (mux, quic): a request is an op
// byte, a 4-byte length and the body; a reply is a 4-byte length and the body. One
// request per stream, so the stream's own end and reset carry completion and abort.
const (
	opPut   = 'P'
	opGet   = 'G'
	opApply = 'A'
)

var (
	getPayload  = bytes.Repeat([]byte{0x5a}, 32<<10)
	applyReply  = bytes.Repeat([]byte{'r'}, 100)
	errBadFrame = errors.New("bad frame")
)

func writeRequest(w io.Writer, op byte, body []byte) error {
	var h [5]byte
	h[0] = op
	binary.BigEndian.PutUint32(h[1:], uint32(len(body)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// readReply drains the reply.
func readReply(r io.Reader) error {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return err
	}
	n, err := io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(h[:])))
	if err != nil {
		return fmt.Errorf("reply cut at %d: %w", n, err)
	}
	return nil
}

// serveFrame answers one request on rw.
func serveFrame(rw io.ReadWriter) error {
	var h [5]byte
	if _, err := io.ReadFull(rw, h[:]); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, rw, int64(binary.BigEndian.Uint32(h[1:]))); err != nil {
		return err
	}
	var out [4]byte
	switch h[0] {
	case opPut:
		binary.BigEndian.PutUint32(out[:], 0)
		_, err := rw.Write(out[:])
		return err
	case opApply:
		binary.BigEndian.PutUint32(out[:], uint32(len(applyReply)))
		if _, err := rw.Write(out[:]); err != nil {
			return err
		}
		_, err := rw.Write(applyReply)
		return err
	case opGet:
		binary.BigEndian.PutUint32(out[:], shardSize)
		if _, err := rw.Write(out[:]); err != nil {
			return err
		}
		for left := shardSize; left > 0; {
			n := min(left, len(getPayload))
			if _, err := rw.Write(getPayload[:n]); err != nil {
				return err
			}
			left -= n
		}
		return nil
	}
	return errBadFrame
}
