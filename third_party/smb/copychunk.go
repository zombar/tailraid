// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"crypto/rand"
	"encoding/binary"

	"github.com/go-filesystems/hostcopy"
)

// Server-side copy (MS-SMB2 3.3.5.15.6): the client asks for a resume key
// on the source handle, then sends COPYCHUNK, or COPYCHUNK_WRITE, on the
// target with a list of ranges. Explorer copies within a share this way, and
// the Linux client turns copy_file_range(2) into it. The bytes never cross
// the network; here they go through go-filesystems/hostcopy -- a megabyte at
// a time, or copy_file_range(2) in the kernel between two files of the host,
// which shares blocks on btrfs and XFS.
const (
	fsctlSrvRequestResumeKey uint32 = 0x00140078
	fsctlSrvCopyChunk        uint32 = 0x001440F2
	fsctlSrvCopyChunkWrite   uint32 = 0x001480F2
)

// The limits a request is held to, Windows' own (MS-SMB2 Appendix A, notes
// 223-225): what one request may ask, so that one request is bounded work.
const (
	copyMaxChunks    = 256
	copyMaxChunkSize = 1 << 20
	copyMaxTotal     = 16 << 20
)

// resumeKey answers FSCTL_SRV_REQUEST_RESUME_KEY: 24 opaque bytes that name
// the source open, kept on the open itself, and a zero ContextLength.
func (c *conn) resumeKey(h header, of *openFile) []byte {
	if !of.hasResumeKey {
		rand.Read(of.resumeKey[:]) // never fails since Go 1.24: it crashes instead
		of.hasResumeKey = true
	}
	out := make([]byte, 28)
	copy(out, of.resumeKey[:])
	return ioctlResponse(h, fsctlSrvRequestResumeKey, out, of.id, statusSuccess)
}

// copyChunk answers FSCTL_SRV_COPYCHUNK and _WRITE, sent on the target.
func (c *conn) copyChunk(h header, code uint32, dst *openFile, in []byte) []byte {
	if len(in) < 32 {
		return errorResponse(h, statusInvalidParameter)
	}
	var key [24]byte
	copy(key[:], in)
	count := binary.LittleEndian.Uint32(in[24:])
	if uint64(len(in)) < 32+uint64(count)*24 {
		return errorResponse(h, statusInvalidParameter)
	}
	type chunk struct {
		src, dst int64
		n        uint32
	}
	chunks := make([]chunk, 0, min(count, copyMaxChunks))
	var total uint64
	tooMuch := count > copyMaxChunks
	for i := range min(count, copyMaxChunks) {
		b := in[32+24*i:]
		ch := chunk{int64(binary.LittleEndian.Uint64(b)), int64(binary.LittleEndian.Uint64(b[8:])), binary.LittleEndian.Uint32(b[16:])}
		if ch.n > copyMaxChunkSize || ch.src < 0 || ch.dst < 0 {
			tooMuch = true
		}
		total += uint64(ch.n)
		chunks = append(chunks, ch)
	}
	if tooMuch || total > copyMaxTotal {
		// The limits go back in the three fields, which is how a client
		// learns them (the Linux client lowers its own from these).
		out := make([]byte, 12)
		binary.LittleEndian.PutUint32(out[0:], copyMaxChunks)
		binary.LittleEndian.PutUint32(out[4:], copyMaxChunkSize)
		binary.LittleEndian.PutUint32(out[8:], copyMaxTotal)
		return ioctlResponse(h, code, out, dst.id, statusInvalidParameter)
	}

	// The source is an open of THIS session: a key from another session's
	// open is not found, as 3.3.5.15.6 says.
	var src *openFile
	for _, of := range c.files {
		if of.hasResumeKey && of.resumeKey == key && of.session == h.sessionID {
			src = of
			break
		}
	}
	if src == nil {
		return errorResponse(h, statusObjectNameNotFound)
	}
	if dst.ro {
		return errorResponse(h, statusMediaWriteProtected)
	}
	if src.dir || dst.dir || src.pipe != nil || dst.pipe != nil {
		return errorResponse(h, statusInvalidDeviceRequest)
	}
	if src.f == nil || dst.w == nil {
		// Not here: the client copies another way (the Linux client
		// falls back to splice on EOPNOTSUPP).
		return errorResponse(h, statusNotSupported)
	}
	defer dst.share.changing()()
	var written uint32
	var bytes uint64
	for _, ch := range chunks {
		// Locks hold as they do for READ and WRITE: the source range crosses
		// no exclusive lock of another open, the target range no lock.
		if src.share.locks.held(src.path, uint64(ch.src), uint64(ch.n), src.id, false) ||
			dst.share.locks.held(dst.path, uint64(ch.dst), uint64(ch.n), dst.id, true) {
			return c.copyStopped(h, code, dst, statusFileLockConflict, written, bytes)
		}
		// The target's final length first: a driver whose WriteAt cannot
		// extend a file grows it only through Truncate.
		end := ch.dst + int64(ch.n)
		if ch.src+int64(ch.n) > src.f.Size() {
			end = ch.dst + max(src.f.Size()-ch.src, 0)
		}
		if end > dst.w.Size() {
			if err := dst.w.Truncate(end); err != nil {
				return c.copyStopped(h, code, dst, statusFor(err, statusAccessDenied), written, bytes)
			}
		}
		n, err := hostcopy.Range(dst.w, src.f, ch.dst, ch.src, int64(ch.n))
		bytes += uint64(n)
		if err != nil {
			return c.copyStopped(h, code, dst, statusFor(err, statusAccessDenied), written, bytes)
		}
		written++
	}
	if err := dst.w.Sync(); err != nil {
		return c.copyStopped(h, code, dst, statusFor(err, statusAccessDenied), written, bytes)
	}
	dst.share.notify(dst.path, actionModified)
	out := make([]byte, 12)
	binary.LittleEndian.PutUint32(out[0:], written)
	binary.LittleEndian.PutUint32(out[8:], uint32(bytes)) // ChunkBytesWritten stays zero
	return ioctlResponse(h, code, out, dst.id, statusSuccess)
}

// copyStopped reports a copy that failed partway, with what it had done.
func (c *conn) copyStopped(h header, code uint32, dst *openFile, status uint32, written uint32, bytes uint64) []byte {
	out := make([]byte, 12)
	binary.LittleEndian.PutUint32(out[0:], written)
	binary.LittleEndian.PutUint32(out[8:], uint32(bytes))
	return ioctlResponse(h, code, out, dst.id, status)
}
