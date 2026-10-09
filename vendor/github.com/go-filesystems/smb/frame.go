// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"fmt"
	"io"
	"slices"
)

// SMB over TCP puts a four-byte header in front of every message: one byte of
// message type (zero, for a session message) and three of length. It is the
// NetBIOS session header with its name service left behind, which is why port
// 445 is called "direct hosting" -- and it is why a length is never more than
// 24 bits, however large the negotiated buffers are.
const (
	frameHeaderLen = 4
	maxFrameLen    = 1 << 24 // what three bytes of length can say

	// A message is refused above this, so a client cannot make the server
	// allocate 16 MiB by asking. It is comfortably larger than the largest
	// read or write this server advertises.
	maxMessageLen = 8 << 20
)

// readFrame returns one message body, without its length header.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != 0 {
		return nil, fmt.Errorf("smb: session message type %#02x is not a session message", hdr[0])
	}
	n := int(binary.BigEndian.Uint32(hdr[:]) & 0x00FFFFFF)
	if n == 0 {
		return nil, fmt.Errorf("smb: empty message")
	}
	if n > maxMessageLen {
		return nil, fmt.Errorf("smb: message of %d bytes is larger than the %d-byte limit", n, maxMessageLen)
	}
	// The buffer follows the bytes that arrive, doubling at most, rather than
	// the length the header announces: four bytes saying 8 MiB used to cost
	// 8 MiB at once, before any authentication, for as long as the client
	// then kept quiet.
	body := make([]byte, 0, min(n, frameFirstRead))
	for len(body) < n {
		if len(body) == cap(body) {
			body = slices.Grow(body, min(n-len(body), len(body)))
		}
		k := min(n, cap(body)) - len(body)
		start := len(body)
		body = body[:start+k]
		if _, err := io.ReadFull(r, body[start:]); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// frameFirstRead is what a frame's buffer starts at: every message but a
// large READ or WRITE fits.
const frameFirstRead = 64 << 10

// writeFrame writes one message body with its length header. The two are sent
// as ONE write: a client that reads the header and then blocks on a body that
// arrives in a second packet is a client waiting on Nagle.
func writeFrame(w io.Writer, body []byte) error {
	if len(body) >= maxFrameLen {
		return fmt.Errorf("smb: message of %d bytes does not fit a three-byte length", len(body))
	}
	buf := make([]byte, frameHeaderLen+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	buf[0] = 0
	copy(buf[frameHeaderLen:], body)
	_, err := w.Write(buf)
	return err
}
