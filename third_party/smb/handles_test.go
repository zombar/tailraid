package smb

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// tinyFS is the smallest filesystem that can be opened, listed and written:
// one directory, one file, and errors for everything else.
type tinyFS struct {
	body    []byte
	failing bool
}

func (f *tinyFS) Close() error  { return nil }
func (f *tinyFS) Label() string { return "TINY" }
func (f *tinyFS) ReadFile(p string) ([]byte, error) {
	if f.failing {
		return nil, os.ErrPermission
	}
	if p == "/file.txt" {
		return f.body, nil
	}
	return nil, os.ErrNotExist
}
func (f *tinyFS) WriteFile(p string, d []byte, _ os.FileMode) error {
	if f.failing {
		return os.ErrPermission
	}
	if p == "/file.txt" {
		f.body = append([]byte(nil), d...)
		return nil
	}
	return os.ErrPermission
}
func (f *tinyFS) ListDir(p string) ([]filesystem.DirEntry, error) {
	if p != "/" {
		return nil, os.ErrNotExist
	}
	return []filesystem.DirEntry{filesystem.NewDirEntry(2, "file.txt", 1)}, nil
}
func (f *tinyFS) Stat(p string) (filesystem.Stat, error) {
	switch p {
	case "/":
		return filesystem.NewStat(0o040755, 0, 1), nil
	case "/file.txt":
		return filesystem.NewStat(0o100644, uint64(len(f.body)), 2), nil
	}
	return nil, os.ErrNotExist
}
func (f *tinyFS) MkDir(string, os.FileMode) error { return os.ErrPermission }
func (f *tinyFS) DeleteFile(string) error         { return nil }
func (f *tinyFS) DeleteDir(string) error          { return nil }
func (f *tinyFS) Rename(string, string) error {
	if f.failing {
		return os.ErrPermission
	}
	return nil
}
func (f *tinyFS) ReadLink(string) (string, error) { return "", os.ErrInvalid }

// opened builds a connection with one share and one handle on it, which is
// what every command below needs and none of them is about.
func opened(t *testing.T, fsys filesystem.Filesystem, path string, ro bool) (*conn, *openFile) {
	t.Helper()
	c := newConn(New(), nil)
	sh := &share{name: "disk", fsys: fsys, ro: ro}
	c.trees[1] = &treeConn{sh: sh, ro: ro}
	st, err := fsys.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	of := &openFile{path: path, share: sh, ro: ro, dir: isDir(st)}
	of.id[0] = 1
	c.files[of.id] = of
	c.lastFile = of.id
	return c, of
}

func request(cmd command, tree uint32, body []byte) []byte {
	h := responseTo(header{command: cmd, treeID: tree}, statusSuccess)
	binary.LittleEndian.PutUint32(h[offFlags:], 0)
	return append(h, body...)
}

func statusOf(t *testing.T, out []byte) uint32 {
	t.Helper()
	h, err := parseHeader(out)
	if err != nil {
		t.Fatalf("the reply is not a message: %v", err)
	}
	return h.status
}

// Every information class a client asks about a file, and the refusal for one
// this server does not write.
func TestQueryInfoClasses(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, of := opened(t, fs, "/file.txt", false)

	ask := func(infoType, class uint8) []byte {
		body := make([]byte, 40)
		body[2], body[3] = infoType, class
		copy(body[24:], of.id[:])
		out, err := c.dispatch(request(cmdQueryInfo, 1, body))
		if err != nil {
			t.Fatalf("type %d class %d: %v", infoType, class, err)
		}
		return out
	}
	for _, class := range []uint8{
		fileBasicInformation, fileStandardInformation, fileInternalInformation,
		fileEaInformation, fileAccessInformation, fileNameInformation,
		fileAlignmentInformation, filePositionInformation,
		fileNetworkOpenInformation, fileStreamInformation, fileAllInformation,
	} {
		out := ask(infoTypeFile, class)
		if st := statusOf(t, out); st != statusSuccess {
			t.Errorf("file class %d answered %#x", class, st)
		}
		if n := binary.LittleEndian.Uint32(out[headerLen+4:]); n == 0 && class != fileStreamInformation {
			t.Errorf("file class %d answered with nothing in it", class)
		}
	}
	for _, class := range []uint8{
		fsVolumeInformation, fsSizeInformation, fsFullSizeInformation,
		fsDeviceInformation, fsAttributeInformation,
	} {
		if st := statusOf(t, ask(infoTypeFilesystem, class)); st != statusSuccess {
			t.Errorf("filesystem class %d answered %#x", class, st)
		}
	}
	if st := statusOf(t, ask(infoTypeFile, 99)); st != statusInvalidInfoClass {
		t.Errorf("an unknown file class answered %#x", st)
	}
	if st := statusOf(t, ask(infoTypeSecurity, 0)); st != statusNotSupported {
		t.Errorf("a security descriptor answered %#x, want NOT_SUPPORTED", st)
	}

	// A directory's stream information is empty rather than an error, and its
	// standard information says it is one.
	cd, dirOf := opened(t, fs, "/", false)
	dirBody := make([]byte, 40)
	dirBody[2], dirBody[3] = infoTypeFile, fileStandardInformation
	copy(dirBody[24:], dirOf.id[:])
	out, err := cd.dispatch(request(cmdQueryInfo, 1, dirBody))
	if err != nil {
		t.Fatal(err)
	}
	if out[headerLen+8+21] != 1 {
		t.Error("a directory's standard information does not say it is one")
	}
}

// Deleting, renaming and truncating are all SET_INFO: SMB2 has no command for
// any of the three.
func TestSetInfoIsHowThingsChange(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, of := opened(t, fs, "/file.txt", false)

	set := func(class uint8, in []byte) uint32 {
		body := make([]byte, 32)
		body[2], body[3] = infoTypeFile, class
		binary.LittleEndian.PutUint32(body[4:], uint32(len(in)))
		binary.LittleEndian.PutUint16(body[8:], uint16(headerLen+32))
		copy(body[16:], of.id[:])
		out, err := c.dispatch(request(cmdSetInfo, 1, append(body, in...)))
		if err != nil {
			t.Fatalf("class %d: %v", class, err)
		}
		return statusOf(t, out)
	}

	if st := set(fileDispositionInformation, []byte{1}); st != statusSuccess {
		t.Errorf("marking delete-on-close answered %#x", st)
	}
	if !of.deleteOnClose {
		t.Error("the handle was not marked delete-on-close")
	}

	rename := make([]byte, 20)
	name := utf16le(`renamed.txt`)
	binary.LittleEndian.PutUint32(rename[16:], uint32(len(name)))
	if st := set(fileRenameInformation, append(rename, name...)); st != statusSuccess {
		t.Errorf("renaming answered %#x", st)
	}
	if of.path != "/renamed.txt" {
		t.Errorf("the handle still calls itself %q", of.path)
	}
	of.path = "/file.txt"

	size := make([]byte, 8)
	binary.LittleEndian.PutUint64(size, 3)
	if st := set(fileEndOfFileInformation, size); st != statusSuccess {
		t.Errorf("truncating answered %#x", st)
	}
	if string(fs.body) != "hel" {
		t.Errorf("the file holds %q after being truncated to 3", fs.body)
	}

	// Timestamps and attributes are accepted and dropped: a client sets them
	// at the end of every copy, and refusing there turns a good copy into a
	// reported failure.
	if st := set(fileBasicInformationSet, make([]byte, 40)); st != statusSuccess {
		t.Errorf("setting times answered %#x", st)
	}
	if st := set(99, nil); st != statusInvalidInfoClass {
		t.Errorf("an unknown class answered %#x", st)
	}
	for _, class := range []uint8{fileDispositionInformation, fileRenameInformation, fileEndOfFileInformation} {
		if st := set(class, nil); st != statusInvalidParameter {
			t.Errorf("class %d with an empty payload answered %#x", class, st)
		}
	}

	// A share exported read-only refuses all of it, before touching anything.
	ro, roOf := opened(t, fs, "/file.txt", true)
	roBody := make([]byte, 32)
	roBody[2], roBody[3] = infoTypeFile, fileDispositionInformation
	copy(roBody[16:], roOf.id[:])
	out, err := ro.dispatch(request(cmdSetInfo, 1, roBody))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusMediaWriteProtected {
		t.Errorf("a read-only share answered %#x", st)
	}
}

// The dispositions: what to do about a file that is or is not there.
func TestCreateDispositions(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, _ := opened(t, fs, "/", false)

	create := func(name string, disp, opts uint32) uint32 {
		n := utf16le(name)
		body := make([]byte, 56)
		binary.LittleEndian.PutUint32(body[36:], disp)
		binary.LittleEndian.PutUint32(body[40:], opts)
		binary.LittleEndian.PutUint16(body[44:], uint16(headerLen+56))
		binary.LittleEndian.PutUint16(body[46:], uint16(len(n)))
		out, err := c.dispatch(request(cmdCreate, 1, append(body, n...)))
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		return statusOf(t, out)
	}

	if st := create("file.txt", dispOpen, 0); st != statusSuccess {
		t.Errorf("opening a file that is there answered %#x", st)
	}
	if st := create("file.txt", dispCreate, 0); st != statusObjectNameCollision {
		t.Errorf("creating a file that is there answered %#x", st)
	}
	if st := create("missing.txt", dispOpen, 0); st != statusObjectNameNotFound {
		t.Errorf("opening a file that is not there answered %#x", st)
	}
	if st := create("file.txt", dispOverwriteIf, 0); st != statusSuccess {
		t.Errorf("overwriting answered %#x", st)
	}
	if len(fs.body) != 0 {
		t.Errorf("overwriting left %d bytes", len(fs.body))
	}
	// Asking for a directory and finding a file, and the other way round.
	if st := create("file.txt", dispOpen, optDirectoryFile); st != statusNotADirectory {
		t.Errorf("opening a file as a directory answered %#x", st)
	}
	if st := create("", dispOpen, optNonDirectoryFile); st != statusFileIsADirectory {
		t.Errorf("opening the root as a file answered %#x", st)
	}
	// A path that is not one.
	if st := create(`file.txt:stream`, dispOpen, 0); st != statusObjectNameNotFound {
		t.Errorf("a stream name answered %#x", st)
	}
	// A read-only share refuses anything that would write, before looking.
	ro, _ := opened(t, fs, "/", true)
	n := utf16le("new.txt")
	body := make([]byte, 56)
	binary.LittleEndian.PutUint32(body[36:], dispCreate)
	binary.LittleEndian.PutUint16(body[44:], uint16(headerLen+56))
	binary.LittleEndian.PutUint16(body[46:], uint16(len(n)))
	out, err := ro.dispatch(request(cmdCreate, 1, append(body, n...)))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusMediaWriteProtected {
		t.Errorf("creating on a read-only share answered %#x", st)
	}
}

// Reads and writes addressed to the wrong kind of thing.
func TestReadAndWriteRefusals(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, dirOf := opened(t, fs, "/", false)

	body := make([]byte, 48)
	binary.LittleEndian.PutUint32(body[4:], 10)
	copy(body[16:], dirOf.id[:])
	out, err := c.dispatch(request(cmdRead, 1, body))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusInvalidDeviceRequest {
		t.Errorf("reading a directory answered %#x", st)
	}
	if out, err = c.dispatch(request(cmdWrite, 1, body)); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusInvalidDeviceRequest {
		t.Errorf("writing to a directory answered %#x", st)
	}

	// Reading past the end says END_OF_FILE rather than succeeding with
	// nothing, which a client reads as "keep going".
	cf, fileOf := opened(t, fs, "/file.txt", false)
	rb := make([]byte, 48)
	binary.LittleEndian.PutUint32(rb[4:], 10)
	binary.LittleEndian.PutUint64(rb[8:], 500)
	copy(rb[16:], fileOf.id[:])
	if out, err = cf.dispatch(request(cmdRead, 1, rb)); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusEndOfFile {
		t.Errorf("reading past the end answered %#x", st)
	}
	// A negative offset is refused rather than wrapped.
	binary.LittleEndian.PutUint64(rb[8:], 1<<63)
	if out, err = cf.dispatch(request(cmdRead, 1, rb)); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusInvalidParameter {
		t.Errorf("a negative offset answered %#x", st)
	}
	// A driver that refuses is reported, not retried.
	fs.failing = true
	binary.LittleEndian.PutUint64(rb[8:], 0)
	if out, err = cf.dispatch(request(cmdRead, 1, rb)); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusAccessDenied {
		t.Errorf("a driver refusing a read answered %#x", st)
	}
}

// A listing is read in pages, and the end of it is NO_MORE_FILES.
func TestListingPagesAndEnds(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, of := opened(t, fs, "/", false)

	page := func(class uint8, flags uint8, max uint32, pattern string) []byte {
		p := utf16le(pattern)
		body := make([]byte, 32)
		body[2], body[3] = class, flags
		copy(body[8:], of.id[:])
		binary.LittleEndian.PutUint16(body[24:], uint16(headerLen+32))
		binary.LittleEndian.PutUint16(body[26:], uint16(len(p)))
		binary.LittleEndian.PutUint32(body[28:], max)
		out, err := c.dispatch(request(cmdQueryDirectory, 1, append(body, p...)))
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		return out
	}
	out := page(infoDirectoryIDBoth, 0, 4096, "*")
	if st := statusOf(t, out); st != statusSuccess {
		t.Fatalf("the first page answered %#x", st)
	}
	// "." and ".." are there, because a client that does not see them
	// concludes it is not looking at a directory.
	if n := binary.LittleEndian.Uint32(out[headerLen+4:]); n < 3*104 {
		t.Errorf("the page is %d bytes: too small for . , .. and the file", n)
	}
	if st := statusOf(t, page(infoDirectoryIDBoth, 0, 4096, "*")); st != statusNoMoreFiles {
		t.Error("a second page did not end the listing")
	}
	// Restarting starts again.
	if st := statusOf(t, page(infoDirectoryIDBoth, restartScans, 4096, "*")); st != statusSuccess {
		t.Error("restarting did not start the listing again")
	}
	// A pattern that matches nothing still has "." and ".." in it, so the
	// listing is not empty; one that matches only the file filters the rest.
	if st := statusOf(t, page(infoDirectoryIDBoth, restartScans, 4096, "*.txt")); st != statusSuccess {
		t.Error("a pattern that matches the file returned nothing")
	}
	// An output buffer too small for even one entry is refused rather than
	// answered with an empty page.
	if st := statusOf(t, page(infoDirectoryIDBoth, restartScans, 8, "*")); st != statusInvalidParameter {
		t.Error("a buffer too small for one entry was answered anyway")
	}
	if st := statusOf(t, page(99, restartScans, 4096, "*")); st != statusInvalidInfoClass {
		t.Error("a class this server does not write was answered anyway")
	}
	// Listing something that is not a directory.
	cf, _ := opened(t, fs, "/file.txt", false)
	fileBody := make([]byte, 32)
	copy(fileBody[8:], cf.lastFile[:])
	out, err := cf.dispatch(request(cmdQueryDirectory, 1, fileBody))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusNotADirectory {
		t.Errorf("listing a file answered %#x", st)
	}
}

// Closing releases the handle, and a handle marked delete-on-close takes the
// file with it.
func TestCloseAndDeleteOnClose(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, of := opened(t, fs, "/file.txt", false)
	of.deleteOnClose = true

	body := make([]byte, 24)
	copy(body[8:], of.id[:])
	out, err := c.dispatch(request(cmdClose, 1, body))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusSuccess {
		t.Errorf("closing answered %#x", st)
	}
	if len(c.files) != 0 {
		t.Error("the handle is still open after being closed")
	}
	if out, err = c.dispatch(request(cmdClose, 1, body)); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusFileClosed {
		t.Errorf("closing a handle twice answered %#x", st)
	}
	// Flush answers success even with nothing to do: a client reads a failed
	// flush as data loss.
	if st := statusOf(t, mustDispatch(t, c, cmdFlush, make([]byte, 24))); st != statusSuccess {
		t.Error("flush did not answer success")
	}
	if st := statusOf(t, mustDispatch(t, c, cmdFlush, nil)); st != statusSuccess {
		t.Error("flush with no body did not answer success")
	}
}

func mustDispatch(t *testing.T, c *conn, cmd command, body []byte) []byte {
	t.Helper()
	out, err := c.dispatch(request(cmd, 1, body))
	if err != nil {
		t.Fatalf("%v: %v", cmd, err)
	}
	return out
}

// A chained request speaks about what the one before it opened.
func TestChainedRequestsInheritTheHandle(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, _ := opened(t, fs, "/", false)

	name := utf16le("file.txt")
	create := make([]byte, 56)
	binary.LittleEndian.PutUint32(create[36:], dispOpen)
	binary.LittleEndian.PutUint16(create[44:], uint16(headerLen+56))
	binary.LittleEndian.PutUint16(create[46:], uint16(len(name)))
	first := request(cmdCreate, 1, append(create, name...))
	for len(first)%8 != 0 {
		first = append(first, 0)
	}
	binary.LittleEndian.PutUint32(first[offNextCommand:], uint32(len(first)))

	query := make([]byte, 40)
	query[2], query[3] = infoTypeFile, fileStandardInformation
	copy(query[24:], allOnesFileID[:]) // "the file the previous operation opened"
	second := request(cmdQueryInfo, 0, query)
	binary.LittleEndian.PutUint32(second[offFlags:], flagRelatedOps)

	out, err := c.dispatchChain(append(first, second...))
	if err != nil {
		t.Fatalf("the chain: %v", err)
	}
	h, err := parseHeader(out)
	if err != nil {
		t.Fatal(err)
	}
	if h.status != statusSuccess || h.nextCommand == 0 {
		t.Fatalf("the first reply is %#x with next=%d; a chain of two should answer twice", h.status, h.nextCommand)
	}
	if st := statusOf(t, out[h.nextCommand:]); st != statusSuccess {
		t.Errorf("the chained QUERY_INFO answered %#x: it did not find the handle", st)
	}
	// A chain whose next-command offset points nowhere is an error, not a
	// wrong answer.
	broken := append([]byte(nil), first...)
	binary.LittleEndian.PutUint32(broken[offNextCommand:], 1<<20)
	if _, err := c.dispatchChain(broken); err == nil || !strings.Contains(err.Error(), "chained") {
		t.Errorf("a chain pointing past the end: %v", err)
	}
}

// A chained request whose predecessor FAILED is not carried out.
//
// This is a rename from Windows, in two messages. Windows checks that the
// target name is free with a compounded CREATE + CLOSE, and the CREATE is
// SUPPOSED to fail. The CLOSE that follows carries an all-ones file id --
// "the file the previous operation opened" -- and there is no such file, so a
// server that runs it anyway closes whatever this connection opened last: the
// source file, which the client is still holding. The client's next SET_INFO
// then comes back FILE_CLOSED and the rename fails with "The handle is
// invalid", about a handle the server shut behind its back.
//
// Nothing else found this. macOS and Linux never send a chain whose first
// operation fails, and the Go client never chains at all.
func TestAChainStopsAtTheFirstFailure(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, of := opened(t, fs, "/file.txt", false)
	live := of.id

	// CREATE a name that is not there, with FILE_OPEN so it cannot be made.
	name := utf16le("nothing-here.txt")
	create := make([]byte, 56)
	binary.LittleEndian.PutUint32(create[36:], dispOpen)
	binary.LittleEndian.PutUint16(create[44:], uint16(headerLen+56))
	binary.LittleEndian.PutUint16(create[46:], uint16(len(name)))
	first := request(cmdCreate, 1, append(create, name...))
	for len(first)%8 != 0 {
		first = append(first, 0)
	}
	binary.LittleEndian.PutUint32(first[offNextCommand:], uint32(len(first)))

	closing := make([]byte, 24)
	copy(closing[8:], allOnesFileID[:])
	second := request(cmdClose, 0, closing)
	binary.LittleEndian.PutUint32(second[offFlags:], flagRelatedOps)

	out, err := c.dispatchChain(append(first, second...))
	if err != nil {
		t.Fatalf("the chain: %v", err)
	}
	h, err := parseHeader(out)
	if err != nil {
		t.Fatal(err)
	}
	if h.status != statusObjectNameNotFound {
		t.Fatalf("the CREATE answered %#x, want OBJECT_NAME_NOT_FOUND", h.status)
	}
	if h.nextCommand == 0 {
		t.Fatal("a chain of two answered once")
	}
	// The same reason twice: the client sees that the chain did not happen,
	// rather than a success for something that was never done.
	if st := statusOf(t, out[h.nextCommand:]); st != statusObjectNameNotFound {
		t.Errorf("the related CLOSE answered %#x, want the CREATE's own status", st)
	}
	// And the point of all of it: the live handle is still open.
	if c.files[live] == nil {
		t.Error("the related CLOSE shut a handle the client still holds")
	}
	// A failed CREATE also leaves no "file the previous operation opened",
	// so an all-ones id on its own finds nothing rather than that handle.
	if c.fileByID(allOnesFileID[:]) != nil {
		t.Error("after a failed CREATE, an all-ones file id still resolves to something")
	}
}

// IPC$ is not a share anybody exported, and connecting to it has to succeed:
// the Linux kernel's client asks for it before it will use a real share, and a
// refusal costs the whole mount.
func TestTheIPCShareConnects(t *testing.T) {
	c := newConn(New(), nil)
	c.sessions[0] = &session{user: "alice"}

	connect := func(name string) []byte {
		n := utf16le(`\\127.0.0.1\` + name)
		body := make([]byte, 8)
		binary.LittleEndian.PutUint16(body[4:], uint16(headerLen+8))
		binary.LittleEndian.PutUint16(body[6:], uint16(len(n)))
		out, err := c.dispatch(request(cmdTreeConnect, 0, append(body, n...)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}

	for _, name := range []string{"IPC$", "ipc$"} {
		out := connect(name)
		if st := statusOf(t, out); st != statusSuccess {
			t.Fatalf("%s answered %#x", name, st)
		}
		if kind := out[headerLen+2]; kind != shareTypePipe {
			t.Errorf("%s came back as share type %d, want a pipe", name, kind)
		}
	}
	// A share nobody exported is still refused by name.
	if st := statusOf(t, connect("nope")); st != statusBadNetworkName {
		t.Errorf("a share that is not there answered %#x", st)
	}

	// There is ONE pipe behind it, and the others are refused by name -- not
	// followed into a filesystem that is not there.
	h, _ := parseHeader(connect("IPC$"))
	open := func(pipe string) uint32 {
		name := utf16le(pipe)
		body := make([]byte, 56)
		binary.LittleEndian.PutUint16(body[44:], uint16(headerLen+56))
		binary.LittleEndian.PutUint16(body[46:], uint16(len(name)))
		out, err := c.dispatch(request(cmdCreate, h.treeID, append(body, name...)))
		if err != nil {
			t.Fatal(err)
		}
		return statusOf(t, out)
	}
	for _, pipe := range []string{"srvsvc", "SRVSVC", `\srvsvc`} {
		if st := open(pipe); st != statusSuccess {
			t.Errorf("opening %s answered %#x", pipe, st)
		}
	}
	for _, pipe := range []string{"wkssvc", "lsarpc", "samr", "spoolss"} {
		if st := open(pipe); st != statusObjectNameNotFound {
			t.Errorf("opening %s answered %#x, want OBJECT_NAME_NOT_FOUND", pipe, st)
		}
	}
}

// A closed handle takes its half-read listing with it. The state is per
// handle and unbounded otherwise: a file manager opens and closes directories
// all day, and each one holds a Stat for every entry it found.
func TestClosingAHandleFreesItsListing(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, of := opened(t, fs, "/", false)

	list := make([]byte, 32)
	list[2] = infoDirectoryIDBoth
	copy(list[8:], of.id[:])
	binary.LittleEndian.PutUint16(list[24:], uint16(headerLen+32))
	binary.LittleEndian.PutUint32(list[28:], 4096)
	if st := statusOf(t, mustDispatch(t, c, cmdQueryDirectory, list)); st != statusSuccess {
		t.Fatal("the listing did not start")
	}
	if len(c.searches) != 1 {
		t.Fatalf("the connection remembers %d listings, want 1", len(c.searches))
	}

	closeBody := make([]byte, 24)
	copy(closeBody[8:], of.id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdClose, closeBody)); st != statusSuccess {
		t.Fatal("closing failed")
	}
	if len(c.searches) != 0 {
		t.Errorf("the listing outlived the handle: %d left", len(c.searches))
	}
}

// Byte-range locks: what an application uses to say "this part of the file is
// mine for a moment". The rules are simple and the consequences are not, so
// each one is checked.
func TestByteRangeLocks(t *testing.T) {
	fs := &tinyFS{body: make([]byte, 4096)}
	c, of := opened(t, fs, "/file.txt", false)

	// A second handle on the same file, which is what a second client is from
	// the lock table's point of view.
	other := &openFile{path: "/file.txt", share: of.share}
	other.id[0] = 2
	c.files[other.id] = other

	ask := func(handle *openFile, off, length uint64, flags uint32) uint32 {
		body := make([]byte, 24+24)
		binary.LittleEndian.PutUint16(body[2:], 1)
		copy(body[8:], handle.id[:])
		binary.LittleEndian.PutUint64(body[24:], off)
		binary.LittleEndian.PutUint64(body[32:], length)
		binary.LittleEndian.PutUint32(body[40:], flags)
		return statusOf(t, mustDispatch(t, c, cmdLock, body))
	}

	if st := ask(of, 0, 100, lockExclusive|lockFailImmediately); st != statusSuccess {
		t.Fatalf("taking a lock answered %#x", st)
	}
	// The same handle may extend its own reservation.
	if st := ask(of, 50, 100, lockExclusive|lockFailImmediately); st != statusSuccess {
		t.Errorf("a handle conflicted with itself: %#x", st)
	}
	// Another handle may not.
	if st := ask(other, 50, 10, lockExclusive|lockFailImmediately); st != statusLockNotGranted {
		t.Errorf("an overlapping exclusive lock answered %#x", st)
	}
	if st := ask(other, 50, 10, lockShared|lockFailImmediately); st != statusLockNotGranted {
		t.Errorf("a shared lock over an exclusive one answered %#x", st)
	}
	// …but a range that does not overlap is free.
	if st := ask(other, 1000, 10, lockExclusive|lockFailImmediately); st != statusSuccess {
		t.Errorf("a lock elsewhere in the file answered %#x", st)
	}

	// Shared locks coexist.
	if st := ask(of, 2000, 100, lockShared|lockFailImmediately); st != statusSuccess {
		t.Fatalf("a shared lock answered %#x", st)
	}
	if st := ask(other, 2050, 10, lockShared|lockFailImmediately); st != statusSuccess {
		t.Errorf("two shared locks did not coexist: %#x", st)
	}
	if st := ask(other, 2050, 10, lockExclusive|lockFailImmediately); st != statusLockNotGranted {
		t.Errorf("an exclusive lock over a shared one answered %#x", st)
	}

	// Unlocking a range nobody holds says so, rather than succeeding: the two
	// sides disagree about what is locked.
	if st := ask(other, 3000, 10, lockUnlock); st != statusRangeNotLocked {
		t.Errorf("unlocking an unheld range answered %#x", st)
	}
	if st := ask(of, 0, 100, lockUnlock); st != statusSuccess {
		t.Errorf("unlocking answered %#x", st)
	}

	// Closing a handle drops what it held: nobody has to ask.
	closeBody := make([]byte, 24)
	copy(closeBody[8:], of.id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdClose, closeBody)); st != statusSuccess {
		t.Fatal("closing failed")
	}
	if st := ask(other, 50, 10, lockExclusive|lockFailImmediately); st != statusSuccess {
		t.Errorf("a lock survived the handle that took it: %#x", st)
	}

	// Malformed requests are refused rather than half-applied.
	if st := statusOf(t, mustDispatch(t, c, cmdLock, make([]byte, 24))); st != statusFileClosed {
		t.Errorf("a lock on no handle answered %#x", st)
	}
	body := make([]byte, 24)
	copy(body[8:], other.id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdLock, body)); st != statusInvalidParameter {
		t.Errorf("a request with no elements answered %#x", st)
	}
}

// A request for several ranges is applied whole or not at all: a client that
// asked for three and got two would have no way to know which.
func TestASplitLockRequestIsUndone(t *testing.T) {
	fs := &tinyFS{body: make([]byte, 4096)}
	c, of := opened(t, fs, "/file.txt", false)
	blocker := &openFile{path: "/file.txt", share: of.share}
	blocker.id[0] = 9
	c.files[blocker.id] = blocker

	// Somebody else holds the second of the two ranges.
	held := make([]byte, 48)
	binary.LittleEndian.PutUint16(held[2:], 1)
	copy(held[8:], blocker.id[:])
	binary.LittleEndian.PutUint64(held[24:], 500)
	binary.LittleEndian.PutUint64(held[32:], 100)
	binary.LittleEndian.PutUint32(held[40:], lockExclusive|lockFailImmediately)
	if st := statusOf(t, mustDispatch(t, c, cmdLock, held)); st != statusSuccess {
		t.Fatal("the blocking lock was not taken")
	}

	both := make([]byte, 24+48)
	binary.LittleEndian.PutUint16(both[2:], 2)
	copy(both[8:], of.id[:])
	binary.LittleEndian.PutUint64(both[24:], 0)
	binary.LittleEndian.PutUint64(both[32:], 100)
	binary.LittleEndian.PutUint32(both[40:], lockExclusive|lockFailImmediately)
	binary.LittleEndian.PutUint64(both[48:], 500)
	binary.LittleEndian.PutUint64(both[56:], 100)
	binary.LittleEndian.PutUint32(both[64:], lockExclusive|lockFailImmediately)
	if st := statusOf(t, mustDispatch(t, c, cmdLock, both)); st != statusLockNotGranted {
		t.Fatalf("a request that could not be granted whole answered %#x", st)
	}

	// The first range must be free again: the failed request left nothing.
	free := make([]byte, 48)
	binary.LittleEndian.PutUint16(free[2:], 1)
	copy(free[8:], blocker.id[:])
	binary.LittleEndian.PutUint64(free[24:], 0)
	binary.LittleEndian.PutUint64(free[32:], 100)
	binary.LittleEndian.PutUint32(free[40:], lockExclusive|lockFailImmediately)
	if st := statusOf(t, mustDispatch(t, c, cmdLock, free)); st != statusSuccess {
		t.Errorf("the half-applied request left a lock behind: %#x", st)
	}
}

// A lock that nothing enforces is decoration: a read crosses a shared lock and
// stops at an exclusive one, and a write stops at either.
func TestLocksAreEnforcedOnReadsAndWrites(t *testing.T) {
	fs := &tinyFS{body: []byte("0123456789abcdefghij")}
	c, of := opened(t, fs, "/file.txt", false)
	other := &openFile{path: "/file.txt", share: of.share}
	other.id[0] = 2
	c.files[other.id] = other

	take := func(handle *openFile, off, length uint64, flags uint32) {
		t.Helper()
		body := make([]byte, 48)
		binary.LittleEndian.PutUint16(body[2:], 1)
		copy(body[8:], handle.id[:])
		binary.LittleEndian.PutUint64(body[24:], off)
		binary.LittleEndian.PutUint64(body[32:], length)
		binary.LittleEndian.PutUint32(body[40:], flags)
		if st := statusOf(t, mustDispatch(t, c, cmdLock, body)); st != statusSuccess {
			t.Fatalf("taking the lock answered %#x", st)
		}
	}
	read := func(handle *openFile, off, length uint64) uint32 {
		body := make([]byte, 48)
		binary.LittleEndian.PutUint32(body[4:], uint32(length))
		binary.LittleEndian.PutUint64(body[8:], off)
		copy(body[16:], handle.id[:])
		return statusOf(t, mustDispatch(t, c, cmdRead, body))
	}
	write := func(handle *openFile, off, length uint64) uint32 {
		body := make([]byte, 48+int(length))
		binary.LittleEndian.PutUint16(body[2:], uint16(headerLen+48))
		binary.LittleEndian.PutUint32(body[4:], uint32(length))
		binary.LittleEndian.PutUint64(body[8:], off)
		copy(body[16:], handle.id[:])
		return statusOf(t, mustDispatch(t, c, cmdWrite, body))
	}

	take(of, 0, 8, lockExclusive|lockFailImmediately)
	if st := read(other, 0, 4); st != statusFileLockConflict {
		t.Errorf("reading through somebody else's exclusive lock answered %#x", st)
	}
	if st := write(other, 0, 4); st != statusFileLockConflict {
		t.Errorf("writing through somebody else's exclusive lock answered %#x", st)
	}
	// The holder itself is not stopped.
	if st := read(of, 0, 4); st != statusSuccess {
		t.Errorf("the holder could not read its own locked range: %#x", st)
	}
	// Past the range, everyone is free.
	if st := read(other, 10, 4); st != statusSuccess {
		t.Errorf("reading outside the lock answered %#x", st)
	}

	// A shared lock lets readers through and stops writers.
	take(of, 12, 4, lockShared|lockFailImmediately)
	if st := read(other, 12, 4); st != statusSuccess {
		t.Errorf("reading through a shared lock answered %#x", st)
	}
	if st := write(other, 12, 4); st != statusFileLockConflict {
		t.Errorf("writing through a shared lock answered %#x", st)
	}
}

// A client that asks to WAIT for a lock is promised an answer and gets it when
// the holder lets go -- rather than being refused, which is all this could do
// before there was an asynchronous reply.
func TestWaitingForALock(t *testing.T) {
	fs := &tinyFS{body: make([]byte, 4096)}
	c, holder := opened(t, fs, "/file.txt", false)
	c.nc = &captureConn{}
	waiter := &openFile{path: "/file.txt", share: holder.share}
	waiter.id[0] = 2
	c.files[waiter.id] = waiter

	lockBody := func(handle *openFile, flags uint32) []byte {
		body := make([]byte, 48)
		binary.LittleEndian.PutUint16(body[2:], 1)
		copy(body[8:], handle.id[:])
		binary.LittleEndian.PutUint64(body[24:], 0)
		binary.LittleEndian.PutUint64(body[32:], 100)
		binary.LittleEndian.PutUint32(body[40:], flags)
		return body
	}

	if st := statusOf(t, mustDispatch(t, c, cmdLock, lockBody(holder, lockExclusive|lockFailImmediately))); st != statusSuccess {
		t.Fatal("the holder did not get the lock")
	}

	// The waiter asks without FAIL_IMMEDIATELY: it is asking to wait.
	interim := mustDispatch(t, c, cmdLock, lockBody(waiter, lockExclusive))
	h, err := parseHeader(interim)
	if err != nil {
		t.Fatal(err)
	}
	if h.status != statusPending {
		t.Fatalf("waiting answered %#x, want STATUS_PENDING", h.status)
	}
	if h.flags&flagAsyncCommand == 0 {
		t.Error("the interim reply is not marked async")
	}
	if h.asyncID == 0 {
		t.Error("the interim reply carries no AsyncId")
	}

	// The holder lets go, and the answer arrives on its own.
	if st := statusOf(t, mustDispatch(t, c, cmdLock, lockBody(holder, lockUnlock))); st != statusSuccess {
		t.Fatal("unlocking failed")
	}
	reply := (c.nc.(*captureConn)).await(t)
	final, err := parseHeader(reply)
	if err != nil {
		t.Fatal(err)
	}
	if final.status != statusSuccess {
		t.Errorf("the awaited lock answered %#x", final.status)
	}
	if final.asyncID != h.asyncID {
		t.Errorf("the answer carries AsyncId %d, the promise carried %d", final.asyncID, h.asyncID)
	}
	if final.messageID != h.messageID {
		t.Error("the answer does not match the request it answers")
	}
}

// A client that gives up is told so, rather than left waiting for an answer
// that is no longer coming.
func TestCancellingAWait(t *testing.T) {
	fs := &tinyFS{body: make([]byte, 4096)}
	c, holder := opened(t, fs, "/file.txt", false)
	c.nc = &captureConn{}
	waiter := &openFile{path: "/file.txt", share: holder.share}
	waiter.id[0] = 2
	c.files[waiter.id] = waiter

	body := make([]byte, 48)
	binary.LittleEndian.PutUint16(body[2:], 1)
	copy(body[8:], holder.id[:])
	binary.LittleEndian.PutUint64(body[32:], 100)
	binary.LittleEndian.PutUint32(body[40:], lockExclusive|lockFailImmediately)
	if st := statusOf(t, mustDispatch(t, c, cmdLock, body)); st != statusSuccess {
		t.Fatal("the holder did not get the lock")
	}

	wait := make([]byte, 48)
	binary.LittleEndian.PutUint16(wait[2:], 1)
	copy(wait[8:], waiter.id[:])
	binary.LittleEndian.PutUint64(wait[32:], 100)
	binary.LittleEndian.PutUint32(wait[40:], lockExclusive)
	interim := mustDispatch(t, c, cmdLock, wait)
	h, _ := parseHeader(interim)

	// CANCEL names the operation by the AsyncId it was promised under.
	req := responseTo(header{command: cmdCancel, messageID: h.messageID}, statusSuccess)
	binary.LittleEndian.PutUint32(req[offFlags:], flagAsyncCommand)
	binary.LittleEndian.PutUint64(req[offAsyncID:], h.asyncID)
	if _, err := c.dispatch(append(req, make([]byte, 4)...)); err != nil {
		t.Fatal(err)
	}
	reply := (c.nc.(*captureConn)).await(t)
	final, _ := parseHeader(reply)
	if final.status != statusCancelled {
		t.Errorf("a cancelled wait answered %#x, want STATUS_CANCELLED", final.status)
	}
	// Cancelling something that is not there is silence, not an error: it
	// finished between the client deciding and the message arriving.
	if out, err := c.dispatch(append(req, make([]byte, 4)...)); err != nil || out != nil {
		t.Errorf("cancelling twice gave %v, %v", out, err)
	}
}

// A file manager asks to be told when a directory changes instead of asking
// again every second. The answer comes when something happens.
func TestChangeNotify(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, dir := opened(t, fs, "/", false)
	c.nc = &captureConn{}

	watch := func(flags uint16, max uint32) header {
		t.Helper()
		body := make([]byte, 32)
		binary.LittleEndian.PutUint16(body[2:], flags)
		binary.LittleEndian.PutUint32(body[4:], max)
		copy(body[8:], dir.id[:])
		interim := mustDispatch(t, c, cmdChangeNotify, body)
		h, err := parseHeader(interim)
		if err != nil {
			t.Fatal(err)
		}
		if h.status != statusPending {
			t.Fatalf("CHANGE_NOTIFY answered %#x at once, want STATUS_PENDING", h.status)
		}
		return h
	}

	h := watch(0, 4096)
	// Something happens: a file is written through this server.
	writer := &openFile{path: "/file.txt", share: dir.share}
	writer.id[0] = 7
	c.files[writer.id] = writer
	wb := make([]byte, 48+5)
	binary.LittleEndian.PutUint16(wb[2:], uint16(headerLen+48))
	binary.LittleEndian.PutUint32(wb[4:], 5)
	copy(wb[16:], writer.id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdWrite, wb)); st != statusSuccess {
		t.Fatal("the write failed")
	}

	reply := (c.nc.(*captureConn)).await(t)
	final, err := parseHeader(reply)
	if err != nil {
		t.Fatal(err)
	}
	if final.status != statusSuccess {
		t.Fatalf("the notification answered %#x", final.status)
	}
	if final.asyncID != h.asyncID {
		t.Errorf("the notification carries AsyncId %d, the promise carried %d", final.asyncID, h.asyncID)
	}
	// The entry names the file, relative to the directory being watched, and
	// says what happened to it.
	out := reply[headerLen+8:]
	if action := binary.LittleEndian.Uint32(out[4:]); action != actionModified {
		t.Errorf("the action is %d, want modified", action)
	}
	n := int(binary.LittleEndian.Uint32(out[8:]))
	if got := fromUTF16le(out[12 : 12+n]); got != "file.txt" {
		t.Errorf("the notification names %q", got)
	}

	// A burst bigger than the buffer the client offered is answered with
	// "look again" rather than a list with holes in it.
	h = watch(0, 8)
	for i := 0; i < 4; i++ {
		if st := statusOf(t, mustDispatch(t, c, cmdWrite, wb)); st != statusSuccess {
			t.Fatal("a write failed")
		}
	}
	reply = (c.nc.(*captureConn)).await(t)
	if final, _ = parseHeader(reply); final.status != statusNotifyEnumDir {
		t.Errorf("an overflowing notification answered %#x, want NOTIFY_ENUM_DIR", final.status)
	}

	// Closing the handle ends the watch rather than leaving a goroutine on it.
	h = watch(0, 4096)
	closeBody := make([]byte, 24)
	copy(closeBody[8:], dir.id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdClose, closeBody)); st != statusSuccess {
		t.Fatal("closing failed")
	}
	// The goroutine removes itself; give it the moment it needs.
	for i := 0; i < 200 && dir.share.watchers.count() != 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if n := dir.share.watchers.count(); n != 0 {
		t.Errorf("%d watches outlived the handle they were taken on", n)
	}

	// A watch on something that is not a directory, and on no handle at all.
	notDir := &openFile{path: "/file.txt", share: dir.share}
	notDir.id[0] = 8
	c.files[notDir.id] = notDir
	body := make([]byte, 32)
	copy(body[8:], notDir.id[:])
	if st := statusOf(t, mustDispatch(t, c, cmdChangeNotify, body)); st != statusInvalidParameter {
		t.Errorf("watching a file answered %#x", st)
	}
	if st := statusOf(t, mustDispatch(t, c, cmdChangeNotify, make([]byte, 32))); st != statusFileClosed {
		t.Errorf("watching no handle answered %#x", st)
	}
}
