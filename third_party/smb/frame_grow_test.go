// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"testing/iotest"
)

// A frame of any size comes back whole, however the bytes arrive, and a
// frame cut short is an error.
func TestReadFrameGrowsToTheMessage(t *testing.T) {
	for _, n := range []int{1, frameFirstRead - 1, frameFirstRead, frameFirstRead + 1, 3<<20 + 7, maxMessageLen} {
		body := make([]byte, n)
		for i := range body {
			body[i] = byte(i*31 + n)
		}
		var frame bytes.Buffer
		binary.Write(&frame, binary.BigEndian, uint32(n))
		frame.Write(body)
		for name, r := range map[string]io.Reader{
			"whole":    bytes.NewReader(frame.Bytes()),
			"by bytes": iotest.OneByteReader(bytes.NewReader(frame.Bytes())),
		} {
			if name == "by bytes" && n > frameFirstRead+1 {
				continue // slow, and no different past the first growth
			}
			got, err := readFrame(r)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("%d bytes, %s: got %d bytes, %v", n, name, len(got), err)
			}
		}
		if n > 1 {
			if _, err := readFrame(bytes.NewReader(frame.Bytes()[:frame.Len()-1])); err == nil {
				t.Fatalf("%d bytes, one short: no error", n)
			}
		}
	}
}
