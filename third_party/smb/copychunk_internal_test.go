// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// wfile is a File and WritableFile over a byte slice, with failures to inject.
type wfile struct {
	data                    []byte
	readErr, truncErr, sync error
}

func (f *wfile) ReadAt(p []byte, off int64) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (f *wfile) WriteAt(p []byte, off int64) (int, error) { return copy(f.data[off:], p), nil }
func (f *wfile) Size() int64                              { return int64(len(f.data)) }
func (f *wfile) Close() error                             { return nil }
func (f *wfile) Sync() error                              { return f.sync }
func (f *wfile) Truncate(n int64) error {
	if f.truncErr != nil {
		return f.truncErr
	}
	f.data = append(f.data[:min(n, int64(len(f.data))):min(n, int64(len(f.data)))], make([]byte, max(n-int64(len(f.data)), 0))...)
	return nil
}

var _ filesystem.WritableFile = (*wfile)(nil)

// pair is a connection with a source and a target open on one share, both
// opened by session 7, and the source's resume key.
func pair(t *testing.T, src, dst *wfile) (*conn, *openFile, *openFile, [24]byte) {
	t.Helper()
	c := newConn(New(), nil)
	sh := &share{name: "disk"}
	so := &openFile{path: "/src", share: sh, session: 7, f: src}
	do := &openFile{path: "/dst", share: sh, session: 7, f: dst, w: dst}
	so.id[0], do.id[0] = 1, 2
	c.files[so.id], c.files[do.id] = so, do
	out := c.resumeKey(header{sessionID: 7}, so)
	if st := statusOf(t, out); st != statusSuccess {
		t.Fatalf("resume key: status %#x", st)
	}
	var key [24]byte
	copy(key[:], out[headerLen+48:])
	if key != so.resumeKey {
		t.Fatal("the resume key in the reply is not the open's")
	}
	if again := c.resumeKey(header{sessionID: 7}, so); !bytes.Equal(again[headerLen+48:headerLen+72], key[:]) {
		t.Fatal("a second request gave another key")
	}
	return c, so, do, key
}

// chunks encodes SRV_COPYCHUNK_COPY.
func chunks(key [24]byte, ch ...[3]uint64) []byte {
	b := append(key[:0:0], key[:]...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(ch)))
	b = binary.LittleEndian.AppendUint32(b, 0)
	for _, c := range ch {
		b = binary.LittleEndian.AppendUint64(b, c[0])
		b = binary.LittleEndian.AppendUint64(b, c[1])
		b = binary.LittleEndian.AppendUint32(b, uint32(c[2]))
		b = binary.LittleEndian.AppendUint32(b, 0)
	}
	return b
}

func counts(out []byte) [3]uint32 {
	r := out[headerLen+48:]
	return [3]uint32{binary.LittleEndian.Uint32(r), binary.LittleEndian.Uint32(r[4:]), binary.LittleEndian.Uint32(r[8:])}
}

func TestCopyChunkCopies(t *testing.T) {
	src := &wfile{data: []byte("0123456789abcdefghij")}
	dst := &wfile{data: []byte("XXXX")}
	c, _, do, key := pair(t, src, dst)
	out := c.copyChunk(header{sessionID: 7}, fsctlSrvCopyChunkWrite, do, chunks(key, [3]uint64{0, 2, 10}, [3]uint64{15, 12, 10}))
	if st := statusOf(t, out); st != statusSuccess {
		t.Fatalf("status %#x", st)
	}
	if got := counts(out); got != [3]uint32{2, 0, 15} {
		t.Fatalf("counts %v, want 2 chunks, 0, 15 bytes (the second stops at the source's end)", got)
	}
	if want := "XX0123456789fghij"; string(dst.data) != want {
		t.Fatalf("target %q, want %q", dst.data, want)
	}
}

// Too many chunks, too long a chunk, or too much in all: refused with the
// limits in the reply, which is how a client learns them.
func TestCopyChunkLimits(t *testing.T) {
	c, _, do, key := pair(t, &wfile{data: make([]byte, 64)}, &wfile{})
	many := make([][3]uint64, copyMaxChunks+1)
	for _, tc := range [][]byte{
		chunks(key, many...),
		chunks(key, [3]uint64{0, 0, copyMaxChunkSize + 1}),
		chunks(key, func() [][3]uint64 {
			var ch [][3]uint64
			for range 17 {
				ch = append(ch, [3]uint64{0, 0, copyMaxChunkSize})
			}
			return ch
		}()...),
		chunks(key, [3]uint64{1 << 63, 0, 1}),
	} {
		out := c.copyChunk(header{sessionID: 7}, fsctlSrvCopyChunk, do, tc)
		if st := statusOf(t, out); st != statusInvalidParameter {
			t.Fatalf("status %#x, want INVALID_PARAMETER", st)
		}
		if got := counts(out); got != [3]uint32{copyMaxChunks, copyMaxChunkSize, copyMaxTotal} {
			t.Fatalf("limits %v", got)
		}
	}
}

func TestCopyChunkRefusals(t *testing.T) {
	boom := errors.New("media error")
	h := header{sessionID: 7}
	for _, tc := range []struct {
		name string
		set  func(c *conn, so, do *openFile, key *[24]byte) []byte
		want uint32
	}{
		{"a short input", func(*conn, *openFile, *openFile, *[24]byte) []byte { return make([]byte, 31) }, statusInvalidParameter},
		{"a chunk list longer than the input", func(_ *conn, _, _ *openFile, k *[24]byte) []byte { return chunks(*k, [3]uint64{0, 0, 1})[:40] }, statusInvalidParameter},
		{"an unknown key", func(_ *conn, _, _ *openFile, k *[24]byte) []byte { k[0] ^= 0xFF; return nil }, statusObjectNameNotFound},
		{"another session's source", func(_ *conn, so, _ *openFile, _ *[24]byte) []byte { so.session = 8; return nil }, statusObjectNameNotFound},
		{"a read-only target", func(_ *conn, _, do *openFile, _ *[24]byte) []byte { do.ro = true; return nil }, statusMediaWriteProtected},
		{"a directory", func(_ *conn, so, _ *openFile, _ *[24]byte) []byte { so.dir = true; return nil }, statusInvalidDeviceRequest},
		{"a pipe", func(_ *conn, _, do *openFile, _ *[24]byte) []byte { do.pipe = &pipe{}; return nil }, statusInvalidDeviceRequest},
		{"no positional source", func(_ *conn, so, _ *openFile, _ *[24]byte) []byte { so.f = nil; return nil }, statusNotSupported},
		{"no in-place target", func(_ *conn, _, do *openFile, _ *[24]byte) []byte { do.w = nil; return nil }, statusNotSupported},
		{"a source range under another's exclusive lock", func(_ *conn, so, _ *openFile, _ *[24]byte) []byte {
			so.share.locks.take(byteLock{path: "/src", offset: 0, length: 4, exclusive: true, owner: [16]byte{9}})
			return nil
		}, statusFileLockConflict},
		{"a target range under another's lock", func(_ *conn, _, do *openFile, _ *[24]byte) []byte {
			do.share.locks.take(byteLock{path: "/dst", offset: 0, length: 4, owner: [16]byte{9}})
			return nil
		}, statusFileLockConflict},
		{"a target that cannot grow", func(_ *conn, _, do *openFile, _ *[24]byte) []byte { do.w.(*wfile).truncErr = boom; return nil }, statusAccessDenied},
		{"a source that cannot be read", func(_ *conn, so, _ *openFile, _ *[24]byte) []byte { so.f.(*wfile).readErr = boom; return nil }, statusAccessDenied},
		{"a target that cannot sync", func(_ *conn, _, do *openFile, _ *[24]byte) []byte { do.w.(*wfile).sync = boom; return nil }, statusAccessDenied},
	} {
		c, so, do, key := pair(t, &wfile{data: []byte("0123456789")}, &wfile{})
		in := tc.set(c, so, do, &key)
		if in == nil {
			in = chunks(key, [3]uint64{0, 0, 4})
		}
		if st := statusOf(t, c.copyChunk(h, fsctlSrvCopyChunk, do, in)); st != tc.want {
			t.Errorf("%s: status %#x, want %#x", tc.name, st, tc.want)
		}
	}
}

// Through IOCTL: the resume key on the source handle, the copy on the target.
func TestCopyChunkThroughIoctl(t *testing.T) {
	src := &wfile{data: []byte("hello")}
	dst := &wfile{}
	c, so, do, _ := pair(t, src, dst)
	ioctl := func(code uint32, id [16]byte, in []byte) []byte {
		body := make([]byte, 56)
		binary.LittleEndian.PutUint16(body, 57)
		binary.LittleEndian.PutUint32(body[4:], code)
		copy(body[8:], id[:])
		binary.LittleEndian.PutUint32(body[24:], uint32(headerLen+56))
		binary.LittleEndian.PutUint32(body[28:], uint32(len(in)))
		msg := request(cmdIoctl, 0, append(body, in...))
		binary.LittleEndian.PutUint64(msg[offSessionID:], 7)
		out, err := c.dispatch(msg)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := ioctl(fsctlSrvRequestResumeKey, so.id, nil)
	var key [24]byte
	copy(key[:], out[headerLen+48:])
	if st := statusOf(t, ioctl(fsctlSrvCopyChunk, do.id, chunks(key, [3]uint64{0, 0, 5}))); st != statusSuccess || string(dst.data) != "hello" {
		t.Fatalf("status %#x, target %q", st, dst.data)
	}
	if st := statusOf(t, ioctl(fsctlSrvRequestResumeKey, [16]byte{0xEE}, nil)); st != statusFileClosed {
		t.Fatalf("resume key on a closed handle: %#x", st)
	}
	if st := statusOf(t, ioctl(fsctlSrvCopyChunk, [16]byte{0xEE}, chunks(key))); st != statusFileClosed {
		t.Fatalf("copy on a closed handle: %#x", st)
	}
	// An input that says it runs past the message.
	body := make([]byte, 56)
	binary.LittleEndian.PutUint16(body, 57)
	binary.LittleEndian.PutUint32(body[4:], fsctlSrvCopyChunk)
	copy(body[8:], do.id[:])
	binary.LittleEndian.PutUint32(body[24:], uint32(headerLen+56))
	binary.LittleEndian.PutUint32(body[28:], 1<<20)
	msg := request(cmdIoctl, 0, body)
	binary.LittleEndian.PutUint64(msg[offSessionID:], 7)
	out, err := c.dispatch(msg)
	if err != nil || statusOf(t, out) != statusInvalidParameter {
		t.Fatalf("an input past the message: %v %#x", err, statusOf(t, out))
	}
}
