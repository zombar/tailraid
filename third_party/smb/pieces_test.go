package smb

import (
	"encoding/binary"
	"errors"
	"github.com/go-filesystems/interface"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// An SMB path is backslashed, relative, and must not leave the share.
func TestPathTranslation(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "/", true},
		{`file.txt`, "/file.txt", true},
		{`sub\dir\file.txt`, "/sub/dir/file.txt", true},
		{`sub\..\file.txt`, "/file.txt", true},
		// Climbing above the root lands AT the root, which is what a
		// filesystem does with "/../x" -- the share is the world.
		{`..\..\file.txt`, "/file.txt", true},
		// A stream name is not a path, and there are no streams here.
		{`file.txt:hidden:$DATA`, "", false},
	} {
		got, ok := smbPathToFS(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("smbPathToFS(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if got := fsPathToSMB("/sub/file.txt"); got != `sub\file.txt` {
		t.Errorf("fsPathToSMB = %q", got)
	}
	if got := baseName("/"); got != "" {
		t.Errorf("baseName(/) = %q", got)
	}
	if got := baseName("/a/b.txt"); got != "b.txt" {
		t.Errorf("baseName = %q", got)
	}
}

// What a client reads instead of the mode.
func TestAttributes(t *testing.T) {
	dir := filesystem.NewStat(0o040755, 4096, 1)
	file := filesystem.NewStat(0o100644, 1234, 2)
	locked := filesystem.NewStat(0o100444, 10, 3)
	noType := filesystem.NewStat(0o644, 7, 4)

	if a := attributesOf(dir, false); a&attrDirectory == 0 {
		t.Errorf("a directory is %#x", a)
	}
	if a := attributesOf(file, false); a&attrDirectory != 0 || a&attrReadOnly != 0 {
		t.Errorf("a writable file is %#x", a)
	}
	if a := attributesOf(locked, false); a&attrReadOnly == 0 {
		t.Error("a file with no write bit is not marked read-only")
	}
	if a := attributesOf(file, true); a&attrReadOnly == 0 {
		t.Error("a file on a read-only share is not marked read-only")
	}
	// A mode with no type bits is a file: the only guess that cannot make a
	// client try to enumerate something that is not a directory.
	if a := attributesOf(noType, false); a&attrDirectory != 0 {
		t.Errorf("a mode with no type bits came out as %#x", a)
	}
	if sizeOf(dir) != 0 {
		t.Error("a directory reported a size")
	}
	if sizeOf(file) != 1234 {
		t.Error("a file lost its size")
	}
	for _, tc := range []struct{ in, want uint64 }{{0, 0}, {1, 4096}, {4096, 4096}, {4097, 8192}} {
		if got := allocationOf(tc.in); got != tc.want {
			t.Errorf("allocationOf(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A driver's error becomes the status a client acts on, and the default is the
// caller's because what "not found" means depends on what was asked.
func TestStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want uint32
	}{
		{nil, statusSuccess},
		{os.ErrNotExist, statusObjectNameNotFound},
		{os.ErrExist, statusObjectNameCollision},
		{os.ErrPermission, statusAccessDenied},
		{io.EOF, statusEndOfFile},
		{errors.New("something else"), statusAccessDenied},
	} {
		if got := statusFor(tc.err, statusAccessDenied); got != tc.want {
			t.Errorf("statusFor(%v) = %#x, want %#x", tc.err, got, tc.want)
		}
	}
}

// The wildcards a client sends, compared the way the protocol compares them:
// without case.
func TestWildcards(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything.txt", true},
		{"*.txt", "notes.TXT", true},
		{"*.txt", "notes.doc", false},
		{"note?.txt", "note1.txt", true},
		{"note?.txt", "note12.txt", false},
		{"[", "x", false}, // a pattern that does not compile matches nothing
	} {
		if got := matchSMB(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchSMB(%q, %q) = %v", tc.pattern, tc.name, got)
		}
	}
}

// Every directory entry class this server writes, and the refusal for one it
// does not.
func TestDirectoryEntryLayout(t *testing.T) {
	e := dirEntry{name: "hello.txt", st: filesystem.NewStat(0o100644, 100, 42)}
	for _, tc := range []struct {
		class uint8
		fixed int
	}{
		{infoDirectoryIDBoth, 104},
		{infoDirectoryBoth, 94},
		{infoDirectoryIDFull, 80}, // what the Linux kernel's client asks for
		{infoDirectoryFull, 68},
		{infoDirectoryPlain, 64},
		{infoDirectoryNames, 12},
	} {
		b := encodeDirEntry(tc.class, e, false)
		if b == nil {
			t.Fatalf("class %d produced nothing", tc.class)
		}
		if len(b)%8 != 0 {
			t.Errorf("class %d: an entry of %d bytes does not end on an eight-byte boundary", tc.class, len(b))
		}
		if len(b) < tc.fixed+len(utf16le(e.name)) {
			t.Errorf("class %d: %d bytes cannot hold %d of fixed fields and the name", tc.class, len(b), tc.fixed)
		}
		// The name is where the class says it is, or a client shows nothing.
		if !strings.Contains(string(b[tc.fixed:]), "h\x00e\x00l\x00l\x00o\x00") {
			t.Errorf("class %d does not carry the name at offset %d", tc.class, tc.fixed)
		}
	}
	if encodeDirEntry(99, e, false) != nil {
		t.Error("a class this server does not write produced an entry anyway")
	}
}

// Credits are what a client spends to ask for anything, so what is granted is
// what it asked for, bounded -- and never zero, which would wedge it.
func TestCredits(t *testing.T) {
	for _, tc := range []struct{ asked, want uint16 }{
		{0, 1}, {1, 1}, {256, 256}, {512, 512}, {5000, 512},
	} {
		if got := creditsFor(header{credits: tc.asked}); got != tc.want {
			t.Errorf("asked %d, granted %d, want %d", tc.asked, got, tc.want)
		}
	}
}

// A truncate goes through whichever capability the driver has, and the last
// path -- rewriting the whole file -- is the one every driver without either
// falls back to.
func TestTruncateTakesWhicheverPathThereIs(t *testing.T) {
	fs := &tinyFS{body: []byte("hello world")}
	c, of := opened(t, fs, "/file.txt", false)
	if err := c.truncate(of, 5); err != nil {
		t.Fatalf("the whole-file path: %v", err)
	}
	if string(fs.body) != "hello" {
		t.Errorf("the file holds %q", fs.body)
	}
	// Growing, through the same path.
	if err := c.truncate(of, 8); err != nil {
		t.Fatal(err)
	}
	if len(fs.body) != 8 {
		t.Errorf("growing left %d bytes", len(fs.body))
	}
	// A driver that refuses the read is reported.
	fs.failing = true
	if err := c.truncate(of, 1); err == nil {
		t.Error("a driver that refuses was not reported")
	}
	fs.failing = false

	// A driver with the positional capability uses it instead.
	tr := &truncatingFile{}
	of.w = tr
	if err := c.truncate(of, 3); err != nil {
		t.Fatal(err)
	}
	if tr.size != 3 {
		t.Errorf("the positional path was not used (size %d)", tr.size)
	}
	// …and flush reaches it too.
	if st := statusOf(t, mustDispatch(t, c, cmdFlush, flushBody(of))); st != statusSuccess {
		t.Error("flush did not answer success")
	}
	if !tr.synced {
		t.Error("flush did not reach the driver")
	}
	tr.err = os.ErrPermission
	if st := statusOf(t, mustDispatch(t, c, cmdFlush, flushBody(of))); st != statusAccessDenied {
		t.Error("a driver refusing a flush was not reported")
	}
}

func flushBody(of *openFile) []byte {
	b := make([]byte, 24)
	copy(b[8:], of.id[:])
	return b
}

// truncatingFile is the positional capability, and nothing else.
type truncatingFile struct {
	size   int64
	synced bool
	err    error
}

func (f *truncatingFile) ReadAt([]byte, int64) (int, error)      { return 0, io.EOF }
func (f *truncatingFile) WriteAt(p []byte, _ int64) (int, error) { return len(p), f.err }
func (f *truncatingFile) Close() error                           { return nil }
func (f *truncatingFile) Size() int64                            { return f.size }
func (f *truncatingFile) Truncate(n int64) error                 { f.size = n; return f.err }
func (f *truncatingFile) Sync() error                            { f.synced = true; return f.err }

// The third message of NTLM, in every shape that is not one.
func TestAuthMessageRefusals(t *testing.T) {
	good := make([]byte, 64)
	copy(good, ntlmSignature[:])
	binary.LittleEndian.PutUint32(good[8:], ntlmAuth)
	if _, err := parseAuth(good); err != nil {
		t.Fatalf("a minimal message did not parse: %v", err)
	}
	for _, tc := range []struct {
		name string
		fix  func([]byte)
	}{
		{"a field that runs past the end", func(b []byte) {
			binary.LittleEndian.PutUint16(b[20:], 40)
			binary.LittleEndian.PutUint32(b[24:], 1<<20)
		}},
		{"a domain that runs past the end", func(b []byte) {
			binary.LittleEndian.PutUint16(b[28:], 40)
			binary.LittleEndian.PutUint32(b[32:], 1<<20)
		}},
		{"a user that runs past the end", func(b []byte) {
			binary.LittleEndian.PutUint16(b[36:], 40)
			binary.LittleEndian.PutUint32(b[40:], 1<<20)
		}},
		{"a workstation that runs past the end", func(b []byte) {
			binary.LittleEndian.PutUint16(b[44:], 40)
			binary.LittleEndian.PutUint32(b[48:], 1<<20)
		}},
		{"a session key that runs past the end", func(b []byte) {
			binary.LittleEndian.PutUint16(b[52:], 40)
			binary.LittleEndian.PutUint32(b[56:], 1<<20)
		}},
		{"an LM response that runs past the end", func(b []byte) {
			binary.LittleEndian.PutUint16(b[12:], 40)
			binary.LittleEndian.PutUint32(b[16:], 1<<20)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			tc.fix(b)
			if _, err := parseAuth(b); err == nil {
				t.Error("it parsed")
			}
		})
	}
	if _, err := parseAuth(make([]byte, 8)); err == nil {
		t.Error("something too short to be a message parsed")
	}
}

// A session setup whose security buffer is not inside the message is an error,
// and one carrying something that is not NTLM is a logon failure -- not a
// crash, and not a success.
func TestSessionSetupRefusals(t *testing.T) {
	c := newConn(New(), nil)
	body := make([]byte, 24)
	binary.LittleEndian.PutUint16(body[12:], 1<<15) // an offset outside the message
	binary.LittleEndian.PutUint16(body[14:], 10)
	if _, err := c.dispatch(request(cmdSessionSetup, 0, body)); err == nil {
		t.Error("a security buffer outside the message was accepted")
	}

	// A buffer holding something that is not a token.
	blob := []byte{0x30, 0x02, 0x01, 0x02}
	body = make([]byte, 24)
	binary.LittleEndian.PutUint16(body[12:], uint16(headerLen+24))
	binary.LittleEndian.PutUint16(body[14:], uint16(len(blob)))
	out, err := c.dispatch(request(cmdSessionSetup, 0, append(body, blob...)))
	if err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusLogonFailure {
		t.Errorf("a token that is not one answered %#x", st)
	}

	// An NTLM message of a type this server does not expect.
	ntlm := append(append([]byte(nil), ntlmSignature[:]...), 9, 0, 0, 0)
	body = make([]byte, 24)
	binary.LittleEndian.PutUint16(body[12:], uint16(headerLen+24))
	binary.LittleEndian.PutUint16(body[14:], uint16(len(ntlm)))
	if out, err = c.dispatch(request(cmdSessionSetup, 0, append(body, ntlm...))); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusLogonFailure {
		t.Errorf("an unexpected NTLM message answered %#x", st)
	}

	// The answer to a challenge that was never sent.
	auth := make([]byte, 64)
	copy(auth, ntlmSignature[:])
	binary.LittleEndian.PutUint32(auth[8:], ntlmAuth)
	body = make([]byte, 24)
	binary.LittleEndian.PutUint16(body[12:], uint16(headerLen+24))
	binary.LittleEndian.PutUint16(body[14:], uint16(len(auth)))
	if out, err = c.dispatch(request(cmdSessionSetup, 0, append(body, auth...))); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, out); st != statusLogonFailure {
		t.Errorf("an answer with no challenge behind it answered %#x", st)
	}
	if _, err := c.dispatch(request(cmdSessionSetup, 0, make([]byte, 4))); err == nil {
		t.Error("a body too short to read was accepted")
	}
}

// Two names that differ only by case can exist together on a case-sensitive
// driver. There is no right answer then, so the answer has to be the SAME one
// every time: a client asking twice must not get two different files.
func TestFoldingIsStableWhenTwoNamesDifferOnlyByCase(t *testing.T) {
	fs := &twoCaseFS{}
	first := resolveCase(fs, "/readme")
	for i := 0; i < 20; i++ {
		if got := resolveCase(fs, "/readme"); got != first {
			t.Fatalf("folding gave %q and then %q", first, got)
		}
	}
	if first != "/README" {
		t.Errorf("folding chose %q; the sorted first is /README", first)
	}
	// An exact match wins over any folding.
	if got := resolveCase(fs, "/ReadMe"); got != "/ReadMe" {
		t.Errorf("an exact name was folded to %q", got)
	}
	// Nothing that matches: the path the client asked for is returned, so the
	// error it gets names what it asked about.
	if got := resolveCase(fs, "/nowhere/deep"); got != "/nowhere/deep" {
		t.Errorf("a path that is not there came back as %q", got)
	}
	if got := resolveCase(fs, "/"); got != "/" {
		t.Errorf("the root came back as %q", got)
	}
}

// twoCaseFS holds README and ReadMe side by side, which ext4 permits.
type twoCaseFS struct{}

func (twoCaseFS) Close() error                                { return nil }
func (twoCaseFS) ReadFile(string) ([]byte, error)             { return nil, os.ErrNotExist }
func (twoCaseFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }
func (twoCaseFS) MkDir(string, os.FileMode) error             { return os.ErrPermission }
func (twoCaseFS) DeleteFile(string) error                     { return os.ErrPermission }
func (twoCaseFS) DeleteDir(string) error                      { return os.ErrPermission }
func (twoCaseFS) Rename(string, string) error                 { return os.ErrPermission }
func (twoCaseFS) ReadLink(string) (string, error)             { return "", os.ErrInvalid }

func (twoCaseFS) Stat(p string) (filesystem.Stat, error) {
	switch p {
	case "/":
		return filesystem.NewStat(0o040755, 0, 1), nil
	case "/README", "/ReadMe":
		return filesystem.NewStat(0o100644, 1, 2), nil
	}
	return nil, os.ErrNotExist
}

func (twoCaseFS) ListDir(p string) ([]filesystem.DirEntry, error) {
	if p != "/" {
		return nil, os.ErrNotExist
	}
	// Deliberately not in sorted order: the answer must not depend on the
	// order the driver happens to return.
	return []filesystem.DirEntry{
		filesystem.NewDirEntry(3, "ReadMe", 1),
		filesystem.NewDirEntry(2, "README", 1),
	}, nil
}

// captureConn stands in for the socket, so a test can read what the server
// sent from a goroutine that is not the one it called.
type captureConn struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *captureConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Strip the four-byte length header the framing adds.
	if len(p) > 4 {
		c.frames = append(c.frames, append([]byte(nil), p[4:]...))
	}
	return len(p), nil
}

// await returns the next frame the server sent, waiting for it to arrive.
func (c *captureConn) await(t *testing.T) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.frames) > 0 {
			f := c.frames[0]
			c.frames = c.frames[1:]
			c.mu.Unlock()
			return f
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("nothing was sent within five seconds")
	return nil
}

func (c *captureConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *captureConn) Close() error                     { return nil }
func (c *captureConn) LocalAddr() net.Addr              { return nil }
func (c *captureConn) RemoteAddr() net.Addr             { return nil }
func (c *captureConn) SetDeadline(time.Time) error      { return nil }
func (c *captureConn) SetReadDeadline(time.Time) error  { return nil }
func (c *captureConn) SetWriteDeadline(time.Time) error { return nil }

// A lock of zero length reserves nothing, which is not the same as reserving
// everything -- and two ranges that merely touch do not overlap.
func TestLockRangesThatDoAndDoNotMeet(t *testing.T) {
	held := byteLock{path: "/f", offset: 100, length: 50}
	for _, tc := range []struct {
		name        string
		path        string
		off, length uint64
		want        bool
	}{
		{"the same range", "/f", 100, 50, true},
		{"one byte inside", "/f", 149, 1, true},
		{"the byte after", "/f", 150, 1, false},
		{"the byte before", "/f", 99, 1, false},
		{"a range around it", "/f", 0, 1000, true},
		{"another file", "/g", 100, 50, false},
		{"nothing at all", "/f", 100, 0, false},
	} {
		if got := held.overlaps(tc.path, tc.off, tc.length); got != tc.want {
			t.Errorf("%s: overlaps = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A held range of zero length reserves nothing either.
	empty := byteLock{path: "/f", offset: 100, length: 0}
	if empty.overlaps("/f", 100, 50) {
		t.Error("a zero-length lock reserved something")
	}
}

// A watch covers its own directory, and everything below it only when the
// client asked to watch the tree.
func TestWhatAWatchCovers(t *testing.T) {
	for _, tc := range []struct {
		name string
		x    watcher
		path string
		want bool
	}{
		{"a file in the directory", watcher{dir: "/sub"}, "/sub/f.txt", true},
		{"a file below it", watcher{dir: "/sub"}, "/sub/deep/f.txt", false},
		{"a file below it, watching the tree", watcher{dir: "/sub", tree: true}, "/sub/deep/f.txt", true},
		{"a file elsewhere", watcher{dir: "/sub"}, "/other/f.txt", false},
		{"a file elsewhere, watching the tree", watcher{dir: "/sub", tree: true}, "/other/f.txt", false},
		{"a sibling with a longer name", watcher{dir: "/sub", tree: true}, "/subway/f.txt", false},
		{"the root watching everything", watcher{dir: "/", tree: true}, "/a/b/c.txt", true},
	} {
		if got := tc.x.covers(tc.path); got != tc.want {
			t.Errorf("%s: covers(%q) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}
