// SPDX-License-Identifier: BSD-3-Clause

package smb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// Create dispositions: what to do about a file that is or is not there.
const (
	dispSupersede   uint32 = 0
	dispOpen        uint32 = 1
	dispCreate      uint32 = 2
	dispOpenIf      uint32 = 3
	dispOverwrite   uint32 = 4
	dispOverwriteIf uint32 = 5
)

// Create options this server reads.
const (
	optDirectoryFile    uint32 = 0x00000001
	optNonDirectoryFile uint32 = 0x00000040
	optDeleteOnClose    uint32 = 0x00001000
)

// Create actions, reported back so a client knows what happened.
const (
	actionSuperseded  uint32 = 0
	actionOpened      uint32 = 1
	actionCreated     uint32 = 2
	actionOverwritten uint32 = 3
)

// An openFile is one handle a client holds.
type openFile struct {
	id [16]byte
	// path is as the driver spells it: "/sub/file.txt". A rename through any handle of
	// the share moves it (share.moveHandles), so it is read through p() and written
	// under pmu: a handle of another connection may be using it at that moment.
	path          string
	pmu           sync.Mutex
	ino           uint64 // the file's inode when it was opened: what a delete-on-close removes (I-225)
	share         *share
	dir           bool
	deleteOnClose bool

	// ro is read-only for THIS handle: the share's own answer plus the one
	// for the user who connected the tree (see access.go). Every refusal and
	// every read-only bit in a reply reads this rather than share.ro, so two
	// people holding the same share get their own answer.
	ro bool

	// pipe is set when this handle is not a file at all: see pipe.go. Every
	// command that would touch a filesystem checks it FIRST, because IPC$ has
	// no filesystem and share.fsys is nil there.
	pipe *pipe

	// f is the driver's positional handle when it has one. A driver without
	// Opener leaves this nil and the whole-file path is used instead, which is
	// O(size) per request and is why the probe exists.
	f filesystem.File
	w filesystem.WritableFile

	// session is the session that opened it, and resumeKey the key a
	// server-side copy names it by once asked for (copychunk.go).
	session      uint64
	resumeKey    [24]byte
	hasResumeKey bool

	// inflight are this handle's WRITEs and READs still running off the read
	// loop (writeAsync, readAsync): a CLOSE waits for them, so a handle is
	// never closed under a write or a read it accepted.
	inflight sync.WaitGroup

	// watch is the change notification this directory handle keeps from its first
	// CHANGE_NOTIFY until CLOSE (I-264); only the read loop touches the field.
	watch *watcher


	// access and shareAccess are what the CREATE asked: the data access it holds and what it
	// lets other opens do (I-265)
	access, shareAccess uint32

	// created is whether this handle's CREATE made the file: its delete-on-close is the
	// creator's own cleanup, which an OwnDeleter may allow where it refuses any other
	// delete (a failed copy removing what it wrote).
	created bool
}

// allOnesFileID is how a chained request says "the file the previous operation
// opened". macOS sends CREATE and QUERY_INFO together and puts this in the
// second one, so a server that looks it up literally finds nothing.
var allOnesFileID = [16]byte{
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
}

func (c *conn) fileByID(b []byte) *openFile {
	if len(b) < 16 {
		return nil
	}
	id := [16]byte(b[:16])
	if id == allOnesFileID {
		return c.files[c.lastFile]
	}
	return c.files[id]
}

// create opens or makes a file, and is where most of a mount's decisions are.
func (c *conn) create(h header, body []byte, msg []byte) ([]byte, error) {
	tc := c.tree(h)
	if tc == nil {
		return errorResponse(h, statusNetworkNameDeleted), nil
	}
	sh := tc.sh
	// From here on this CREATE may fail, and if it does there is no "file the
	// previous operation opened" any more. Clearing it first means an
	// all-ones file id after a failure finds NOTHING rather than the handle
	// this connection opened before -- which is somebody's live file.
	c.lastFile = [16]byte{}
	// CREATE stats, may write, stats again and opens. Two clients creating the
	// same file would otherwise both find it missing.
	defer sh.changing()()
	if len(body) < 56 {
		return nil, fmt.Errorf("smb: CREATE body of %d bytes is too short", len(body))
	}
	access := dataAccess(binary.LittleEndian.Uint32(body[24:]))
	shareAccess := binary.LittleEndian.Uint32(body[32:])
	disposition := binary.LittleEndian.Uint32(body[36:])
	options := binary.LittleEndian.Uint32(body[40:])
	nameOff := int(binary.LittleEndian.Uint16(body[44:]))
	nameLen := int(binary.LittleEndian.Uint16(body[46:]))
	if nameOff < 0 || nameLen < 0 || nameOff+nameLen > len(msg) {
		return nil, fmt.Errorf("smb: the file name is not inside the message")
	}
	name := fromUTF16le(msg[nameOff : nameOff+nameLen])
	if sh.ipc {
		// Not a path: a pipe name. IPC$ has no filesystem behind it, and the
		// handle that comes back speaks DCE/RPC.
		return c.openPipe(h, name)
	}
	p, ok := smbPathToFS(name)
	if !ok {
		return errorResponse(h, statusObjectNameNotFound), nil
	}

	writing := disposition != dispOpen
	if writing && tc.ro {
		return errorResponse(h, statusMediaWriteProtected), nil
	}

	// SMB is caseless and this server says so; the driver underneath may not
	// be. See casefold.go: the name is taken as it came unless nothing is
	// there under it.
	p = resolveCase(sh.fsys, p)

	st, statErr := sh.fsys.Stat(p)
	exists := statErr == nil
	action := actionOpened
	if !exists && parentMissing(sh.fsys, p) {
		return errorResponse(h, statusObjectPathNotFound), nil // never made on the way (I-233)
	}

	// an open that conflicts with one held is refused before it changes anything (I-265)
	if exists && st != nil && sh.sharingViolation(st.Inode(), access, shareAccess) {
		return errorResponse(h, statusSharingViolation), nil
	}

	switch {
	case exists && (disposition == dispCreate):
		return errorResponse(h, statusObjectNameCollision), nil
	case !exists && (disposition == dispOpen || disposition == dispOverwrite):
		return errorResponse(h, statusObjectNameNotFound), nil
	case !exists && (disposition == dispCreate || disposition == dispOpenIf ||
		disposition == dispOverwriteIf || disposition == dispSupersede):
		x, excl := sh.fsys.(ExclusiveCreator)
		xd, exclDir := sh.fsys.(ExclusiveDirCreator)
		action = actionCreated
		var err error
		switch {
		case options&optDirectoryFile != 0 && exclDir && disposition == dispCreate:
			err = xd.MkDirExcl(p, 0o755)
		case options&optDirectoryFile != 0:
			err = sh.fsys.MkDir(p, 0o755)
		case excl && (disposition == dispCreate || disposition == dispOpenIf):
			// another server over the same volume may have made the name since Stat
			// looked: the backend decides. A name taken there is a collision for
			// FILE_CREATE, and for FILE_OPEN_IF the other server's file, opened and
			// never truncated
			err = x.CreateExcl(p, 0o644)
			if disposition == dispOpenIf && errors.Is(err, os.ErrExist) {
				err, action = nil, actionOpened
			}
		default:
			err = sh.fsys.WriteFile(p, nil, 0o644)
		}
		if err != nil {
			return errorResponse(h, statusFor(err, statusAccessDenied)), nil
		}
		if st, statErr = sh.fsys.Stat(p); statErr != nil {
			return errorResponse(h, statusFor(statErr, statusObjectNameNotFound)), nil
		}
	case exists && (disposition == dispOverwrite || disposition == dispOverwriteIf || disposition == dispSupersede):
		if isDir(st) {
			return errorResponse(h, statusFileIsADirectory), nil
		}
		if err := sh.fsys.WriteFile(p, nil, 0o644); err != nil {
			return errorResponse(h, statusFor(err, statusAccessDenied)), nil
		}
		action = actionOverwritten
		if disposition == dispSupersede {
			action = actionSuperseded
		}
		if st, statErr = sh.fsys.Stat(p); statErr != nil {
			return errorResponse(h, statusFor(statErr, statusObjectNameNotFound)), nil
		}
	}

	// A client says what it expects to find, and being told the truth is what
	// lets it fall back instead of failing later on a read.
	dir := isDir(st)
	if dir && options&optNonDirectoryFile != 0 {
		return errorResponse(h, statusFileIsADirectory), nil
	}
	if !dir && options&optDirectoryFile != 0 {
		return errorResponse(h, statusNotADirectory), nil
	}

	created := action == actionCreated && !dir
	if options&optDeleteOnClose != 0 && !tc.ro {
		// the delete happens at CLOSE, whose reply carries no failure: a backend that
		// may refuse it is asked now, where the client reads the status
		if err := canDelete(sh.fsys, p, created); err != nil {
			return errorResponse(h, statusFor(err, statusAccessDenied)), nil
		}
	}
	of := &openFile{path: p, share: sh, ro: tc.ro, dir: dir, deleteOnClose: options&optDeleteOnClose != 0, session: h.sessionID, created: created, access: access, shareAccess: shareAccess}
	if st != nil {
		of.ino = st.Inode()
	}
	if !dir {
		if o, canOpen := sh.fsys.(filesystem.Opener); canOpen {
			if f, err := o.OpenFile(p); err == nil {
				of.f = f
				// The positional write path is an upgrade OF the open file,
				// probed on the File and not on the Filesystem: a driver can
				// return a plain File for one file and a writable one for the
				// next.
				if w, canWrite := f.(filesystem.WritableFile); canWrite {
					of.w = w
				}
			}
		}
	}
	if action == actionCreated {
		sh.notify(p, actionAdded)
	}
	c.nextFile++
	binary.LittleEndian.PutUint64(of.id[:8], c.nextFile)
	binary.LittleEndian.PutUint64(of.id[8:], uint64(h.treeID))
	c.files[of.id] = of
	sh.register(of)
	c.lastFile = of.id

	const bodyLen = 88
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen+1)...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 89)
	binary.LittleEndian.PutUint32(rb[4:], action)
	writeTimes(rb[8:], st)
	size := sizeOf(st)
	binary.LittleEndian.PutUint64(rb[40:], allocationOf(size))
	binary.LittleEndian.PutUint64(rb[48:], size)
	binary.LittleEndian.PutUint32(rb[56:], attributesOf(of.p(), st, tc.ro))
	copy(rb[64:], of.id[:])
	// The contexts offset stays ZERO because there are none. Pointing it at
	// the one pad byte the structure size accounts for is what a real client
	// rejects: it requires the offset to be eight-byte aligned, and 153 is
	// not. Zero is how "there are none" is spelled.
	return b, nil
}

// DeleteChecker is the optional capability of a filesystem that can refuse a delete
// before it is marked: SMB2 deletes at CLOSE, whose reply carries no failure, so a
// backend that may refuse one (a protected folder) is asked when the client sets
// delete-on-close, at CREATE or through SET_INFO, and the client reads the status.
type DeleteChecker interface {
	CanDelete(path string) error
}

// OwnDeleter is the optional capability of a filesystem that lets the handle which
// created a file delete it where any other delete would be refused (a protected folder
// taking a failed copy's cleanup): CanDeleteOwn answers for such a handle in place of
// CanDelete, and its delete-on-close calls DeleteOwnFile in place of DeleteFile.
type OwnDeleter interface {
	CanDeleteOwn(path string) error
	DeleteOwnFile(path string) error
}

// canDelete asks the backend whether a handle may mark path delete-on-close: as its
// creator's own cleanup when created and the backend tells the two apart.
func canDelete(fsys filesystem.Filesystem, path string, created bool) error {
	if o, ok := fsys.(OwnDeleter); ok && created {
		return o.CanDeleteOwn(path)
	}
	if d, ok := fsys.(DeleteChecker); ok {
		return d.CanDelete(path)
	}
	return nil
}

// ExclusiveCreator is the optional capability of a filesystem shared with other servers
// (several gateways over one volume): CreateExcl makes an empty file at path or fails
// with an error matching os.ErrExist when the name exists, decided by the backend rather
// than by this server's Stat, which may not have seen the other server's create yet.
// FILE_CREATE uses it, so the losing client sees STATUS_OBJECT_NAME_COLLISION and the
// winner's file is never truncated.
type ExclusiveCreator interface {
	CreateExcl(path string, perm os.FileMode) error
}

// ExclusiveDirCreator is ExclusiveCreator for a directory: MkDirExcl makes it only when
// the name is free, as the backend decides, so FILE_CREATE of a directory another server
// made first is a collision too.
type ExclusiveDirCreator interface {
	MkDirExcl(path string, perm os.FileMode) error
}

// InodeDeleter is the optional capability of a filesystem that deletes by identity:
// DeleteFileIno removes the file or directory at path only when it is still the inode
// ino, and does nothing otherwise, decided where the names live (a shared volume, where
// another server may have renamed it away and made a new one under the name). own is as
// for OwnDeleter: the handle that created the file is deleting it. A delete-on-close
// uses it (I-225).
type InodeDeleter interface {
	DeleteFileIno(path string, ino uint64, own bool) error
}

// NoReplaceRenamer is the optional capability of a filesystem that renames without
// replacing, decided where the names live: an existing destination is an error matching
// os.ErrExist and nothing moves. A rename without ReplaceIfExists uses it (I-224).
type NoReplaceRenamer interface {
	RenameNoReplace(from, to string) error
}

// sameFile reports whether path still names the inode ino (0: unknown, taken as yes).
func sameFile(fsys filesystem.Filesystem, path string, ino uint64) bool {
	st, err := fsys.Stat(path)
	return err == nil && (ino == 0 || st.Inode() == ino)
}

// p is the handle's path now.
func (of *openFile) p() string {
	of.pmu.Lock()
	defer of.pmu.Unlock()
	return of.path
}

// Timestamper is the optional capability of a Stat that knows its times. The
// go-filesystems Stat is mode, size and inode; a driver whose metadata carries
// times returns a Stat with this method too, and a client then sees them.
type Timestamper interface {
	Times() (atime, mtime, ctime time.Time)
}

// writeTimes fills the four timestamps a client shows: creation, last access,
// last write, change. They are the Stat's own when it has them (creation is
// the change time: go-filesystems has no birth time), and otherwise all NOW
// rather than zero: a zero FILETIME is the year 1601, which a file manager
// displays as a date and a backup tool treats as ancient.
func writeTimes(b []byte, st filesystem.Stat) {
	now := time.Now()
	a, m, c := now, now, now
	if ts, ok := st.(Timestamper); ok {
		a, m, c = ts.Times()
	}
	for i, t := range []time.Time{c, a, m, c} {
		if t.IsZero() {
			t = now
		}
		binary.LittleEndian.PutUint64(b[i*8:], filetime(t))
	}
}

// timeOf is the way back from a FILETIME a client sets.
func timeOf(ft uint64) time.Time {
	const epochDelta = 11644473600 // seconds from 1601 to 1970
	return time.Unix(int64(ft/10_000_000)-epochDelta, int64(ft%10_000_000)*100)
}

// closeOpen closes one open as CLOSE does, delete-on-close included: for a CLOSE, and
// for every open its client leaves behind when the connection drops, the session logs
// off or the tree disconnects (MS-SMB2 3.3.7.1, 3.3.5.6, 3.3.5.8, I-240). Without it a
// copy's guard file outlived a lost connection, full size and made of holes.
func (c *conn) closeOpen(of *openFile) {
	of.inflight.Wait() // every WRITE and READ this handle accepted is answered first
	if of.f != nil {
		of.f.Close()
	}
	delete(c.files, of.id)
	of.share.locks.releaseAll(of.id)
	if of.watch != nil {
		of.share.watchers.remove(of.watch)
		of.watch = nil
	}
	c.cancelForFile(of.id)
	defer of.share.changing()() // the file may go with the handle
	// a rename holds the share's lock while it moves the handles: this one leaves the
	// list only under it, so its path below is the file's, never a name it had (I-225)
	of.share.unregister(of)
	// The listing this handle was paging through goes with it. It holds a Stat
	// for every entry in the directory, and a client that opens and closes
	// directories all day -- which is what a file manager does -- would
	// otherwise leave one behind each time.
	delete(c.searches, of.id)
	if of.deleteOnClose && !of.ro {
		// the file this handle opened, or nothing: another client may have renamed it
		// away (the handle followed) and made a new file under its old name (I-225)
		p := of.p()
		d, byIno := of.share.fsys.(InodeDeleter)
		switch {
		case byIno && of.ino != 0:
			d.DeleteFileIno(p, of.ino, of.created) // the backend decides, atomically
		case !sameFile(of.share.fsys, p, of.ino):
			// someone else's file now: left alone
		default:
			if o, ok := of.share.fsys.(OwnDeleter); ok && of.created {
				o.DeleteOwnFile(p)
			} else if of.dir {
				of.share.fsys.DeleteDir(p)
			} else {
				of.share.fsys.DeleteFile(p)
			}
		}
		of.share.notify(p, actionRemoved)
	}
}

func (c *conn) closeFile(h header, body []byte) ([]byte, error) {
	if len(body) < 24 {
		return nil, fmt.Errorf("smb: CLOSE body of %d bytes is too short", len(body))
	}
	of := c.fileByID(body[8:])
	if of == nil {
		return errorResponse(h, statusFileClosed), nil
	}
	c.closeOpen(of)

	b := append(responseTo(h, statusSuccess), make([]byte, 60)...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 60)
	// Flags bit 0 means "the attributes below are filled in"; leaving it clear
	// says they are not, which is honest for a handle that has just gone.
	return b, nil
}

func (c *conn) read(h header, body []byte) ([]byte, error) {
	if len(body) < 48 {
		return nil, fmt.Errorf("smb: READ body of %d bytes is too short", len(body))
	}
	length := int(binary.LittleEndian.Uint32(body[4:]))
	offset := int64(binary.LittleEndian.Uint64(body[8:]))
	of := c.fileByID(body[16:])
	if of == nil {
		return errorResponse(h, statusFileClosed), nil
	}
	if of.pipe != nil {
		return c.readPipe(h, of, length), nil
	}
	if of.dir {
		return errorResponse(h, statusInvalidDeviceRequest), nil
	}
	if length > maxReadSize {
		length = maxReadSize
	}
	if offset < 0 {
		return errorResponse(h, statusInvalidParameter), nil
	}
	// A lock nobody enforces is decoration. A read may cross a SHARED lock
	// held by somebody else; an exclusive one stops it.
	if of.share.locks.held(of.p(), uint64(offset), uint64(length), of.id, false) {
		return errorResponse(h, statusFileLockConflict), nil
	}
	if of.f != nil {
		// Positional, and off the read loop once it takes long (readAsync), so a
		// READ waiting for its bytes holds up no other request on the connection;
		// the share lock is held around it unless the driver says its reads are
		// safe alongside its other calls (ConcurrentReader, I-254).
		if c.alone {
			return c.readAsync(h, of, length, offset), nil
		}
		defer lockUnlessConcurrentRead(of.share)()
		return readAt(h, of, length, offset), nil
	}
	defer of.share.reading()()

	// No positional read: the whole file, every time. See Opener's
	// documentation for why a driver that cannot answer a byte range
	// should not pretend to.
	buf := make([]byte, length)
	var n int
	data, err := of.share.fsys.ReadFile(of.p())
	if err != nil {
		return errorResponse(h, statusFor(err, statusUnexpectedIoError)), nil
	}
	if offset < int64(len(data)) {
		n = copy(buf, data[offset:])
	}
	if n == 0 {
		// END_OF_FILE, not a zero-length success: a client that gets zero
		// bytes with success reads forever.
		return errorResponse(h, statusEndOfFile), nil
	}

	const bodyLen = 16
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen+n)...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 17)
	rb[2] = headerLen + bodyLen // data offset, from the start of the message
	binary.LittleEndian.PutUint32(rb[4:], uint32(n))
	copy(rb[bodyLen:], buf[:n])
	return b, nil
}

// readAt reads length bytes at offset through the handle's positional reader and builds
// the reply.
func readAt(h header, of *openFile, length int, offset int64) []byte {
	buf := make([]byte, length)
	n, err := of.f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return errorResponse(h, statusFor(err, statusUnexpectedIoError))
	}
	if n == 0 {
		// END_OF_FILE, not a zero-length success: a client that gets zero
		// bytes with success reads forever.
		return errorResponse(h, statusEndOfFile)
	}
	const bodyLen = 16
	b := append(responseTo(h, statusSuccess), make([]byte, bodyLen+n)...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 17)
	rb[2] = headerLen + bodyLen // data offset, from the start of the message
	binary.LittleEndian.PutUint32(rb[4:], uint32(n))
	copy(rb[bodyLen:], buf[:n])
	return b
}

// asyncReads bounds the READs one connection runs at once off the read loop, each
// holding up to maxReadSize; one more waits for a slot.
const asyncReads = 32

// readAsync runs a READ off the read loop, as writeAsync a WRITE (I-254): one that
// finishes within writeGrace is answered as any other, a slower one STATUS_PENDING at
// once and its data when the driver returns, under the same MessageId and AsyncId.
// Windows reads every file over one connection, and a cold chunk takes a network round
// trip: before this, one file's READ waiting for its shards was every file's stall. A
// READ changes nothing, so a CANCEL of it is honoured: STATUS_CANCELLED, and the bytes,
// when they come, are dropped.
func (c *conn) readAsync(h header, of *openFile, length int, offset int64) []byte {
	c.readSlots <- struct{}{}
	of.inflight.Add(1)
	done := make(chan []byte, 1)
	later := make(chan *pendingOp, 1)
	key := c.replyKey(h)
	go func() {
		defer of.inflight.Done()
		defer func() { <-c.readSlots }()
		unlock := lockUnlessConcurrentRead(of.share)
		reply := readAt(h, of, length, offset)
		unlock()
		done <- reply
		if op := <-later; op != nil {
			c.finishAsyncWith(op, asFinal(reply, op.asyncID), key)
		}
	}()
	t := time.NewTimer(writeGrace)
	defer t.Stop()
	select {
	case reply := <-done:
		later <- nil
		return reply
	case <-t.C:
	}
	op, interim := c.beginAsync(h, of.id)
	if key != nil {
		signMessage(c.dialect, key, interim)
	}
	_ = c.send(interim) // a failed send ends the connection at the next read
	later <- op
	return nil
}

// ConcurrentReader is the optional capability of a filesystem whose positional reads
// (File.ReadAt) may run alongside any of its other calls: a READ from it then takes no
// share lock, so one waiting for its bytes delays no CREATE, CLOSE or rename on the share
// (I-254). A driver without it has every READ hold the share's read lock, as before.
type ConcurrentReader interface {
	ConcurrentReads() bool
}

// lockUnlessConcurrentRead takes the share's read lock for a positional read unless the
// driver is a ConcurrentReader, and returns its release.
func lockUnlessConcurrentRead(sh *share) func() {
	if cr, ok := sh.fsys.(ConcurrentReader); ok && cr.ConcurrentReads() {
		return func() {}
	}
	return sh.reading()
}

func (c *conn) write(h header, body []byte, msg []byte) ([]byte, error) {
	if len(body) < 48 {
		return nil, fmt.Errorf("smb: WRITE body of %d bytes is too short", len(body))
	}
	dataOff := int(binary.LittleEndian.Uint16(body[2:]))
	length := int(binary.LittleEndian.Uint32(body[4:]))
	offset := int64(binary.LittleEndian.Uint64(body[8:]))
	of := c.fileByID(body[16:])
	if of == nil {
		return errorResponse(h, statusFileClosed), nil
	}
	if dataOff < 0 || length < 0 || dataOff+length > len(msg) {
		return nil, fmt.Errorf("smb: the write payload is not inside the message")
	}
	data := msg[dataOff : dataOff+length]
	// A pipe is written even on IPC$, which is read-only as a share: what the
	// read-only flag is about is the filesystem, and there is none here.
	if of.pipe != nil {
		return c.writePipe(h, of, data), nil
	}
	if of.ro {
		return errorResponse(h, statusMediaWriteProtected), nil
	}
	if of.dir {
		return errorResponse(h, statusInvalidDeviceRequest), nil
	}
	// A write crosses NO lock held by anybody else, shared or exclusive.
	if of.share.locks.held(of.p(), uint64(offset), uint64(length), of.id, true) {
		return errorResponse(h, statusFileLockConflict), nil
	}
	if of.w != nil {
		// Positional, and off the read loop once it takes long (writeAsync),
		// so a WRITE waiting in the driver holds up no other request on the
		// connection. The share lock is held around it unless the driver
		// says its positional writes are safe alongside its other calls
		// (ConcurrentWriter): a Filesystem promises nothing of the kind.
		if c.alone {
			return c.writeAsync(h, of, data, offset), nil
		}
		defer lockUnlessConcurrent(of.share)()
		return writeAt(h, of, data, offset), nil
	}
	defer of.share.changing()()
	{
		// Read, splice, write the whole file back. O(size) per request, which
		// a client streaming in blocks turns into O(n^2) -- the reason
		// WritableFile exists. A failure here is reported and NEVER retried
		// through another path: it has already changed the file.
		old, err := of.share.fsys.ReadFile(of.p())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errorResponse(h, statusFor(err, statusAccessDenied)), nil
		}
		end := offset + int64(len(data))
		if int64(len(old)) < end {
			grown := make([]byte, end)
			copy(grown, old)
			old = grown
		}
		copy(old[offset:], data)
		if err := of.share.fsys.WriteFile(of.p(), old, 0o644); err != nil {
			return errorResponse(h, statusFor(err, statusAccessDenied)), nil
		}
	}

	of.share.notify(of.p(), actionModified)
	return writeReply(h, length), nil
}

func writeReply(h header, length int) []byte {
	b := append(responseTo(h, statusSuccess), make([]byte, 16)...)
	rb := b[headerLen:]
	binary.LittleEndian.PutUint16(rb[0:], 17)
	binary.LittleEndian.PutUint32(rb[4:], uint32(length))
	return b
}

// writeAt is one WRITE through the driver's positional handle, and its reply.
func writeAt(h header, of *openFile, data []byte, offset int64) []byte {
	if _, err := of.w.WriteAt(data, offset); err != nil {
		return errorResponse(h, statusFor(err, statusAccessDenied))
	}
	if err := of.w.Sync(); err != nil {
		return errorResponse(h, statusFor(err, statusAccessDenied))
	}
	of.share.notify(of.p(), actionModified)
	return writeReply(h, len(data))
}

// The bounds of WRITEs answered later.
const (
	// writeGrace is how long the read loop waits for a WRITE before it
	// answers STATUS_PENDING and goes on: a WAL fsync fits inside it, a
	// WRITE waiting for room in the gateway's caps does not.
	writeGrace = 5 * time.Millisecond
	// asyncWrites bounds the WRITEs one connection runs at once, each
	// holding its payload (a megabyte at most); one more waits for a slot,
	// which is the backpressure a client feels.
	asyncWrites = 64
)

// writeAsync runs a WRITE off the read loop. One that finishes within
// writeGrace is answered as any other; a slower one is answered
// STATUS_PENDING at once, and its real reply goes out when the driver
// returns, under the same MessageId and the AsyncId the interim named
// (MS-SMB2 3.3.4.2). Windows copies every file to a server over one
// connection: before this, one copy's WRITE waiting at a cap was every
// copy's stall.
//
// It sends the interim itself and returns nil, so that nothing the read
// loop sends later can overtake it; it is only used for a request that is
// not part of a chain (conn.alone).
func (c *conn) writeAsync(h header, of *openFile, data []byte, offset int64) []byte {
	c.writeSlots <- struct{}{}
	of.inflight.Add(1)
	done := make(chan []byte, 1)
	later := make(chan *pendingOp, 1)
	key := c.replyKey(h)
	go func() {
		defer of.inflight.Done()
		defer func() { <-c.writeSlots }()
		unlock := lockUnlessConcurrent(of.share)
		reply := writeAt(h, of, data, offset)
		unlock()
		c.trace("write_done", "session", h.sessionID, "msg", h.messageID, "path", of.p(), "ino", of.ino, "len", len(data), "off", offset, "status", fmt.Sprintf("%#x", replyStatus(reply)))
		done <- reply
		if op := <-later; op != nil {
			c.finishAsyncWith(op, asFinal(reply, op.asyncID), key)
		}
	}()
	t := time.NewTimer(writeGrace)
	defer t.Stop()
	select {
	case reply := <-done:
		later <- nil
		return reply
	case <-t.C:
	}
	op, interim := c.beginAsyncOp(h, of.id, true) // the write is in the driver: CANCEL cannot take it back (I-247)
	if key != nil {
		signMessage(c.dialect, key, interim)
	}
	_ = c.send(interim) // a failed send ends the connection at the next read
	later <- op
	return nil
}

// ConcurrentWriter is the optional capability of a filesystem whose positional writes
// (WritableFile.WriteAt and Sync) may run alongside any of its other calls: a WRITE to
// it then takes no share lock, so one waiting in the driver delays no other request on
// the share. A driver without it has every WRITE serialised with the share's other
// changes, as before.
type ConcurrentWriter interface {
	ConcurrentWrites() bool
}

// lockUnlessConcurrent takes the share lock for a positional write unless the driver is a
// ConcurrentWriter, and returns its release.
func lockUnlessConcurrent(sh *share) func() {
	if cw, ok := sh.fsys.(ConcurrentWriter); ok && cw.ConcurrentWrites() {
		return func() {}
	}
	return sh.changing()
}

// asFinal turns a reply built for a synchronous answer into the final reply
// of an operation answered STATUS_PENDING: the async flag and AsyncId in the
// header, and no credits, which the interim granted already.
func asFinal(reply []byte, asyncID uint64) []byte {
	flags := binary.LittleEndian.Uint32(reply[offFlags:]) | flagAsyncCommand
	binary.LittleEndian.PutUint32(reply[offFlags:], flags)
	binary.LittleEndian.PutUint64(reply[offAsyncID:], asyncID)
	binary.LittleEndian.PutUint16(reply[offCredits:], 0)
	return reply
}

// flush has nothing to do for a driver that has already written, and says so
// with success rather than with NOT_IMPLEMENTED: a client treats a failed
// flush as data loss.
func (c *conn) flush(h header, body []byte) ([]byte, error) {
	if len(body) >= 24 {
		if of := c.fileByID(body[8:]); of != nil && of.w != nil {
			defer fileLock(of)() // a concurrent driver's Sync takes no share lock (I-268)
			if err := of.w.Sync(); err != nil {
				return errorResponse(h, statusFor(err, statusAccessDenied)), nil
			}
		}
	}
	return simpleResponse(h, statusSuccess, 4), nil
}

// statusFor maps a driver's error onto the status a client reads. The default
// is the caller's, because what "not found" means depends on what was asked.
// ErrDirectoryNotEmpty is what a DeleteChecker answers for a folder that still holds
// entries: the client is told STATUS_DIRECTORY_NOT_EMPTY when it marks the folder, as
// MS-FSA's FileDispositionInformation has it (I-236), rather than success and a folder
// that stays.
var ErrDirectoryNotEmpty = errors.New("smb: the directory is not empty")

func statusFor(err error, fallback uint32) uint32 {
	switch {
	case err == nil:
		return statusSuccess
	case errors.Is(err, os.ErrNotExist):
		return statusObjectNameNotFound
	case errors.Is(err, os.ErrExist):
		return statusObjectNameCollision
	case errors.Is(err, os.ErrPermission):
		return statusAccessDenied
	case errors.Is(err, ErrDirectoryNotEmpty):
		return statusDirectoryNotEmpty
	case isDiskFull(err):
		return statusDiskFull
	case errors.Is(err, io.EOF):
		return statusEndOfFile
	default:
		return fallback
	}
}

// isDiskFull reports whether err is the filesystem saying it has no room --
// no space, no quota left, or a file grown past what it allows -- however
// many layers of wrapping a driver put around the errno.
func isDiskFull(err error) bool {
	for _, target := range diskFullErrnos {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
